import React, { useEffect, useMemo, useState } from 'react';
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
} from '@grafana/ui';
import { QueryEditorProps, SelectableValue } from '@grafana/data';
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
  validateFilters,
} from '../types';

type Props = QueryEditorProps<DataSource, NetBoxQuery, NetBoxDataSourceOptions>;

const QUERY_TYPES: Array<SelectableValue<QueryType>> = [
  { label: 'Objects', value: 'objects', description: 'Query a NetBox object type as a joinable table' },
  {
    label: 'IP enrichment',
    value: 'ip-enrichment',
    description: 'Resolve IPs to their NetBox device, interface and address record (longest-prefix fallback)',
  },
  { label: 'Topology', value: 'topology', description: 'Devices + cables as a node graph' },
];

export function QueryEditor({ query, onChange, onRunQuery, datasource }: Props) {
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
    queryType === 'ip-enrichment' ? undefined : queryType === 'topology' ? 'dcim/devices' : query.objectType;

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
    const p = fieldType ? datasource.getFilterFields(fieldType, branch) : Promise.resolve<FilterField[]>([]);
    p.then((ff) => active && setFilterFields(ff)).catch(() => active && setFilterFields([]));
    return () => {
      active = false;
    };
  }, [datasource, fieldType, query.branch]);

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

  const filters = query.filters ?? [];
  const filterIssues = validateFilters(filters);
  const update = (patch: Partial<NetBoxQuery>) => onChange({ ...query, ...patch });
  const updateFilter = (i: number, patch: Partial<FilterRow>) =>
    update({ filters: filters.map((f, idx) => (idx === i ? { ...f, ...patch } : f)) });
  const addFilter = () => update({ filters: [...filters, { field: '', operator: '', value: '' }] });
  const removeFilter = (i: number) => update({ filters: filters.filter((_, idx) => idx !== i) });

  const filterLabel = queryType === 'topology' ? 'Device filter' : 'Filter';

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
            update(
              qt === 'topology' ? { queryType: qt, connectedOnly: query.connectedOnly ?? true } : { queryType: qt }
            );
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
              update({ objectType: v?.value, fields: [], filters: [], valueField: undefined });
              onRunQuery();
            }}
          />
        </InlineField>
      )}

      {queryType === 'ip-enrichment' && (
        <>
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
          <InlineField
            label="Context fields"
            labelWidth={20}
            grow
            tooltip="Columns to return alongside each IP: identity, longest-matching prefix, the address record, the interface it's assigned to, and the owning device or virtual machine"
          >
            <MultiSelect
              options={IP_CONTEXT_FIELD_GROUPS}
              value={(query.contextFields ?? DEFAULT_IP_CONTEXT_FIELDS).map((f) => ({ label: f, value: f }))}
              onChange={(vals) => update({ contextFields: vals.map((v) => v.value!).filter(Boolean) })}
            />
          </InlineField>
        </>
      )}

      {queryType === 'topology' && (
        <>
          <div style={{ opacity: 0.75, fontSize: 12, marginLeft: 4 }}>
            Returns NetBox devices as nodes and inter-device links as edges, colored by device status. Use the Node
            Graph visualization. Filter the device set below (e.g. site or role).
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

      {/* Filters — for objects and topology */}
      {(queryType === 'objects' || queryType === 'topology') &&
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
      {(queryType === 'objects' || queryType === 'topology') && (
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
          fieldOptions={queryType === 'ip-enrichment' ? IP_CONTEXT_FIELD_OPTIONS : fieldOptions}
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
