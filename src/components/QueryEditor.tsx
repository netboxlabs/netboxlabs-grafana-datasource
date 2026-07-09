import React, { useEffect, useMemo, useState } from 'react';
import { InlineField, Input, Select, MultiSelect, Stack, Button, IconButton, TextArea, InlineSwitch } from '@grafana/ui';
import { QueryEditorProps, SelectableValue } from '@grafana/data';
import { DataSource } from '../datasource';
import {
  FILTER_OPERATORS,
  FilterRow,
  IP_CONTEXT_FIELDS,
  JOIN_KEY_TRANSFORMS,
  JoinKeyMapping,
  NetBoxDataSourceOptions,
  NetBoxQuery,
  ObjectTypeOption,
  QueryType,
} from '../types';

type Props = QueryEditorProps<DataSource, NetBoxQuery, NetBoxDataSourceOptions>;

const QUERY_TYPES: Array<SelectableValue<QueryType>> = [
  { label: 'Objects', value: 'objects', description: 'Query a NetBox object type as a joinable table' },
  { label: 'IP enrichment', value: 'ip-enrichment', description: 'Resolve IPs to their NetBox prefix/site/tenant (longest match)' },
  { label: 'Topology', value: 'topology', description: 'Devices + cables as a node graph' },
];

export function QueryEditor({ query, onChange, onRunQuery, datasource }: Props) {
  const queryType: QueryType = query.queryType ?? 'objects';
  const [objectTypes, setObjectTypes] = useState<ObjectTypeOption[]>([]);
  const [fields, setFields] = useState<string[]>([]);
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
  const fieldType =
    queryType === 'ip-enrichment' ? 'ipam/prefixes' : queryType === 'topology' ? 'dcim/devices' : query.objectType;

  useEffect(() => {
    let active = true;
    const p = fieldType ? datasource.getFields(fieldType) : Promise.resolve<Array<{ name: string }>>([]);
    p.then((f) => active && setFields(f.map((x) => x.name))).catch(() => active && setFields([]));
    return () => {
      active = false;
    };
  }, [datasource, fieldType]);

  const typeOptions: Array<SelectableValue<string>> = useMemo(
    () => objectTypes.map((t) => ({ label: t.label, value: t.value, description: t.value })),
    [objectTypes]
  );
  const fieldOptions: Array<SelectableValue<string>> = useMemo(
    () => fields.map((f) => ({ label: f, value: f })),
    [fields]
  );

  const filters = query.filters ?? [];
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
            update(qt === 'topology' ? { queryType: qt, connectedOnly: query.connectedOnly ?? true } : { queryType: qt });
            onRunQuery();
          }}
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
              update({ objectType: v?.value, fields: [], filters: [] });
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
          <InlineField label="Context fields" labelWidth={20} grow tooltip="Prefix columns to return alongside each IP">
            <MultiSelect
              options={IP_CONTEXT_FIELDS.map((f) => ({ label: f, value: f }))}
              allowCustomValue
              value={(query.contextFields ?? IP_CONTEXT_FIELDS).map((f) => ({ label: f, value: f }))}
              onChange={(vals) => update({ contextFields: vals.map((v) => v.value!).filter(Boolean) })}
            />
          </InlineField>
        </>
      )}

      {queryType === 'topology' && (
        <>
          <div style={{ opacity: 0.75, fontSize: 12, marginLeft: 4 }}>
            Returns NetBox devices as nodes and inter-device cables as edges, colored by device status. Use the Node Graph
            visualization. Filter the device set below (e.g. site or role).
          </div>
          <InlineField label="Connected only" labelWidth={20} tooltip="Drop devices with no inter-device cable">
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
          <Stack key={i} gap={1} direction="row" alignItems="flex-end">
            <InlineField label={i === 0 ? filterLabel : ' '} labelWidth={20}>
              <Select
                width={24}
                options={fieldOptions}
                allowCustomValue
                value={f.field ? { label: f.field, value: f.field } : null}
                placeholder="field"
                onChange={(v) => updateFilter(i, { field: v?.value ?? '' })}
              />
            </InlineField>
            <Select
              width={16}
              options={FILTER_OPERATORS}
              value={FILTER_OPERATORS.find((o) => o.value === f.operator) ?? FILTER_OPERATORS[0]}
              onChange={(v) => updateFilter(i, { operator: v?.value ?? '' })}
            />
            <Input
              width={24}
              value={f.value}
              placeholder="value or $variable"
              onChange={(e) => updateFilter(i, { value: e.currentTarget.value })}
              onBlur={() => onRunQuery()}
            />
            <IconButton name="trash-alt" aria-label="Remove filter" onClick={() => removeFilter(i)} />
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
              update({ count: e.currentTarget.checked });
              onRunQuery();
            }}
          />
        </InlineField>
      )}

      {/* Join keys — for objects and ip-enrichment */}
      {(queryType === 'objects' || queryType === 'ip-enrichment') && (
        <JoinKeysEditor
          joinKeys={query.joinKeys ?? []}
          fieldOptions={fieldOptions}
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
          <InlineField label={i === 0 ? 'Join key' : ' '} labelWidth={20} tooltip="Derive a key column to match a metric label">
            <Select
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
