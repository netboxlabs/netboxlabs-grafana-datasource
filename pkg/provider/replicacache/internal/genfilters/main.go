// Command genfilters builds filtervocab.json, NetBox's own filter vocabulary:
// for every model with an API viewset, each filter NetBox has on it and what that
// filter means in terms a replica of NetBox's database can act on. Replica-cache
// mode accepts a filter exactly when NetBox has it, with NetBox's meaning, so the
// list is read from NetBox rather than kept by hand.
//
// It reads what introspect.py writes inside a NetBox container, one file per
// NetBox version, oldest version first, and writes model label -> filter name ->
// the distinct meanings that name has across the versions, newest first. A name
// any version has is kept, and where the versions disagree both meanings are, so
// a replica of either NetBox finds its own: the one whose column it stores is
// picked when a query runs. The price is that a filter only a newer NetBox has is
// honoured on a replica of an older one (NetBox 4.4 ignores face__n and returns
// every row). The committed file was generated from NetBox 4.4.10 and 4.6.10;
// regenerate it when the supported range moves:
//
//	docker exec -i <netbox 4.4.10 container> /opt/netbox/venv/bin/python - \
//	  < pkg/provider/replicacache/internal/genfilters/introspect.py > fs-4.4.10.json
//	docker exec -i <netbox 4.6.10 container> /opt/netbox/venv/bin/python - \
//	  < pkg/provider/replicacache/internal/genfilters/introspect.py > fs-4.6.10.json
//	go run ./pkg/provider/replicacache/internal/genfilters \
//	  fs-4.4.10.json fs-4.6.10.json > pkg/provider/replicacache/filtervocab.json
//
// The per-version files are not committed: they are 3 to 4 MB each and only
// restate NetBox. The file is embedded by vocab.go, whose tests pin what the
// rules below produce on the filters that were measured against NetBox.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

// versionDoc is what introspect.py writes for one NetBox version. Only the
// fields the rules read are declared; the rest of the file (the filterset's name
// and path, the field a filter walks, whether a target is a tree) is there for a
// person reading it.
type versionDoc struct {
	Version string `json:"version"`
	Models  map[string]struct {
		Filters []filter `json:"filters"`
	} `json:"models"`
}

// filter is one entry of a filterset's base_filters. Lookup variants such as
// site__n are entries of their own.
type filter struct {
	Name    string `json:"name"`
	Cls     string `json:"cls"`
	Lookup  string `json:"lookup"`
	Exclude bool   `json:"exclude"`
	// Method is the name of the filterset method that computes the filter, when
	// it has one.
	Method  string `json:"method"`
	ToField string `json:"to_field"`
	// NullLabel is the form field's: nil where NetBox offers no null choice
	// (cable-terminations' cable), "None" on every declared related filter.
	NullLabel *string `json:"null_label"`
	// NullValue is the filter's own literal for null ("null" on text and number
	// filters, nil on choice filters that have no null choice).
	NullValue *string `json:"null_value"`
	// NullProbe says that <name>=null is valid and compiles to SQL with IS NULL,
	// measured inside NetBox. Only filters of the classes and lookups that could
	// read null as IS NULL are probed.
	NullProbe bool  `json:"null_probe"`
	Hops      []hop `json:"hops"`
}

// hop is one part of the filter's field_name, walked from the model. Column is
// the database column of a local field or foreign key and is empty for the rest
// (a many-to-many, a reverse or generic relation, a name that is no field).
type hop struct {
	Part   string `json:"part"`
	Kind   string `json:"kind"`
	Column string `json:"column"`
	Target string `json:"target"`
}

// entry is one meaning of a filter, as vocab.go's vocabEntry reads it; the JSON
// tags are the contract between the two.
type entry struct {
	Kind      string `json:"k"`
	Column    string `json:"c,omitempty"`
	Lookup    string `json:"l,omitempty"`
	Exclude   bool   `json:"x,omitempty"`
	ToField   string `json:"t,omitempty"`
	Target    string `json:"target,omitempty"`
	Null      bool   `json:"null,omitempty"`
	NullValue bool   `json:"nv,omitempty"`
	Single    bool   `json:"single,omitempty"`
	Method    string `json:"m,omitempty"`
	Path      string `json:"p,omitempty"`
	Reason    string `json:"r,omitempty"`
}

// vocabulary is model label -> filter name -> meanings, newest version first.
type vocabulary map[string]map[string][]entry

