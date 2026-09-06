import React, { useEffect, useMemo, useState } from 'react';
import { css } from '@emotion/css';
import {
  Alert,
  InlineField,
  Input,
  Select,
  MultiSelect,
  Stack,
  Button,
  IconButton,
  TextArea,
  InlineSwitch,
  RadioButtonGroup,
  useStyles2,
} from '@grafana/ui';
import { GrafanaTheme2, QueryEditorProps, SelectableValue } from '@grafana/data';
import { getTemplateSrv } from '@grafana/runtime';
import { DataSource } from '../datasource';
import {
  useBranchingInstalled,
  BRANCH_FIELD_TOOLTIP,
  BRANCH_FIELD_DISABLED_TOOLTIP,
} from '../hooks/useBranchingInstalled';
import {
  EMPTY_FAMILY_OPERATORS,
  FilterField,
  filterFieldOptionsFrom,
  filterOperatorsFor,
  isOperatorValidForField,
  FilterRow,
  IP_CONTEXT_FIELD_GROUPS,
  IP_CONTEXT_FIELD_OPTIONS,
  DEFAULT_IP_CONTEXT_FIELDS,
  JOIN_KEY_TRANSFORMS,
  JoinKeyMapping,
  NetBoxDataSourceOptions,
  NetBoxQuery,
  ObjectTypeOption,
  QueryType,
  composeOrdering,
  orderingField,
  orderingFieldsFor,
  orderingIsDescending,
  validateFilters,
} from '../types';

type Props = QueryEditorProps<DataSource, NetBoxQuery, NetBoxDataSourceOptions>;

/** Copy for the Sort by picker.
 *
 *  A constant so it can be asserted directly: Grafana mounts a tooltip's text
 *  only on hover, which jsdom does not reproduce, so a DOM-based test of this
 *  wording tests the tooltip library rather than the copy (same reason as
 *  FAST_PAGING_TOOLTIP, and this one is deliberately shorter than that — a
 *  paragraph in this editor breaks the layout).
 *
 *  It has to carry the cost. Sorting reads as free — every table sorts — and
 *  here it is not: the same picker that costs 0.3s on 4030 sites costs 27.6s on
 *  6.8M devices, once per panel refresh, and nothing downstream will say so. */
export const ORDERING_TOOLTIP = [
  'Ask NetBox to sort before the row limit applies, so a limited panel shows the first rows by this field',
  'rather than an arbitrary set. Only the fields NetBox is known to sort are offered.',
  'Not free at scale: on 6.8M devices this measured 2.3s to 27.6s depending on the field, against 0.9s unsorted.',
].join(' ');

/** Copy for the Sort by picker while it is disabled. Names the setting to
 *  change, because nothing else on this screen explains the control being dead —
 *  the setting lives on the data source, not the panel.
 *
 *  Scoped to THIS query, not to the data source: fast paging being on is not by
 *  itself a reason NetBox will not sort. The refusal comes from the cursor walk,
 *  and only a query that takes it is affected — an alert-table query on this same
 *  data source sorts, and its picker is live (see orderingDisabled). Saying "this
 *  data source cannot sort" would be visibly false one control away. */
export const ORDERING_DISABLED_TOOLTIP = [
  'This query pages by ID (Fast paging, in the data source settings),',
  'and NetBox refuses a sort on an ID-paged walk.',
  'Turn fast paging off to sort here, or sort the table in the panel to order the rows it was given.',
].join(' ');

/** Sort directions. Lowercase labels match the join-key transform picker. */
const ORDERING_DIRECTIONS: Array<SelectableValue<string>> = [
  { label: 'ascending', value: 'asc' },
  { label: 'descending', value: 'desc' },
];

