import { DataSourceJsonData, SelectableValue } from '@grafana/data';
import { DataQuery } from '@grafana/schema';

/** Enrichment backend. Only 'netbox' exists today; the type and the `mode`
 * option are kept as the seam for a planned second, high-volume backend
 * (no UI selector until then). */
export type ProviderMode = 'netbox';

/** A single field/operator/value constraint applied to a query. */
export interface FilterRow {
  field: string;
  operator: string; // NetBox lookup: '' (exact), 'ic', 'n', 'gte', 'lte', ...
  value: string;
}

export type QueryType = 'objects' | 'ip-enrichment' | 'topology' | 'annotations';

/** One source→output join-key mapping with an optional value transform. */
export interface JoinKeyMapping {
  source: string;
  output: string;
  transform: string; // none|lower|upper|host|iphost|ifshort|regex
  regex?: string;
  replace?: string;
}

export interface NetBoxQuery extends DataQuery {
  /** Determines the result shape — see QueryType. Defaults to 'objects'. */
  queryType?: QueryType;
  /** Object type path, e.g. 'dcim/devices' or 'plugins/bgp/bgp-sessions'. */
  objectType?: string;
  /** Optional netbox-branching schema id (or a $variable resolving to one).
   * When set, the query targets that branch; empty targets main. Requires the
   * netbox-branching plugin on the NetBox side. */
  branch?: string;
  filters?: FilterRow[];
  /** Optional subset (and order) of columns to return. */
  fields?: string[];
  /** For objects queries: return a single numeric count of matching objects
   * instead of a table. Required to alert on object counts — Grafana alert
   * expressions evaluate a number, not a table. */
  count?: boolean;
  /** For objects queries: reshape the result for Grafana alerting — string
   * label columns plus one numeric `value` column, one alert instance per
   * row. Mutually exclusive with `count` (the editor enforces it). */
  alertTable?: boolean;
  /** Column supplying the numeric value in alert-table mode (e.g.
   * 'utilization'). Empty = constant 1 per row. */
  valueField?: string;
  limit?: number;
  /** Derived key columns so the result lines up with metric labels. */
  joinKeys?: JoinKeyMapping[];
  /** For annotation queries: restrict to NetBox content types, e.g. 'dcim.device'. */
  objectTypes?: string[];
  /** For ip-enrichment: IPs (comma/space/newline separated; supports $variables). */
  ips?: string;
  /** For ip-enrichment: which context columns to return — prefix, address,
   * interface and device (see IP_CONTEXT_FIELD_GROUPS), not prefix alone. */
  contextFields?: string[];
  /** For topology: drop devices with no inter-device cable (default true). */
  connectedOnly?: boolean;
  /** For topology: edge derivation — 'logical' (NetBox cable paths; panels and
   * circuits resolve to the far device) or 'physical' (raw cables; panels
   * appear as nodes). Default 'logical'. */
  connections?: 'logical' | 'physical';
}

export const DEFAULT_QUERY: Partial<NetBoxQuery> = {
  queryType: 'objects',
  filters: [],
  limit: 100,
};

/** Variable query — populates a dashboard variable from NetBox. */
export interface NetBoxVariableQuery extends DataQuery {
  objectType?: string;
  valueField?: string;
  textField?: string;
  filters?: FilterRow[];
  /** Optional netbox-branching schema id (or $variable) to scope the variable's options. */
  branch?: string;
}

export interface NetBoxDataSourceOptions extends DataSourceJsonData {
  url?: string;
  /** Browser-facing NetBox base URL, if different from url (e.g. Grafana
   * reaches NetBox via internal service DNS). Used to rewrite deep links. */
  publicUrl?: string;
  mode?: ProviderMode;
  tlsSkipVerify?: boolean;
  timeoutSeconds?: number;
}

/** Secret values — never returned to the frontend after being set. */
export interface NetBoxSecureJsonData {
  apiToken?: string;
}

export interface ObjectTypeOption {
  value: string;
  label: string;
  app: string;
  model: string;
}

export interface FieldOption {
  name: string;
  type: string;
}

/** A valid filter parameter for an object type and the operators NetBox supports on it. */
export interface FilterField {
  name: string;
  operators: string[];
}

