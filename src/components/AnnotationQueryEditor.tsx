import React from 'react';
import { InlineField, MultiSelect, Input, Stack } from '@grafana/ui';
import { SelectableValue } from '@grafana/data';
import { DataSource } from '../datasource';
import { useBranchingInstalled, BRANCH_FIELD_TOOLTIP, BRANCH_FIELD_DISABLED_TOOLTIP } from '../hooks/useBranchingInstalled';
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

export function AnnotationQueryEditor({ query, onChange, datasource }: Props) {
  const branchingInstalled = useBranchingInstalled(datasource);
  const branchDisabled = branchingInstalled === false;
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