var errUsage = errors.New("usage: genfilters fs.json [fs.json ...]  (one introspect.py output per NetBox version, oldest first)")

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "genfilters:", err)
		os.Exit(1)
	}
}

// run reads the per-version files in order and writes the vocabulary. Nothing is
// written to stdout unless every file read.
func run(paths []string, stdout, stderr io.Writer) error {
	if len(paths) == 0 {
		return errUsage
	}
	out := vocabulary{}
	for _, path := range paths {
		// A developer runs this by hand on the files introspect.py wrote and names
		// them on the command line; it is never part of the plugin, so reading the
		// paths it is given is the point, not a traversal.
		raw, err := os.ReadFile(path) // #nosec G304 G703 -- reads the introspection files named on the command line
		if err != nil {
			return err
		}
		var doc versionDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		// Another generator's input, or an introspection that found nothing, must
		// not become a vocabulary that covers no table.
		if len(doc.Models) == 0 {
			return fmt.Errorf("%s: no models; is it introspect.py's output?", path)
		}
		version, err := build(doc)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		merge(out, version)
		note(stderr, "genfilters: %s: NetBox %s, %d models, %d filters\n", path, doc.Version, len(doc.Models), count(version))
	}
	several := 0
	for _, names := range out {
		for _, alts := range names {
			if len(alts) > 1 {
				several++
			}
		}
	}
	note(stderr, "genfilters: %d models, %d meanings, %d filters with more than one\n", len(out), count(out), several)
	return encode(stdout, out)
}