/** Join-key value transforms surfaced in the query editor. */
export const JOIN_KEY_TRANSFORMS: Array<{ label: string; value: string; description?: string }> = [
  { label: 'none', value: 'none', description: 'Use the value as-is' },
  { label: 'lowercase', value: 'lower' },
  { label: 'UPPERCASE', value: 'upper' },
  { label: 'strip domain', value: 'host', description: 'leaf1.dc.com → leaf1' },
  { label: 'IP host', value: 'iphost', description: 'strip CIDR mask: 10.0.0.1/24 → 10.0.0.1' },
  { label: 'interface short name', value: 'ifshort', description: 'GigabitEthernet0/1 → Gi0/1 (netutils standard)' },
  { label: 'regex', value: 'regex', description: 'extract/replace with a regular expression' },
];

/** Context fields for the IP enrichment query, grouped by source object.
 *  Mirrors IPEnrichColumns() in pkg/provider/netbox/ipenrich.go exactly — the
 *  picker is a closed vocabulary, so any name offered here that the backend
 *  cannot produce renders a permanently blank column. Keep the two in sync.
 *  `ip` and `match_count` are not namespaced: `ip` is the documented Grafana
 *  join key (docs/RECIPES.md) and `match_count` describes the row itself.
 *  There is no `prefix_site` — NetBox exposes a prefix's site under `scope`.
 *  Address has no `*_dns` NAT fields: NetBox 4.4's nested IP serializer has
 *  no dns_name property, so nat_inside/nat_outside expose only the peer
 *  address, not a hostname. Interface columns stop at name/description: the
 *  embedded assigned_object is the brief serializer and carries nothing else. */
export const IP_CONTEXT_FIELD_GROUPS: Array<{
  label: string;
  options: Array<{ label: string; value: string }>;
}> = [
  {
    label: 'Identity',
    options: [
      { label: 'ip', value: 'ip' },
      { label: 'match_count', value: 'match_count' },
    ],
  },
  {
    label: 'Prefix',
    options: ['cidr', 'scope', 'tenant', 'role', 'vrf', 'vlan', 'description'].map((f) => ({
      label: `prefix_${f}`,
      value: `prefix_${f}`,
    })),
  },
  {
    label: 'Address',
    options: ['dns_name', 'status', 'role', 'vrf', 'tenant', 'description', 'nat_inside', 'nat_outside'].map((f) => ({
      label: `address_${f}`,
      value: `address_${f}`,
    })),
  },
  {
    label: 'Interface',
    options: ['name', 'description'].map((f) => ({ label: `interface_${f}`, value: `interface_${f}` })),
  },
  {
    label: 'Device',
    options: [
      'name',
      'role',
      'platform',
      'device_type',
      'site',
      'location',
      'rack',
      'tenant',
      'status',
      'is_primary_ip',
    ].map((f) => ({ label: `device_${f}`, value: `device_${f}` })),
  },
];

/** Every context field as a flat list, labelled by its full column name.
 *
 *  Labels are namespaced rather than group-local ("device_name", not "name")
 *  because @grafana/ui resolves a MultiSelect's selected chips against the
 *  option list: a bare label made the default selection render two adjacent
 *  chips both reading "name" (device_name and interface_name), and a panel
 *  asking for nine columns read "match_count cidr scope tenant role vlan name
 *  is_primary_ip name". The group headings still carry the namespace, so
 *  nothing is lost by repeating it.
 *
 *  This flat form is also what the join-key "source field" picker offers for an
 *  IP-enrichment query. Those columns are a closed vocabulary this plugin
 *  produces, NOT the fields of any NetBox object type, so populating that picker
 *  from `resources/fields?type=ipam/prefixes` offered 29 names the frame never
 *  contains — `scope` was suggested and yielded an empty key, while the two that
 *  work, `prefix_scope` and `device_name`, were not offered at all. */
export const IP_CONTEXT_FIELD_OPTIONS: Array<{ label: string; value: string }> = IP_CONTEXT_FIELD_GROUPS.flatMap(
  (g) => g.options
);

/** What a new IP-enrichment query selects. match_count is included so an
 *  ambiguous pick (anycast, VRRP VIPs) is visible out of the box. */
export const DEFAULT_IP_CONTEXT_FIELDS: string[] = [
  'ip',
  'match_count',
  'address_dns_name',
  'device_name',
  'interface_name',
  'device_is_primary_ip',
  'device_site',
  'device_tenant',
];