const QUERY_TYPES: Array<SelectableValue<QueryType>> = [
  { label: 'Objects', value: 'objects', description: 'Query a NetBox object type as a joinable table' },
  {
    label: 'IP enrichment',
    value: 'ip-enrichment',
    description: 'Resolve IPs to their NetBox device, interface and address record (longest-prefix fallback)',
  },
  { label: 'Topology', value: 'topology', description: 'Devices + cables as a node graph' },
  {
    label: 'Topology edges',
    value: 'topology-edges',
    description:
      'The same links as joinable rows (device, peer, roles) — for alert rules that ask whether a neighbour is also down',
  },
];

/** Both topology shapes run the same traversal and take the same controls; only
 *  the output differs (node graph vs joinable rows). */
const isTopologyQuery = (t?: QueryType) => t === 'topology' || t === 'topology-edges';

/** Theme-derived styles, so the editor follows Grafana's colours and type scale
 *  in light and dark rather than fixed inline values. */
const getStyles = (theme: GrafanaTheme2) => ({
  hint: css({
    color: theme.colors.text.secondary,
    fontSize: theme.typography.bodySmall.fontSize,
    marginLeft: theme.spacing(0.5),
  }),
});

export function QueryEditor({ query, onChange, onRunQuery, datasource }: Props) {
  const styles = useStyles2(getStyles);
  const queryType: QueryType = query.queryType ?? 'objects';
  const branchingInstalled = useBranchingInstalled(datasource);
  const branchDisabled = branchingInstalled === false;
  const [objectTypes, setObjectTypes] = useState<ObjectTypeOption[]>([]);
  const [fields, setFields] = useState<string[]>([]);
  const [filterFields, setFilterFields] = useState<FilterField[]>([]);
  const [loadingTypes, setLoadingTypes] = useState(true);

  useEffect(() => {
    let active = true;
    datasource
      .getObjectTypes()
      .then((t) => active && setObjectTypes(t))
      .catch(() => active && setObjectTypes([]))
      .finally(() => active && setLoadingTypes(false));
    return () => {
      active = false;
    };
  }, [datasource]);

  // The object type used for field discovery depends on the query type.
  //
  // ip-enrichment deliberately discovers nothing. Its result columns are this
  // plugin's own closed vocabulary (IP_CONTEXT_FIELD_OPTIONS), not any NetBox
  // object type's fields, and the only consumer of `fields` for this query type
  // is the join-key picker below. Pointing it at ipam/prefixes filled that picker
  // with 29 names the frame never contains — every suggestion silently derived an
  // empty join key — so there is nothing to fetch here.
  const fieldType =
    queryType === 'ip-enrichment' ? undefined : isTopologyQuery(queryType) ? 'dcim/devices' : query.objectType;
  const isScope = queryType === 'ip-enrichment' && query.ipSource === 'scope';
  // Filter rows are bound to a NetBox object type. For a scope-sourced
  // ip-enrichment query that is ipam/ip-addresses, even though the result's own
  // columns (fieldType) are still this plugin's closed vocabulary.
  const filterType = isScope ? 'ipam/ip-addresses' : fieldType;
  const showsFilters = queryType === 'objects' || isTopologyQuery(queryType) || isScope;

  useEffect(() => {
    let active = true;
    const branch = query.branch ? getTemplateSrv().replace(query.branch) : undefined;
    const p = fieldType ? datasource.getFields(fieldType, branch) : Promise.resolve<Array<{ name: string }>>([]);
    p.then((f) => active && setFields(f.map((x) => x.name))).catch(() => active && setFields([]));
    return () => {
      active = false;
    };
  }, [datasource, fieldType, query.branch]);

  useEffect(() => {
    let active = true;
    const branch = query.branch ? getTemplateSrv().replace(query.branch) : undefined;
    const p = filterType ? datasource.getFilterFields(filterType, branch) : Promise.resolve<FilterField[]>([]);
    p.then((ff) => active && setFilterFields(ff)).catch(() => active && setFilterFields([]));
    return () => {
      active = false;
    };
  }, [datasource, filterType, query.branch]);

  const typeOptions: Array<SelectableValue<string>> = useMemo(
    () => objectTypes.map((t) => ({ label: t.label, value: t.value, description: t.value })),
    [objectTypes]
  );
  const fieldOptions: Array<SelectableValue<string>> = useMemo(
    () => fields.map((f) => ({ label: f, value: f })),
    [fields]
  );

  const filterFieldOptions = filterFieldOptionsFrom(filterFields, fieldOptions);
  const schemaMode = filterFields.length > 0;

  const settings = datasource.datasourceInstanceSettings?.jsonData;
  const cacheMode = settings?.mode === 'replica-cache';

  // Read back through the same helpers the writer uses, so a stored '-name'
  // (or a provisioned ' -name ') shows as its field plus a direction.
  const sortField = orderingField(query.ordering);
  const sortDescending = orderingIsDescending(query.ordering);

  // Sorting is a closed vocabulary in NetBox mode, unlike every other picker on
  // this form. The rest are built from the object type's schema; that one
  // CANNOT be, because an unknown ordering field is not harmlessly ignored by
  // NetBox — some raise a 500 that ends the query, and dcim/sites'
  // `device_count` is a schema column that does exactly that once the
  // `?fields=` projection is in play. So the options are the measured
  // allow-list for THIS object type and nothing else, and a type with no entry
  // gets no control rather than an empty dropdown.
  //
  // None of that reasoning is about replica-cache. There the sortable set is
  // the STORED columns, which the backend already publishes as the filterable
  // set — filterFieldsFor restricts it to exactly the raw columns — so the
  // options come from the schema like every other picker. The allow-list was
  // wrong for it in both directions: dcim/racks and every plugin model have no
  // entry and so got no sort control at all, while dcim/devices offered site
  // and role, which are derived and which the backend drops with a note.
  //
  // A sort already stored is kept in the list even when the schema has not
  // arrived yet, or no longer has that column, so the control does not vanish
  // from under a saved panel.
  const orderingOptions: Array<SelectableValue<string>> = useMemo(() => {
    const names = cacheMode ? filterFields.map((f) => f.name) : orderingFieldsFor(query.objectType);
    if (cacheMode && sortField && !names.includes(sortField)) {
      names.push(sortField);
      names.sort();
    }
    return names.map((f) => ({ label: f, value: f }));
  }, [cacheMode, filterFields, query.objectType, sortField]);
  // Fast paging walks by cursor, and NetBox refuses ?ordering= together with
  // ?start= ("Ordering cannot be specified in conjunction with cursor
  // pagination"), so a sort cannot reach a query that takes that walk.
  // Disabled rather than hidden, and the tooltip names the setting: it lives on
  // the data source, so a panel author has no other way to learn why sorting is
  // unavailable here. Optional chaining because a saved query is edited by
  // whatever object Grafana hands us.
  //
  // Mode is part of the condition. Fast paging is NetBox's cursor walk; the
  // replica-cache backend ignores FastPagingNoTotals entirely and honours its
  // own sort parameter, so reading the flag alone took a working control away
  // from it — and the value survives a mode switch, so a datasource that had
  // fast paging on before being pointed at the cache arrived with sorting
  // disabled for a limit that does not apply.
  const fastPaging = !cacheMode && settings?.fastPagingNoTotals === true;
  // The setting alone is not the condition — the cursor walk is, and an
  // alert-table query never takes it. pkg/plugin/query.go's alertTable branch
  // leaves AllowUncounted false (only the plain objects branch sets it, and only
  // off-alert), so netbox.go's `p.cursorPaging && spec.AllowUncounted &&
  // cursorLegal(q)` stays false: the sort goes out with no ?start= beside it, and
  // this shape behaves identically whether the setting is on or off. Gating on
  // the setting alone disabled a control the backend honours, and blamed a limit
  // that does not apply to it. (Alert EVALUATION drops the sort — queryOrdering
  // on the FromAlert header — but that is every shape's behaviour, fast paging or
  // not, and the picker is live for those too.)
  //
  // A count query needs no equivalent term: the whole block below is hidden for
  // one, several lines further down, and for a better reason than this one.
  const orderingDisabled = fastPaging && !(query.alertTable ?? false);

  const filters = query.filters ?? [];
  const filterIssues = validateFilters(filters);
  const update = (patch: Partial<NetBoxQuery>) => onChange({ ...query, ...patch });
  const updateFilter = (i: number, patch: Partial<FilterRow>) =>
    update({ filters: filters.map((f, idx) => (idx === i ? { ...f, ...patch } : f)) });
  const addFilter = () => update({ filters: [...filters, { field: '', operator: '', value: '' }] });
  const removeFilter = (i: number) => update({ filters: filters.filter((_, idx) => idx !== i) });

  const filterLabel = isTopologyQuery(queryType) ? 'Device filter' : 'Filter';

  return (
    <Stack gap={1} direction="column">
      <InlineField label="Query type" labelWidth={20} grow>
        <Select<QueryType>
          inputId="query-type"
          width={40}
          options={QUERY_TYPES}
          value={QUERY_TYPES.find((o) => o.value === queryType)}
          onChange={(v) => {
            const qt = v?.value ?? 'objects';
            // Filter rows are bound to a NetBox model: dcim/devices for objects
            // and topology, ipam/ip-addresses for a scope-sourced ip-enrichment
            // query. Rows written for one must not survive into the other —
            // NetBox ignores an unknown filter parameter and returns
            // everything, which for a scope is the whole address table under
            // the limit, with nothing to refuse. Objects ↔ topology keep theirs.
            const modelChanges = (qt === 'ip-enrichment') !== (queryType === 'ip-enrichment');
            update({
              queryType: qt,
              ...(isTopologyQuery(qt) ? { connectedOnly: query.connectedOnly ?? true } : {}),
              ...(modelChanges ? { filters: [] } : {}),
            });
            onRunQuery();
          }}
        />
      </InlineField>

      <InlineField
        label="Branch"
        labelWidth={20}
        disabled={branchDisabled}
        tooltip={branchDisabled ? BRANCH_FIELD_DISABLED_TOOLTIP : BRANCH_FIELD_TOOLTIP}
      >
        <Input
          id="query-branch"
          width={40}
          value={query.branch ?? ''}
          placeholder="(main)"
          onChange={(e) => update({ branch: e.currentTarget.value || undefined })}
          onBlur={() => onRunQuery()}
        />
      </InlineField>

      {queryType === 'objects' && (
        <InlineField label="Object type" labelWidth={20} grow tooltip="Discovered from NetBox, including plugin models">
          <Select
            inputId="query-object-type"
            width={50}
            isLoading={loadingTypes}
            options={typeOptions}
            value={typeOptions.find((o) => o.value === query.objectType) ?? null}
            placeholder="Select an object type (e.g. Devices)"
            onChange={(v) => {
              // `ordering` is reset alongside the rest because the sortable set
              // is per object type: `role` is offered on dcim/devices and not on
              // ipam/prefixes, so carrying it over leaves a picker showing a
              // sort the backend will refuse to send — unsorted rows, and the
              // control claiming otherwise.
              update({ objectType: v?.value, fields: [], filters: [], valueField: undefined, ordering: undefined });
              onRunQuery();
            }}
          />
        </InlineField>
      )}

      {queryType === 'ip-enrichment' && (
        <>
          <InlineField
            label="Source"
            labelWidth={20}
            tooltip="IP list: resolve the IPs you give (or a variable of them). NetBox scope: every address NetBox holds under the filters below — for alert rules, which cannot supply an IP list but can join a metric's IP against this table in a SQL expression."
          >
            <RadioButtonGroup<'list' | 'scope'>
              options={[
                { label: 'IP list', value: 'list' },
                { label: 'NetBox scope', value: 'scope' },
              ]}
              value={query.ipSource ?? 'list'}
              onChange={(v) => {
                // Each source owns its input; stale IPs on a scope query would
                // be misleading in the saved JSON, and stale filters on a list
                // query would be sent to nothing. Not run here: a scope with no
                // filter row is the whole address table (filterQuery holds it
                // back until a row has a field; the value's onBlur runs it).
                update(
                  v === 'scope'
                    ? {
                        ipSource: v,
                        ips: undefined,
                        filters: [],
                        // prefix_* can never fill on a scope query (the pickers
                        // stop offering it), so a stale selection goes too — as a
                        // context field and as a join-key source, which would
                        // otherwise derive an empty column on every row.
                        contextFields: query.contextFields?.filter((f) => !f.startsWith('prefix_')),
                        joinKeys: query.joinKeys?.filter((k) => !k.source.startsWith('prefix_')),
                      }
                    : { ipSource: v, filters: [] }
                );
              }}
            />
          </InlineField>
          {!isScope && (
            <InlineField
              label="IPs"
              labelWidth={20}
              grow
              tooltip="IPs to resolve (comma/space/newline separated). Supports $variables — e.g. a query variable of source IPs from your flow data."
            >
              <TextArea
                rows={3}
                value={query.ips ?? ''}
                placeholder="10.0.0.5, 192.0.2.10  or  $flow_src_ips"
                onChange={(e) => update({ ips: e.currentTarget.value })}
                onBlur={() => onRunQuery()}
              />
            </InlineField>
          )}
          <InlineField
            label="Context fields"
            labelWidth={20}
            grow
            tooltip="Columns to return alongside each IP: identity, longest-matching prefix, the address record, the interface it's assigned to, and the owning device or virtual machine"
          >
            <MultiSelect
              // Every scope row has an address record, and the prefix hop runs
              // only for IPs that have none, so prefix_* can never fill on a
              // scope query: not offered rather than offered and always blank.
              options={isScope ? IP_CONTEXT_FIELD_GROUPS.filter((g) => g.label !== 'Prefix') : IP_CONTEXT_FIELD_GROUPS}
              value={(query.contextFields ?? DEFAULT_IP_CONTEXT_FIELDS).map((f) => ({ label: f, value: f }))}
              onChange={(vals) => update({ contextFields: vals.map((v) => v.value!).filter(Boolean) })}
            />
          </InlineField>
        </>
      )}

      {isTopologyQuery(queryType) && (
        <>
          <div className={styles.hint}>
            {queryType === 'topology-edges'
              ? 'Returns each link as a row (device, peer, and both roles) for joining in an alert rule — see docs/alerting/topology-suppression.md. Use a Table visualization. Filter the device set below; the filter must cover every device the rule evaluates.'
              : 'Returns NetBox devices as nodes and inter-device links as edges, colored by device status. Use the Node Graph visualization. Filter the device set below (e.g. site or role).'}
          </div>
          <InlineField
            label="Connections"
            labelWidth={20}
            tooltip="logical: NetBox cable paths — patch panels and circuits resolve to the far device. physical: raw cables — panels appear as nodes. Wireless links are always included."
          >
            <Select
              inputId="query-connections"
              width={30}
              options={[
                { label: 'logical (cable paths)', value: 'logical' },
                { label: 'physical (raw cables)', value: 'physical' },
              ]}
              value={query.connections ?? 'logical'}
              onChange={(v) => {
                update({ connections: (v?.value as 'logical' | 'physical') ?? 'logical' });
                onRunQuery();
              }}
            />
          </InlineField>
          <InlineField label="Connected only" labelWidth={20} tooltip="Drop devices with no inter-device link">
            <InlineSwitch
              value={query.connectedOnly ?? true}
              onChange={(e) => {
                update({ connectedOnly: e.currentTarget.checked });
                onRunQuery();
              }}
            />
          </InlineField>
        </>
      )}

      {/* Filters — for objects, topology, and a scope-sourced ip-enrichment query */}
      {showsFilters &&
        filters.map((f, i) => (
          <Stack key={i} gap={0} direction="column">
            <Stack gap={1} direction="row" alignItems="flex-end">
              <InlineField label={i === 0 ? filterLabel : ' '} labelWidth={20}>
                <Select
                  aria-label={`filter-field-${i}`}
                  width={24}
                  options={
                    f.field && !filterFieldOptions.some((o) => o.value === f.field)
                      ? [...filterFieldOptions, { label: f.field, value: f.field }]
                      : filterFieldOptions
                  }
                  allowCustomValue={!schemaMode}
                  value={f.field ? { label: f.field, value: f.field } : null}
                  placeholder="field"
                  onChange={(v) => {
                    const nf = v?.value ?? '';
                    // Reset a stale operator when the new field doesn't support it
                    // (schema-known fields only); otherwise carrying e.g. `contains`
                    // onto a field that only supports `=` re-creates a silently
                    // ignored filter. Unknown/fallback fields keep the operator.
                    const operator = isOperatorValidForField(filterFields, nf, f.operator) ? f.operator : '';
                    updateFilter(i, { field: nf, operator });
                  }}
                />
              </InlineField>
              {(() => {
                const opts = filterOperatorsFor(filterFields, f.field, f.operator);
                return (
                  <Select
                    aria-label={`filter-operator-${i}`}
                    width={16}
                    options={opts}
                    value={opts.find((o) => o.value === f.operator) ?? opts[0]}
                    onChange={(v) => {
                      const operator = v?.value ?? '';
                      updateFilter(i, { operator });
                      // Empty-family operators take no value, so the row is
                      // already a complete filter the moment the operator is
                      // picked — the value Input (the usual run trigger, via
                      // its onBlur) doesn't render for these, so nothing else
                      // would ever run the query. Other operators still wait
                      // for the value Input's onBlur, so mid-edit changes don't
                      // fire a query per keystroke/selection.
                      if (EMPTY_FAMILY_OPERATORS.includes(operator)) {
                        onRunQuery();
                      }
                    }}
                  />
                );
              })()}
              {!EMPTY_FAMILY_OPERATORS.includes(f.operator) && (
                <Input
                  width={24}
                  value={f.value}
                  placeholder="value or $variable"
                  onChange={(e) => updateFilter(i, { value: e.currentTarget.value })}
                  onBlur={() => onRunQuery()}
                />
              )}
              <IconButton name="trash-alt" aria-label="Remove filter" onClick={() => removeFilter(i)} />
            </Stack>
            {filterIssues
              .filter((issue) => issue.index === i)
              .map((issue, k) => (
                <Alert key={k} severity={issue.severity} title={issue.message} />
              ))}
          </Stack>
        ))}
      {showsFilters && (
        <Stack gap={1} direction="row">
          <Button variant="secondary" size="sm" icon="plus" onClick={addFilter}>
            Add filter
          </Button>
        </Stack>
      )}

      {queryType === 'objects' && (
        <InlineField
          label="Return fields"
          labelWidth={20}
          grow
          tooltip="Optional. Leave empty to return all columns. Pick a key column (name/address) to join with metrics."
        >
          <MultiSelect
            inputId="query-fields"
            options={fieldOptions}
            value={(query.fields ?? []).map((f) => ({ label: f, value: f }))}
            placeholder="All columns"
            onChange={(vals) => {
              update({ fields: vals.map((v) => v.value!).filter(Boolean) });
              onRunQuery();
            }}
          />
        </InlineField>
      )}

      {/* Sort by — objects only, and not for a count: a single number has no
          row order, so offering to sort one would be a control that changes
          nothing but the query's cost. */}
      {queryType === 'objects' && !(query.count ?? false) && orderingOptions.length > 0 && (
        <Stack gap={1} direction="row" alignItems="flex-end">
          <InlineField
            label="Sort by"
            labelWidth={20}
            disabled={orderingDisabled}
            tooltip={orderingDisabled ? ORDERING_DISABLED_TOOLTIP : ORDERING_TOOLTIP}
          >
            <Select
              inputId="query-ordering"
              width={30}
              isClearable
              options={orderingOptions}
              // Built from the stored value, not looked up in the options: a
              // provisioned dashboard can carry a field this list no longer
              // offers, and showing it is what lets the user see and clear it.
              value={sortField ? { label: sortField, value: sortField } : null}
              placeholder="NetBox's own order"
              onChange={(v) => {
                update({ ordering: composeOrdering(v?.value ?? '', sortDescending) });
                onRunQuery();
              }}
            />
          </InlineField>
          {/* Direction only once a field is chosen — asc/desc of nothing is not
              a question. Compared explicitly rather than left truthy so an
              empty sortField renders nothing at all, not an empty text node. */}
          {sortField !== '' && (
            <Select
              aria-label="ordering-direction"
              width={18}
              disabled={orderingDisabled}
              options={ORDERING_DIRECTIONS}
              value={ORDERING_DIRECTIONS.find((o) => o.value === (sortDescending ? 'desc' : 'asc'))}
              onChange={(v) => {
                update({ ordering: composeOrdering(sortField, v?.value === 'desc') });
                onRunQuery();
              }}
            />
          )}
        </Stack>
      )}

      {queryType === 'objects' && (
        <InlineField
          label="Return count only"
          labelWidth={20}
          tooltip="Return a single numeric count of matching objects instead of a table. Enable this to alert on object counts — Grafana alert rules evaluate a number, not a table."
        >
          <InlineSwitch
            label="Return count only"
            value={query.count ?? false}
            onChange={(e) => {
              update({ count: e.currentTarget.checked, alertTable: false, valueField: undefined });
              onRunQuery();
            }}
          />
        </InlineField>
      )}

      {queryType === 'objects' && (
        <InlineField
          label="Alert table"
          labelWidth={20}
          tooltip="Shape the result for Grafana alerting: every selected column becomes an alert label and a single numeric 'value' column drives the condition — one alert instance per row. Pick a Value field (e.g. utilization) or leave it empty to emit a constant 1 per matching object."
        >
          <InlineSwitch
            label="Alert table"
            value={query.alertTable ?? false}
            onChange={(e) => {
              update({
                alertTable: e.currentTarget.checked,
                count: false,
                valueField: e.currentTarget.checked ? query.valueField : undefined,
              });
              onRunQuery();
            }}
          />
        </InlineField>
      )}

      {queryType === 'objects' && (query.alertTable ?? false) && (
        <InlineField
          label="Value field"
          labelWidth={20}
          tooltip="Numeric column used as the alert value. Leave empty to emit a constant 1 per row (alert on existence, e.g. offline devices)."
        >
          <Select
            inputId="query-alert-value-field"
            width={40}
            isClearable
            placeholder="constant 1"
            options={fieldOptions}
            value={query.valueField ? { label: query.valueField, value: query.valueField } : null}
            onChange={(v) => {
              update({ valueField: v?.value ?? undefined });
              onRunQuery();
            }}
          />
        </InlineField>
      )}

      {/* Join keys — for objects and ip-enrichment */}
      {(queryType === 'objects' || queryType === 'ip-enrichment') && (
        <JoinKeysEditor
          joinKeys={query.joinKeys ?? []}
          // The source field must be a column the result actually has, and for
          // ip-enrichment that is the enrichment vocabulary, not a NetBox object
          // type's schema. `device_name` — the column that joins against a
          // Prometheus `device` label, see docs/RECIPES.md recipe 3b — used to
          // require typing it in as a custom value.
          //
          // The whole vocabulary is offered, NOT just the Context fields
          // selection, and the backend makes that honest: a source outside the
          // selection is added to the enrichment request so the join can compute,
          // then removed again so it does not appear as a column (see
          // pkg/plugin.ipEnrichFields). Narrowing this list to the selection was
          // the alternative, and it answers "join on device_name" with "you may
          // not" for a request that is both expressible and cheap to honour.
          fieldOptions={
            isScope
              ? IP_CONTEXT_FIELD_OPTIONS.filter((o) => !o.value.startsWith('prefix_'))
              : queryType === 'ip-enrichment'
                ? IP_CONTEXT_FIELD_OPTIONS
                : fieldOptions
          }
          onChange={(joinKeys) => update({ joinKeys })}
          onRunQuery={onRunQuery}
        />
      )}

      <InlineField label="Limit" labelWidth={20} tooltip="Maximum number of rows">
        <Input
          width={20}
          type="number"
          value={query.limit ?? 100}
          onChange={(e) => update({ limit: parseInt(e.currentTarget.value, 10) || 0 })}
          onBlur={() => onRunQuery()}
        />
      </InlineField>
    </Stack>
  );
}

