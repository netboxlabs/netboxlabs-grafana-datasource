import React, { useEffect, useMemo, useState } from 'react';
import { InlineField, Select, Input, Stack, Button, IconButton } from '@grafana/ui';
import { SelectableValue } from '@grafana/data';
import { DataSource } from '../datasource';
import { getTemplateSrv } from '@grafana/runtime';
import { useBranchingInstalled, BRANCH_FIELD_TOOLTIP, BRANCH_FIELD_DISABLED_TOOLTIP } from '../hooks/useBranchingInstalled';
import { FieldOption, FilterRow, NetBoxVariableQuery, ObjectTypeOption } from '../types';

interface Props {
  query: NetBoxVariableQuery;
  onChange: (query: NetBoxVariableQuery) => void;
  datasource: DataSource;
}

export function VariableQueryEditor({ query, onChange, datasource }: Props) {
  const branchingInstalled = useBranchingInstalled(datasource);
  const branchDisabled = branchingInstalled === false;
  const [objectTypes, setObjectTypes] = useState<ObjectTypeOption[]>([]);
  const [fields, setFields] = useState<string[]>([]);

  useEffect(() => {
    datasource.getObjectTypes().then(setObjectTypes).catch(() => setObjectTypes([]));
  }, [datasource]);

  useEffect(() => {
    let active = true;
    const branch = query.branch ? getTemplateSrv().replace(query.branch) : undefined;
    const p = query.objectType
      ? datasource.getFields(query.objectType, branch)
      : Promise.resolve<FieldOption[]>([]);
    p.then((f) => active && setFields(f.map((x) => x.name))).catch(() => active && setFields([]));
    return () => {
      active = false;
    };
  }, [datasource, query.objectType, query.branch]);

  const typeOptions: Array<SelectableValue<string>> = useMemo(
    () => objectTypes.map((t) => ({ label: t.label, value: t.value, description: t.value })),
    [objectTypes]
  );
  const fieldOptions: Array<SelectableValue<string>> = useMemo(
    () => fields.map((f) => ({ label: f, value: f })),
    [fields]
  );

  const filters = query.filters ?? [];
  const update = (patch: Partial<NetBoxVariableQuery>) => onChange({ ...query, ...patch });
  const updateFilter = (i: number, patch: Partial<FilterRow>) =>
    update({ filters: filters.map((f, idx) => (idx === i ? { ...f, ...patch } : f)) });

  return (
    <Stack gap={1} direction="column">
      <InlineField
        label="Branch"
        labelWidth={16}
        disabled={branchDisabled}
        tooltip={branchDisabled ? BRANCH_FIELD_DISABLED_TOOLTIP : BRANCH_FIELD_TOOLTIP}
      >
        <Input
          id="variable-branch"
          width={40}
          value={query.branch ?? ''}
          placeholder="(main)"
          onChange={(e) => update({ branch: e.currentTarget.value || undefined })}
        />
      </InlineField>
      <InlineField label="Object type" labelWidth={16} grow>
        <Select
          width={40}
          options={typeOptions}
          value={typeOptions.find((o) => o.value === query.objectType) ?? null}
          placeholder="Select an object type"
          onChange={(v) => update({ objectType: v?.value })}
        />
      </InlineField>
      <InlineField label="Value field" labelWidth={16} tooltip="Field used as the variable value (default: name)">
        <Select
          width={40}
          options={fieldOptions}
          allowCustomValue
          value={query.valueField ? { label: query.valueField, value: query.valueField } : null}
          placeholder="name"
          onChange={(v) => update({ valueField: v?.value })}
        />
      </InlineField>
      <InlineField label="Text field" labelWidth={16} tooltip="Field shown in the dropdown (default: value field)">
        <Select
          width={40}
          options={fieldOptions}
          allowCustomValue
          value={query.textField ? { label: query.textField, value: query.textField } : null}
          placeholder="(same as value)"
          onChange={(v) => update({ textField: v?.value })}
        />
      </InlineField>

      {filters.map((f, i) => (
        <Stack key={i} gap={1} direction="row" alignItems="flex-end">
          <InlineField label={i === 0 ? 'Filter' : ' '} labelWidth={16}>
            <Select
              width={24}
              options={fieldOptions}
              allowCustomValue
              value={f.field ? { label: f.field, value: f.field } : null}
              placeholder="field"
              onChange={(v) => updateFilter(i, { field: v?.value ?? '' })}
            />
          </InlineField>
          <Input
            width={24}
            value={f.value}
            placeholder="value or $variable"
            onChange={(e) => updateFilter(i, { value: e.currentTarget.value })}
          />
          <IconButton
            name="trash-alt"
            aria-label="Remove filter"
            onClick={() => update({ filters: filters.filter((_, idx) => idx !== i) })}
          />
        </Stack>
      ))}
      <Stack gap={1} direction="row">
        <Button
          variant="secondary"
          size="sm"
          icon="plus"
          onClick={() => update({ filters: [...filters, { field: '', operator: '', value: '' }] })}
        >
          Add filter
        </Button>
      </Stack>
    </Stack>
  );
}