/** NetBox filter lookup operators surfaced in the query editor. */
// NetBox lookup operators. `value` is the lookup suffix sent to the API
// (empty = exact); the backend maps `field__<value>`. Keep this in sync with
// suffixToken/operatorOrder in pkg/provider/netbox/schema.go — the query editor
// only offers operators present in BOTH this list and the object type's schema.
export const FILTER_OPERATORS: Array<{ label: string; value: string }> = [
  { label: '=', value: '' },
  { label: 'not', value: 'n' },
  { label: '= (ci)', value: 'ie' },
  { label: 'not (ci)', value: 'nie' },
  { label: 'contains', value: 'ic' },
  { label: 'not contains', value: 'nic' },
  { label: 'starts with', value: 'isw' },
  { label: 'not starts with', value: 'nisw' },
  { label: 'ends with', value: 'iew' },
  { label: 'not ends with', value: 'niew' },
  { label: 'regex', value: 'regex' },
  { label: 'regex (ci)', value: 'iregex' },
  { label: '>=', value: 'gte' },
  { label: '<=', value: 'lte' },
  { label: '>', value: 'gt' },
  { label: '<', value: 'lt' },
  { label: 'is empty', value: 'empty' },
  { label: 'has any value', value: 'nempty' },
];

/**
 * Operators that ask about presence rather than value: they take no value and
 * both map to the same NetBox param (`<field>__empty=true|false`).
 */
export const EMPTY_FAMILY_OPERATORS = ['empty', 'nempty'];

/**
 * NetBox filters whose repeated params are AND-ed rather than OR-ed
 * (`TagFilter`/`TagIDFilter` set `conjoined=True`). Stacking two such rows
 * already means what the UI implies, so they must NOT be flagged as colliding.
 * Verified against NetBox 4.4.10: with one device tagged [crit, prod] and
 * another tagged [crit], both `?tag=crit&tag=prod` and `?tag_id=1&tag_id=2`
 * return only the device carrying both.
 */
export const CONJOINED_WIRE_KEYS = ['tag', 'tag_id'];

/**
 * Operators whose repeated params NetBox genuinely combines with OR — the only
 * case where stacked rows mean something different from the AND the UI implies,
 * and therefore the only case worth warning about.
 *
 * This is deliberately an allowlist rather than a list of exemptions. Repeated
 * *negated* params are excluded as a group — `qs.exclude(Q(a) | Q(b))`, i.e.
 * NOT a AND NOT b — which is exactly the stacked AND, so warning there would
 * state the inverse of the truth. (Verified: `?site__n=ams1&site__n=nyc1`
 * returns only the device in neither site.) An allowlist fails safe: a new
 * operator that nobody adds here loses a warning, whereas a missed exemption
 * would actively mislead.
 *
 * Caveat this list cannot express: whether repeated params OR is a property of
 * the NetBox filter *class*, not the lookup suffix. `MultiValue*Filter` /
 * `ModelMultipleChoiceFilter` OR; plain single-value filters read
 * `QueryDict.get`, so the LAST value wins. Verified on NetBox 4.4.10:
 * `?q=AMS1&q=NYC1` and `?q=NYC1&q=AMS1` return different sets (order matters =
 * last wins), while `?name__ic=AMS1&name__ic=NYC1` returns the same union in
 * either order. Both behaviours still differ from the stacked AND, so the
 * warning is warranted either way — which is why the message wording avoids
 * promising OR outright rather than plumbing a filter-class discriminator
 * through provider.FilterField (that belongs with Phase 2's param-kind ladder).
 *
 * Includes the legacy `'exact'` token alongside `''`: the backend treats them
 * byte-identically (`f.Operator != "" && f.Operator != "exact"` in
 * buildFilterValues — same wire key, same `q.Add`), so repeated params OR
 * exactly as they do for `''`. It looks like a redundant duplicate of `''`
 * because the query editor itself never emits `'exact'` (FILTER_OPERATORS has
 * no such entry); it only reaches here via a provisioned or hand-edited
 * dashboard. Do not "clean up" this entry — removing it silently drops the
 * warning for that case (see filterWireKey, which maps 'exact' to the same
 * key as '').
 */