interface JoinKeysProps {
  joinKeys: JoinKeyMapping[];
  fieldOptions: Array<SelectableValue<string>>;
  onChange: (keys: JoinKeyMapping[]) => void;
  onRunQuery: () => void;
}

function JoinKeysEditor({ joinKeys, fieldOptions, onChange, onRunQuery }: JoinKeysProps) {
  const update = (i: number, patch: Partial<JoinKeyMapping>) =>
    onChange(joinKeys.map((k, idx) => (idx === i ? { ...k, ...patch } : k)));
  const add = () => onChange([...joinKeys, { source: '', output: '', transform: 'none' }]);
  const remove = (i: number) => onChange(joinKeys.filter((_, idx) => idx !== i));

  return (
    <Stack gap={1} direction="column">
      {joinKeys.map((k, i) => (
        <Stack key={i} gap={1} direction="row" alignItems="flex-end">
          <InlineField
            label={i === 0 ? 'Join key' : ' '}
            labelWidth={20}
            tooltip="Derive a key column to match a metric label"
          >
            <Select
              aria-label={`join-key-source-${i}`}
              width={22}
              options={fieldOptions}
              allowCustomValue
              value={k.source ? { label: k.source, value: k.source } : null}
              placeholder="source field"
              onChange={(v) => update(i, { source: v?.value ?? '' })}
            />
          </InlineField>
          <Input
            width={20}
            value={k.output}
            placeholder="output column (e.g. instance)"
            onChange={(e) => update(i, { output: e.currentTarget.value })}
            onBlur={() => onRunQuery()}
          />
          <Select
            width={18}
            options={JOIN_KEY_TRANSFORMS}
            value={JOIN_KEY_TRANSFORMS.find((o) => o.value === (k.transform || 'none'))}
            onChange={(v) => {
              update(i, { transform: v?.value ?? 'none' });
              onRunQuery();
            }}
          />
          {k.transform === 'regex' && (
            <>
              <Input
                width={20}
                value={k.regex ?? ''}
                placeholder="regex"
                onChange={(e) => update(i, { regex: e.currentTarget.value })}
                onBlur={() => onRunQuery()}
              />
              <Input
                width={16}
                value={k.replace ?? ''}
                placeholder="replace (opt)"
                onChange={(e) => update(i, { replace: e.currentTarget.value })}
                onBlur={() => onRunQuery()}
              />
            </>
          )}
          <IconButton name="trash-alt" aria-label="Remove join key" onClick={() => remove(i)} />
        </Stack>
      ))}
      <Stack gap={1} direction="row">
        <Button variant="secondary" size="sm" icon="plus" onClick={add}>
          Add join key
        </Button>
      </Stack>
    </Stack>
  );
}
