import React, { ChangeEvent } from 'react';
import { InlineField, Input, SecretInput, Select, InlineSwitch, FieldSet } from '@grafana/ui';
import { DataSourcePluginOptionsEditorProps, SelectableValue } from '@grafana/data';
import { NetBoxDataSourceOptions, NetBoxSecureJsonData, ProviderMode } from '../types';

interface Props extends DataSourcePluginOptionsEditorProps<NetBoxDataSourceOptions, NetBoxSecureJsonData> {}

const MODE_OPTIONS: Array<SelectableValue<ProviderMode>> = [
  { label: 'NetBox REST API', value: 'netbox', description: 'Query the NetBox REST API directly' },
  {
    label: 'Network Context Service (coming soon)',
    value: 'ncs',
    description: 'High-volume enrichment service for NetBox Cloud/Enterprise',
  },
];

export function ConfigEditor(props: Props) {
  const { onOptionsChange, options } = props;
  const { jsonData, secureJsonFields, secureJsonData } = options;

  const onJsonChange = (patch: Partial<NetBoxDataSourceOptions>) => {
    onOptionsChange({ ...options, jsonData: { ...jsonData, ...patch } });
  };

  const onTokenChange = (event: ChangeEvent<HTMLInputElement>) => {
    onOptionsChange({ ...options, secureJsonData: { apiToken: event.target.value } });
  };

  const onResetToken = () => {
    onOptionsChange({
      ...options,
      secureJsonFields: { ...secureJsonFields, apiToken: false },
      secureJsonData: { ...secureJsonData, apiToken: '' },
    });
  };

  const mode = jsonData.mode ?? 'netbox';

  return (
    <>
      <FieldSet label="Connection">
        <InlineField label="Mode" labelWidth={20} tooltip="Where to read NetBox context from">
          <Select<ProviderMode>
            inputId="config-mode"
            width={40}
            options={MODE_OPTIONS}
            value={MODE_OPTIONS.find((o) => o.value === mode)}
            onChange={(v) => onJsonChange({ mode: (v.value as ProviderMode) ?? 'netbox' })}
          />
        </InlineField>

        <InlineField label="NetBox URL" labelWidth={20} tooltip="Base URL of the NetBox instance, without /api">
          <Input
            id="config-url"
            width={40}
            value={jsonData.url ?? ''}
            placeholder="https://netbox.example.com"
            onChange={(e: ChangeEvent<HTMLInputElement>) => onJsonChange({ url: e.target.value })}
          />
        </InlineField>

        <InlineField
          label="Browser URL"
          labelWidth={20}
          tooltip="Where users' browsers reach NetBox, if different from the URL above (e.g. Grafana connects via an internal service name). Used to build 'View in NetBox' links. Leave empty if both match."
        >
          <Input
            id="config-public-url"
            width={40}
            value={jsonData.publicUrl ?? ''}
            placeholder="(optional) where browsers reach NetBox"
            onChange={(e: ChangeEvent<HTMLInputElement>) => onJsonChange({ publicUrl: e.target.value })}
          />
        </InlineField>

        <InlineField label="API Token" labelWidth={20} tooltip="NetBox API token (v1 or v2)">
          <SecretInput
            required
            id="config-api-token"
            width={40}
            isConfigured={Boolean(secureJsonFields?.apiToken)}
            value={secureJsonData?.apiToken ?? ''}
            placeholder="nbt_… or a 40-character token"
            onReset={onResetToken}
            onChange={onTokenChange}
          />
        </InlineField>
      </FieldSet>

      <FieldSet label="Advanced">
        <InlineField label="Skip TLS verify" labelWidth={20} tooltip="Accept self-signed certificates">
          <InlineSwitch
            id="config-tls-skip"
            value={Boolean(jsonData.tlsSkipVerify)}
            onChange={(e) => onJsonChange({ tlsSkipVerify: e.currentTarget.checked })}
          />
        </InlineField>

        <InlineField label="Timeout (s)" labelWidth={20} tooltip="Per-request upstream timeout">
          <Input
            id="config-timeout"
            width={40}
            type="number"
            value={jsonData.timeoutSeconds ?? 30}
            onChange={(e: ChangeEvent<HTMLInputElement>) =>
              onJsonChange({ timeoutSeconds: parseInt(e.target.value, 10) || 30 })
            }
          />
        </InlineField>
      </FieldSet>
    </>
  );
}