export const OR_COMBINING_OPERATORS = [
  '',
  'exact',
  'ie',
  'ic',
  'isw',
  'iew',
  'regex',
  'iregex',
  'gt',
  'gte',
  'lt',
  'lte',
];

type OpOption = { label: string; value: string };

/** Filter-field dropdown options: schema filter fields when available, else the
 * fallback column options (schema unavailable). */
export function filterFieldOptionsFrom(
  filterFields: FilterField[],
  fallback: Array<SelectableValue<string>>
): Array<SelectableValue<string>> {
  return filterFields.length > 0 ? filterFields.map((f) => ({ label: f.name, value: f.name })) : fallback;
}

/** Operators offered for a filter field. Schema-restricted when the field is
 * known; falls back to all operators when the schema is unavailable
 * (filterFields empty) or the field isn't in the schema (legacy/custom), so
 * saved queries are never blocked. A stored operator not in the allowed set is
 * appended so it stays visible/editable. */
export function filterOperatorsFor(filterFields: FilterField[], field: string, storedOperator: string): OpOption[] {
  const ff = filterFields.find((f) => f.name === field);
  let ops: OpOption[] =
    filterFields.length === 0 || !ff
      ? FILTER_OPERATORS
      : FILTER_OPERATORS.filter((o) => ff.operators.includes(o.value));
  if (!ops.some((o) => o.value === storedOperator)) {
    ops = [
      ...ops,
      FILTER_OPERATORS.find((o) => o.value === storedOperator) ?? {
        label: storedOperator || '=',
        value: storedOperator,
      },
    ];
  }
  return ops;
}

/** Whether an operator is valid for a filter field per the schema. Unknown
 * fields and the empty-schema fallback are permissive (true) so nothing is
 * blocked; a schema-known field validates against its operator set. Used to
 * reset a stale operator when the user switches a filter's field. */
export function isOperatorValidForField(filterFields: FilterField[], field: string, operator: string): boolean {
  const ff = filterFields.find((f) => f.name === field);
  return !ff || ff.operators.includes(operator);
}

/**
 * The NetBox query-param key a filter row will produce. Mirrors
 * buildFilterValues in pkg/provider/netbox/enrich.go — keep the two in sync.
 * Two rows sharing a key are OR-ed by NetBox even though the stacked UI reads
 * as AND, which is what validateFilters warns about.
 */
export function filterWireKey(row: FilterRow): string {
  // A missing `operator` (a provisioned/hand-edited row can omit it even
  // though the type is non-optional) means exact match, identical to '' —
  // that is how both the editor default and buildFilterValues treat it.
  // Coerced explicitly rather than relied on via truthiness so this can't be
  // "simplified" back into a check that silently mishandles the next
  // absent-value variant (e.g. null).
  const operator = row.operator ?? '';
  if (EMPTY_FAMILY_OPERATORS.includes(operator)) {
    return `${row.field}__empty`;
  }
  // Mirrors the backend's legacy branch: `f.Operator != "" && f.Operator != "exact"`
  // treats the literal string "exact" the same as "" (both mean the bare field).
  // Unreachable from the editor (FILTER_OPERATORS never emits "exact"), but a
  // provisioned/hand-edited dashboard could use it.
  if (!operator || operator === 'exact') {
    return row.field;
  }
  return `${row.field}__${operator}`;
}

/**
 * Something worth telling the user about one filter row, reported against its
 * index. Severity is a ladder, not decoration:
 *   - 'info'    — status the user may not have finished acting on yet (a row
 *                 with no value). Must not read as a scolding: picking a field
 *                 before typing a value is the normal authoring flow, and
 *                 plenty of users are just browsing and never filter at all.
 *   - 'warning' — the user probably did not mean this (two rows NetBox will
 *                 combine differently from the AND the stacked UI implies).
 */
export interface FilterIssue {
  index: number;
  severity: 'info' | 'warning';
  message: string;
}

/**
 * A row's operator, normalised. `operator` is non-optional in the `FilterRow`
 * type, but a provisioned or hand-edited dashboard's JSON can omit it, making
 * it `undefined` at runtime. A missing operator means exact match, identical
 * to `''` — that is how both the editor default and buildFilterValues treat
 * it (`f.Operator != "" && f.Operator != "exact"` in
 * pkg/provider/netbox/enrich.go). Coerced once, here, rather than added as
 * another entry to EMPTY_FAMILY_OPERATORS/OR_COMBINING_OPERATORS: enumerating
 * `undefined` alongside `''` only patches this one shape, and the next
 * absent-value variant (`null`, say) would fail the same way `'exact'` did in
 * an earlier round — the collision found, then the warning silently dropped
 * because the raw value wasn't in the allowlist. Every read of `.operator` in
 * this file goes through this function so they can't drift apart.
 */