// note reports progress. A stderr that cannot be written to is no reason not to
// write the vocabulary.
func note(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// count is the number of meanings, not names.
func count(v vocabulary) int {
	n := 0
	for _, names := range v {
		for _, alts := range names {
			n += len(alts)
		}
	}
	return n
}

// build gives every filter of one version its meaning.
func build(doc versionDoc) (vocabulary, error) {
	out := make(vocabulary, len(doc.Models))
	for label, model := range doc.Models {
		names := make(map[string][]entry, len(model.Filters))
		for _, f := range model.Filters {
			if _, dup := names[f.Name]; dup {
				return nil, fmt.Errorf("%s lists the filter %s twice", label, f.Name)
			}
			names[f.Name] = []entry{classify(label, f)}
		}
		out[label] = names
	}
	return out, nil
}

// merge folds a newer version into the older ones already in into.
func merge(into, newer vocabulary) {
	for label, names := range newer {
		if into[label] == nil {
			into[label] = map[string][]entry{}
		}
		for name, alts := range names {
			for _, e := range alts {
				into[label][name] = withMeaning(into[label][name], e)
			}
		}
	}
}

// withMeaning puts e, from a version newer than any meaning in alts, in front. A
// meaning alts already has is replaced by e, so it moves up to where the newest
// version that has it puts it, and takes that version's wording of a reason.
func withMeaning(alts []entry, e entry) []entry {
	out := make([]entry, 0, len(alts)+1)
	out = append(out, e)
	for _, a := range alts {
		if !sameMeaning(a, e) {
			out = append(out, a)
		}
	}
	return out
}

// sameMeaning is true when every field but the wording of the reason agrees:
// two refusals of one filter are one meaning, whichever way each version words
// its reason.
func sameMeaning(a, b entry) bool {
	a.Reason, b.Reason = "", ""
	return a == b
}

// encode writes the vocabulary with its keys in order and one filter to a line,
// so the file diffs by filter and two runs on the same input are the same bytes.
func encode(w io.Writer, v vocabulary) error {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	labels := slices.Sorted(maps.Keys(v))
	for i, label := range labels {
		key, err := compact(label)
		if err != nil {
			return err
		}
		fmt.Fprintf(&buf, " %s: {\n", key)
		names := slices.Sorted(maps.Keys(v[label]))
		for j, name := range names {
			key, err := compact(name)
			if err != nil {
				return err
			}
			alts, err := compact(v[label][name])
			if err != nil {
				return err
			}
			fmt.Fprintf(&buf, "  %s: %s%s\n", key, alts, comma(j, len(names)))
		}
		fmt.Fprintf(&buf, " }%s\n", comma(i, len(labels)))
	}
	buf.WriteString("}\n")
	_, err := w.Write(buf.Bytes())
	return err
}

func comma(i, n int) string {
	if i < n-1 {
		return ","
	}
	return ""
}

// compact is v as JSON on one line, with the arrow in a path left as it is.
func compact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Filter classes by what they do. NetBox's own filters (utilities/filters.py)
// and django-filter's are named as the introspection reports them.
var (
	// refusedClasses are refused whatever they walk, with the reason that says
	// why: the replica stores decimals and timestamps as text (DATA-457,
	// DATA-250), so it cannot compare them as NetBox compares a number or a date;
	// it has no content-type names, and arrays, UUIDs and WWNs have no comparison
	// it can reproduce.
	refusedClasses = map[string]string{
		"MultiValueDecimalFilter":     "decimal",
		"MultiValueDateFilter":        "date",
		"MultiValueDateTimeFilter":    "date",
		"MultiValueTimeFilter":        "date",
		"DateFilter":                  "date",
		"DateTimeFilter":              "date",
		"TimeFilter":                  "date",
		"DateTimeFromToRangeFilter":   "date",
		"DateFromToRangeFilter":       "date",
		"IsoDateTimeFilter":           "date",
		"ContentTypeFilter":           "contenttype",
		"MultiValueContentTypeFilter": "contenttype",
		"NumericArrayFilter":          "array",
		"UUIDFilter":                  "uuid",
		"MultiValueWWNFilter":         "wwn",
	}
	// relationKinds are the filters that compare a foreign key by a field of its
	// target; the tree filter includes the target's descendants and the model
	// choice takes one value.
	relationKinds = map[string]string{
		"ModelMultipleChoiceFilter":    "related",
		"TreeNodeMultipleChoiceFilter": "tree",
		"ModelChoiceFilter":            "modelchoice",
	}
	// valueClasses compare one local column with the filter's lookup.
	valueClasses = map[string]bool{
		"MultiValueCharFilter": true, "CharFilter": true, "MultipleChoiceFilter": true, "ChoiceFilter": true,
		"MultiValueNumberFilter": true, "MultiValueBigNumberFilter": true, "NumberFilter": true,
	}
	// valueLookups are the lookups the replica has an operator for. Any other
	// (family, net_mask_length, contains, range_contains, ...) compares something
	// a replica cannot reproduce.
	valueLookups = map[string]bool{
		"exact": true, "iexact": true, "icontains": true, "istartswith": true, "iendswith": true,
		"regex": true, "iregex": true, "gt": true, "gte": true, "lt": true, "lte": true,
	}
	// nullClasses are the classes whose form takes the text "null" and whose
	// filter turns it into None: django-filter's MultipleChoiceFilter does, with
	// its null_value. The number filters' forms refuse the text (mtu=null is a
	// 400).
	nullClasses = map[string]bool{
		"MultiValueCharFilter": true, "MultiValueMACAddressFilter": true, "ChoiceFilter": true, "MultipleChoiceFilter": true,
	}
	// singleClasses read one value: django-filter's form fields for them take the
	// last of a repeated parameter, where the multi-valued ones take all.
	singleClasses = map[string]bool{
		"CharFilter": true, "ChoiceFilter": true, "NumberFilter": true, "BooleanFilter": true, "ModelChoiceFilter": true,
	}
)

// classify is the whole meaning of one filter of one model.
func classify(model string, f filter) entry {
	e := kindOf(model, f)
	e.Single = singleClasses[f.Cls]
	// A refusal is a refusal whichever way the filter would have excluded.
	if f.Exclude && e.Kind != "refuse" {
		e.Exclude = true
	}
	return e
}

// kindOf applies the rules in order; the first that matches decides.
func kindOf(model string, f filter) entry {
	// 1. A method computes the filter, whatever its class says. Four are ones
	// the provider reproduces: the address match (NetBox's net_in), the prefix
	// match, a prefix's containment search and the local-config-context flag.
	// The same search_contains on IP ranges is not one of them, nor are the
	// others (q, ip-addresses' device, ...), which stay what they are.
	if f.Method != "" {
		column := ownColumn(f)
		switch {
		case f.Method == "filter_address":
			return entry{Kind: "address", Column: column}
		case f.Method == "filter_prefix":
			return entry{Kind: "prefix", Column: column}
		case model == "ipam.prefix" && f.Method == "search_contains":
			// Its field_name is its own name, which is no column; the method
			// searches the prefix column.
			return entry{Kind: "contains", Column: "prefix"}
		case f.Method == "_local_context_data":
			return entry{Kind: "localcontext", Column: column}
		}
		return entry{Kind: "method", Method: f.Method}
	}

	// 2. Refused by class. First the filters named <x>__empty whose class is not
	// a boolean: NetBox gives a declared number, date or array filter an __empty
	// name but keeps its class and sets its lookup to isnull, so "true" fails the
	// filter's own form (400): speed__empty "Enter a whole number.",
	// created__empty "Enter a valid date/time.". Those whose class is a boolean
	// (mtu__empty) work and are rule 4's.
	if strings.HasSuffix(f.Name, "__empty") && f.Cls != "BooleanFilter" {
		return refuse("badempty")
	}
	if reason, ok := refusedClasses[f.Cls]; ok {
		return refuse(reason)
	}

	// 3. A foreign key compared by a field of its target: the filter walks the
	// key alone (site_id), or the key and then the field its to_field names
	// (site__slug). Anything longer goes through other relations, which is rule
	// 6's. The column is the key's own (_site -> _site_id), and a null choice is
	// there when the form field has a null label.
	if kind, ok := relationKinds[f.Cls]; ok && len(f.Hops) > 0 {
		first, rest := f.Hops[0], f.Hops[1:]
		if (first.Kind == "fk" || first.Kind == "o2o") && (len(rest) == 0 || (len(rest) == 1 && f.ToField != "" && rest[0].Part == f.ToField)) {
			to := f.ToField
			if to == "" {
				to = "id"
			}
			return entry{Kind: kind, Column: first.Column, ToField: to, Target: first.Target, Null: f.NullLabel != nil}
		}
	}

	// 4. A boolean on one field: NetBox's empty lookup, IS [NOT] NULL, or an
	// equality on a stored boolean.
	if f.Cls == "BooleanFilter" && len(f.Hops) == 1 {
		hop := f.Hops[0]
		switch {
		case f.Lookup == "empty":
			return entry{Kind: "empty", Column: hop.Column}
		case f.Lookup == "isnull":
			return entry{Kind: "isnull", Column: hop.Column}
		case f.Lookup == "exact" && hop.Kind == "local":
			return entry{Kind: "bool", Column: hop.Column}
		}
	}

	// 5. One local column, text, choice, number or MAC address.
	if len(f.Hops) == 1 && f.Hops[0].Kind == "local" && (valueClasses[f.Cls] || f.Cls == "MultiValueMACAddressFilter") {
		if !valueLookups[f.Lookup] {
			return refuse("lookup:" + f.Lookup)
		}
		kind := "value"
		if f.Cls == "MultiValueMACAddressFilter" {
			kind = "mac"
		}
		return entry{Kind: kind, Column: f.Hops[0].Column, Lookup: f.Lookup, NullValue: readsNull(f)}
	}

	// 6. Through more than one relation, or through a relation this table holds
	// no column for (a many-to-many, a reverse or a generic one): a join.
	if len(f.Hops) > 1 || (len(f.Hops) == 1 && slices.Contains([]string{"m2m", "reverse", "generic"}, f.Hops[0].Kind)) {
		parts := make([]string, len(f.Hops))
		for i, h := range f.Hops {
			parts[i] = h.Part
		}
		return entry{Kind: "path", Path: strings.Join(parts, " → ")}
	}

	// 7. Nothing above: a field the model does not have, a class this does not
	// know.
	return refuse("other")
}

func refuse(reason string) entry {
	return entry{Kind: "refuse", Reason: reason}
}

// ownColumn is the column of a filter that walks one local field of the model,
// and none when it walks anything else.
func ownColumn(f filter) string {
	if len(f.Hops) > 0 && f.Hops[0].Kind == "local" {
		return f.Hops[0].Column
	}
	return ""
}

// readsNull says that the literal null means IS NULL on a text, choice or MAC
// filter: its lookup is an equality (Django reads exact=None and iexact=None as
// IS NULL, and raises on None for the others), the filter's null_value is "null",
// its class passes the text on, and compiling <name>=null inside NetBox gave an
// IS NULL. The compile is what leaves out sites' time_zone and its negation,
// which NetBox compares as the empty string, without naming them.
func readsNull(f filter) bool {
	return nullClasses[f.Cls] && (f.Lookup == "exact" || f.Lookup == "iexact") &&
		f.NullValue != nil && *f.NullValue == "null" && f.NullProbe
}
