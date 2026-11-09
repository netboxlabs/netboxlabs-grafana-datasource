import React, { useState } from 'react';
import { InlineField, InlineSwitch, MultiSelect, Input, RadioButtonGroup, Select, Stack } from '@grafana/ui';
import { SelectableValue } from '@grafana/data';
import { DataSource } from '../datasource';
import {
  useBranchingInstalled,
  BRANCH_FIELD_TOOLTIP,
  BRANCH_FIELD_DISABLED_TOOLTIP,
} from '../hooks/useBranchingInstalled';
import { NetBoxQuery } from '../types';

interface Props {
  query: NetBoxQuery;
  onChange: (query: NetBoxQuery) => void;
  // Grafana passes the datasource to annotation query editors at runtime
  // (AnnotationQueryEditorProps extends QueryEditorProps); declare it so we can
  // gate the Branch field on branching availability.
  datasource: DataSource;
}

// Common NetBox content types for filtering changelog annotations. Users can add
// custom values for plugin models.
const CONTENT_TYPE_PRESETS: Array<SelectableValue<string>> = [
  { label: 'Device', value: 'dcim.device' },
  { label: 'Interface', value: 'dcim.interface' },
  { label: 'Cable', value: 'dcim.cable' },
  { label: 'Site', value: 'dcim.site' },
  { label: 'Rack', value: 'dcim.rack' },
  { label: 'IP address', value: 'ipam.ipaddress' },
  { label: 'Prefix', value: 'ipam.prefix' },
  { label: 'VLAN', value: 'ipam.vlan' },
  { label: 'Circuit', value: 'circuits.circuit' },
];

const SCOPE_TYPE_PRESETS: Array<SelectableValue<string>> = [
  { label: 'Device', value: 'dcim.device' },
  { label: 'Virtual machine', value: 'virtualization.virtualmachine' },
  { label: 'Rack', value: 'dcim.rack' },
  { label: 'Site', value: 'dcim.site' },
  { label: 'Circuit', value: 'circuits.circuit' },
  { label: 'Interface', value: 'dcim.interface' },
];

const SCOPE_MATCH_OPTIONS: Array<SelectableValue<'name' | 'id'>> = [
  { label: 'Name', value: 'name' },
  { label: 'ID', value: 'id' },
];

const ACTION_OPTIONS: Array<SelectableValue<string>> = [
  { label: 'Created', value: 'create' },
  { label: 'Updated', value: 'update' },
  { label: 'Deleted', value: 'delete' },
];

export function AnnotationQueryEditor({ query, onChange, datasource }: Props) {
  const branchingInstalled = useBranchingInstalled(datasource);
  const branchDisabled = branchingInstalled === false;
  // Grafana re-runs the annotation query on every editor change, so the scope is
  // edited locally and committed on blur.
  const [scopeText, setScopeText] = useState(query.scope ?? '');
  const [seenScope, setSeenScope] = useState(query.scope);
  if (seenScope !== query.scope) {
    // The query changed from outside (e.g. an undo): adopt it.
    setSeenScope(query.scope);
    setScopeText(query.scope ?? '');
  }
  const selected = (query.objectTypes ?? []).map((v) => ({ label: v, value: v }));

  return (
    <Stack gap={1} direction="column">
      <InlineField
        label="Object types"
        labelWidth={20}
        grow
        tooltip="Restrict to these NetBox content types. Leave empty for all changes."
      >
        <MultiSelect
          options={CONTENT_TYPE_PRESETS}
          value={selected}
          allowCustomValue
          placeholder="All object types"
          onChange={(vals) => onChange({ ...query, objectTypes: vals.map((v) => v.value!).filter(Boolean) })}
        />
      </InlineField>
      <InlineField
        label="Scope type"
        labelWidth={20}
        tooltip="NetBox content type of the objects to follow. Leave empty to show all changes."
      >
        <Select
          inputId="annotation-scope-type"
          width={40}
          options={SCOPE_TYPE_PRESETS}
          value={query.scopeType ? { label: query.scopeType, value: query.scopeType } : null}
          allowCustomValue
          isClearable
          placeholder="dcim.device"
          onChange={(v) => onChange({ ...query, scopeType: v?.value || undefined })}
        />
      </InlineField>
      <InlineField
        label="Scope"
        labelWidth={20}
        tooltip="Names, ids or a dashboard variable selecting the objects whose changes to show."
      >
        <Input
          id="annotation-scope"
          width={40}
          value={scopeText}
          placeholder="name, id or $variable"
          onChange={(e) => setScopeText(e.currentTarget.value)}
          onBlur={() => {
            const next = scopeText === '' ? undefined : scopeText;
            if (next !== query.scope) {
              onChange({ ...query, scope: next });
            }
          }}
        />
      </InlineField>
      <InlineField label="Match by" labelWidth={20} tooltip="Whether the scope values are object names or ids.">
        <RadioButtonGroup
          options={SCOPE_MATCH_OPTIONS}
          value={query.scopeMatch ?? 'name'}
          onChange={(v) => onChange({ ...query, scopeMatch: v })}
        />
      </InlineField>
      <InlineField
        label="Actions"
        labelWidth={20}
        grow
        tooltip="Restrict to these change actions. Leave empty for all."
      >
        <MultiSelect
          inputId="annotation-actions"
          options={ACTION_OPTIONS}
          value={query.actions ?? []}
          placeholder="All actions"
          onChange={(vals) => {
            const actions = vals.map((v) => v.value!).filter(Boolean);
            onChange({ ...query, actions: actions.length ? actions : undefined });
          }}
        />
      </InlineField>
      <InlineField
        label="Changed fields"
        labelWidth={20}
        grow
        tooltip="Only changes that touched one of these fields. Custom fields are written cf_name."
      >
        <MultiSelect
          inputId="annotation-changed-fields"
          options={[]}
          value={(query.changedFields ?? []).map((v) => ({ label: v, value: v }))}
          allowCustomValue
          placeholder="Any field (custom fields: cf_name)"
          onChange={(vals) => {
            const fields = vals.map((v) => v.value!).filter(Boolean);
            onChange({ ...query, changedFields: fields.length ? fields : undefined });
          }}
        />
      </InlineField>
      <InlineField label="Journal entries" labelWidth={20} tooltip="Also show journal entries for the scoped objects.">
        <InlineSwitch
          id="annotation-journal"
          value={query.journal ?? false}
          onChange={(e) => onChange({ ...query, journal: e.currentTarget.checked })}
        />
      </InlineField>
      <InlineField label="Limit" labelWidth={20} tooltip="Maximum number of change events">
        <Input
          width={20}
          type="number"
          value={query.limit ?? 1000}
          onChange={(e) => onChange({ ...query, limit: parseInt(e.currentTarget.value, 10) || 0 })}
        />
      </InlineField>
      <InlineField
        label="Branch"
        labelWidth={20}
        disabled={branchDisabled}
        tooltip={branchDisabled ? BRANCH_FIELD_DISABLED_TOOLTIP : BRANCH_FIELD_TOOLTIP}
      >
        <Input
          id="annotation-branch"
          width={40}
          value={query.branch ?? ''}
          placeholder="(main)"
          onChange={(e) => onChange({ ...query, branch: e.currentTarget.value || undefined })}
        />
      </InlineField>
    </Stack>
  );
}