function op(f: FilterRow): string {
  return f.operator ?? '';
}

/**
 * Whether a row will emit a NetBox param at all. Mirrors buildFilterValues in
 * pkg/provider/netbox/enrich.go exactly: empty-family operators always emit
 * `__empty` regardless of value; everything else splits `value` on `,` (CSV,
 * for multi-value variables) and emits only if at least one segment survives
 * a trim. `f.value ?? ''` because a provisioned/saved row can omit `value`
 * entirely; `f.value.split` would throw and blank the whole editor (mirrors
 * the defensive interpolation at src/datasource.ts:73). A value made only of
 * separators — `','`, `',,'`, `' , '` — reads as populated by naive
 * `.trim()` but the backend's per-segment split drops every segment, so it
 * must count as NOT emitting here too. Used by both validation passes below
 * so they can't drift apart on what "blank" means.
 */
function emitsParam(f: FilterRow): boolean {
  return EMPTY_FAMILY_OPERATORS.includes(op(f)) || (f.value ?? '').split(',').some((v) => v.trim() !== '');
}

/**
 * Validates filter rows against the two ways they silently misbehave today:
 * a row with a field but no value is dropped by the backend (returning
 * unfiltered data), and two rows resolving to the same NetBox param are OR-ed
 * rather than AND-ed. A wholly blank row is ignored — the user is still typing.
 */
export function validateFilters(filters: FilterRow[]): FilterIssue[] {
  const issues: FilterIssue[] = [];

  filters.forEach((f, index) => {
    if (!f.field) {
      return; // nothing chosen yet
    }
    if (!emitsParam(f)) {
      issues.push({
        index,
        severity: 'info',
        message: "This filter has no value, so it isn't applied.",
      });
    }
  });

  const byKey = new Map<string, number[]>();
  filters.forEach((f, index) => {
    if (!f.field) {
      return;
    }
    // A row that will not emit a param can't collide with anything. Without
    // this, a row already flagged 'info' by the pass above would ALSO be
    // flagged as OR-colliding with a populated row on the same field —
    // self-contradictory, and wrong on the wire.
    if (!emitsParam(f)) {
      return;
    }
    const key = filterWireKey(f);
    byKey.set(key, [...(byKey.get(key) ?? []), index]);
  });
  for (const [key, indexes] of byKey) {
    if (indexes.length < 2) {
      continue;
    }
    if (CONJOINED_WIRE_KEYS.includes(key)) {
      // Repeated tag params are AND-ed by NetBox, so stacked tag rows already
      // mean what the UI implies. Warning would state the opposite of the truth
      // and push the user to delete a correct row.
      continue;
    }

    let message: string;
    if (indexes.every((i) => EMPTY_FAMILY_OPERATORS.includes(op(filters[i])))) {
      // buildFilterValues uses Set for __empty (a boolean param), so the LAST
      // row wins rather than the rows combining.
      message = `Another filter also asks whether ${filters[indexes[0]].field} is empty — only the last one is applied.`;
    } else if (indexes.every((i) => OR_COMBINING_OPERATORS.includes(op(filters[i])))) {
      // Deliberately does not promise OR outright: whether repeated params OR is
      // a property of the NetBox filter *class*, not the lookup suffix, and this
      // allowlist can only see the suffix. Multi-value filters OR; single-value
      // ones (q, contains, has_primary_ip) keep only the last value. Both differ
      // from the AND the stacked UI implies, which is the actionable point.
      message =
        `Another filter uses ${key} too — NetBox does not AND repeated parameters: ` +
        `it usually combines them with OR, and for single-value parameters such as q only the last value applies.`;
    } else {
      // Negated operators (and anything not known to OR): NetBox excludes the
      // values as a group, which already equals the stacked AND. Say nothing
      // rather than guess.
      continue;
    }

    for (const index of indexes) {
      issues.push({ index, severity: 'warning', message });
    }
  }

  return issues;
}
