import React, { ChangeEvent, useEffect } from 'react';
import { InlineField, Input, SecretInput, InlineSwitch, FieldSet, Select } from '@grafana/ui';
import { DataSourcePluginOptionsEditorProps } from '@grafana/data';
import { NetBoxDataSourceOptions, NetBoxSecureJsonData, ProviderMode } from '../types';

interface Props extends DataSourcePluginOptionsEditorProps<NetBoxDataSourceOptions, NetBoxSecureJsonData> {}

/** Copy for the Fast paging switch.
 *
 *  Lives in a constant so it can be asserted directly: Grafana renders a
 *  tooltip as an icon and only mounts its text on hover, which jsdom does not
 *  reproduce, so a DOM-based test of this wording either passes for the wrong
 *  reason or tests the tooltip library rather than the copy.
 *
 *  It has to carry BOTH halves of the trade. This switch makes a panel faster
 *  and quietly changes two things a reader would otherwise assume: row order
 *  and whether totals exist. Someone enabling it from the tooltip alone must
 *  learn that here, because nothing downstream will tell them. */
export const FAST_PAGING_TOOLTIP = [
  'For NetBox instances holding millions of objects: table panels page by ID instead of asking',
  'NetBox to count the matches, which is where the time goes at that size.',
  'In exchange, rows come back in ID order rather than the natural order, and totals are',
  'unavailable — a panel says "showing the first 100" rather than "showing 100 of N".',
  'Alert rules are unaffected; every evaluation asks for the real total and natural order.',
  'Leave it off below a few million records.',
].join(' ');

/** Copy for the mode selector.
 *
 *  Kept as a constant for the same reason as FAST_PAGING_TOOLTIP: Grafana only
 *  mounts tooltip text on hover, which jsdom does not reproduce, so asserting
 *  on the constant is how the wording stays under test. Deliberately short:
 *  the differences between the modes live in docs/REPLICA-CACHE.md and in the
 *  query editor at the point of failure, not on the connection form.
 */
export const MODE_TOOLTIP =
  'Where this datasource reads from: the NetBox API, or a replica-cache mirror of the instance, for deployments too large for the API to serve interactively. The URL and API token below name that service.';

const MODE_OPTIONS: Array<{ label: string; value: ProviderMode; description: string }> = [
  { label: 'NetBox API', value: 'netbox', description: 'Query the NetBox instance directly.' },
  { label: 'Replica cache', value: 'replica-cache', description: 'Query a replica-cache mirror of the instance.' },
];

export function ConfigEditor(props: Props) {
  const { onOptionsChange, options } = props;
  const { jsonData, secureJsonFields, secureJsonData } = options;

  const onJsonChange = (patch: Partial<NetBoxDataSourceOptions>) => {
    onOptionsChange({ ...options, jsonData: { ...jsonData, ...patch } });
  };

  // Grafana's data-sources LIST renders each row as
  //   <instance name>
  //   <plugin display name> | <url>
  // reading that url from the settings' TOP-LEVEL field, not from jsonData. We
  // keep the real setting in jsonData.url (models.PluginSettings unmarshals
  // from jsonData and never reads this one), so without mirroring it we are the
  // only datasource in the list with no address shown — and there is no way to
  // tell a NetBox Cloud instance from a self-hosted one at a glance.
  //
  // Mirrored rather than moved: making the top-level field the source of truth
  // would be a settings migration for every existing datasource, and this is
  // presentation only. It is deliberately NOT read anywhere server-side.
  const onUrlChange = (url: string) => {
    onOptionsChange({ ...options, url, jsonData: { ...jsonData, url } });
  };

  // Every datasource configured before the mirror above existed has its address
  // in jsonData.url and an empty top-level url, so on-edit mirroring alone only
  // ever reaches datasources created afterwards — or ones whose URL someone
  // happened to retype. Backfill on open so the rest catch up.
  //
  // This only populates the form's in-memory settings: the list row stays blank
  // until the user presses Save and Test, which is the same round trip any other
  // config change needs. Nothing here changes what the datasource DOES, so a
  // save is never made necessary by this — an untouched config page can still be
  // closed without saving.
  //
  // Runs only when there is something to backfill, and never overwrites a
  // top-level url that already holds something else: that value came from
  // somewhere other than this mirror, and jsonData.url stays the source of
  // truth. Because the guard needs an empty options.url, the write that lands
  // closes it — no render loop.
  // Unset means NetBox, matching the backend default in LoadPluginSettings.
  const mode: ProviderMode = jsonData.mode ?? 'netbox';
  const isCache = mode === 'replica-cache';

  // The address a datasource SHOWS is the one it queries: jsonData.url names
  // whichever service the mode reads from.
  const mirroredUrl = jsonData.url ?? '';
  const topLevelUrl = options.url ?? '';
  useEffect(() => {
    if (mirroredUrl !== '' && topLevelUrl === '') {
      onOptionsChange({ ...options, url: mirroredUrl });
    }
    // options/onOptionsChange are deliberately out of the dep list: they are new
    // identities on every parent render, and this effect must fire on the state
    // it guards on, not on every keystroke elsewhere in the form.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mirroredUrl, topLevelUrl]);

  // One token for both modes: the credential for whichever service the URL
  // names. A mode switch clears it (see the Mode picker), so a credential is
  // re-entered for the new service rather than carried across.
  const onTokenChange = (event: ChangeEvent<HTMLInputElement>) => {
    onOptionsChange({ ...options, secureJsonData: { ...secureJsonData, apiToken: event.target.value } });
  };

  const onResetToken = () => {
    onOptionsChange({
      ...options,
      secureJsonFields: { ...secureJsonFields, apiToken: false },
      secureJsonData: { ...secureJsonData, apiToken: '' },
    });
  };

  return (
    <>
      <FieldSet label="Connection">
        <InlineField label="Mode" labelWidth={20} tooltip={MODE_TOOLTIP}>
          <Select
            inputId="config-mode"
            width={40}
            options={MODE_OPTIONS}
            value={mode}
            onChange={(v) => {
              // Switching mode is switching service. The one token field would
              // otherwise carry the old service's credential over, and the
              // moment the URL is pointed at the new service it would be sent
              // to a host it was never issued for. Cleared here, so it has to
              // be re-entered; Save & test says "API token is missing" until
              // then.
              onOptionsChange({
                ...options,
                jsonData: { ...jsonData, mode: (v.value ?? 'netbox') as ProviderMode },
                secureJsonFields: { ...secureJsonFields, apiToken: false },
                secureJsonData: { ...secureJsonData, apiToken: '' },
              });
            }}
          />
        </InlineField>

        <InlineField
          label="URL"
          labelWidth={20}
          tooltip={
            isCache
              ? 'Base URL of the replica-cache deployment this datasource reads from.'
              : 'Base URL of the NetBox instance, without /api.'
          }
        >
          <Input
            id="config-url"
            width={40}
            value={jsonData.url ?? ''}
            placeholder={isCache ? 'https://<id>.replica-cache.example.com' : 'https://netbox.example.com'}
            onChange={(e: ChangeEvent<HTMLInputElement>) => onUrlChange(e.target.value)}
          />
        </InlineField>

        <InlineField
          label="Browser URL"
          labelWidth={20}
          tooltip={
            isCache
              ? "Where users' browsers reach the NetBox the replica mirrors, if different from the address the replica reports. 'View in NetBox' links are rewritten to it. Leave empty if both match."
              : "Where users' browsers reach NetBox, if different from the address Grafana uses (e.g. Grafana connects via an internal service name). 'View in NetBox' links are rewritten to it. Leave empty if both match."
          }
        >
          <Input
            id="config-public-url"
            width={40}
            value={jsonData.publicUrl ?? ''}
            placeholder="(optional) where browsers reach NetBox"
            onChange={(e: ChangeEvent<HTMLInputElement>) => onJsonChange({ publicUrl: e.target.value })}
          />
        </InlineField>

        {isCache && (
          <InlineField
            label="NetBox instance ID"
            labelWidth={20}
            tooltip="The NetBox instance the replica holds (nb-…), sent as the NBC-Netbox-ID header."
          >
            <Input
              required
              id="config-netbox-id"
              width={40}
              value={jsonData.netboxId ?? ''}
              placeholder="nb-…"
              onChange={(e: ChangeEvent<HTMLInputElement>) => onJsonChange({ netboxId: e.target.value })}
            />
          </InlineField>
        )}

        <InlineField
          label="API token"
          labelWidth={20}
          tooltip={isCache ? 'Bearer token for the replica-cache deployment.' : 'NetBox API token (v1 or v2).'}
        >
          <SecretInput
            required
            id="config-api-token"
            width={40}
            isConfigured={Boolean(secureJsonFields?.apiToken)}
            value={secureJsonData?.apiToken ?? ''}
            placeholder={isCache ? 'ff_…' : 'nbt_… or a 40-character token'}
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

        {/* Off by default, and the description says what is given up rather than
            only what is gained: on anything smaller than a few million objects
            this trades two visible behaviours for a saving the user will not
            notice. The counts that alert rules evaluate are NOT affected — an
            alert evaluation never takes this path, whatever shape its query has
            (pkg/plugin/query.go gates it on the FromAlert header, not on the
            Alert table switch) — which is the one thing a reader of this switch
            most needs to be sure of. */}
        {/* NetBox only: the replica-cache backend has no cursor walk to opt
            into, so the switch would be a control that changes nothing. Hidden
            rather than disabled — there is nothing to explain, unlike the
            ordering picker, which is disabled because a NetBox setting really
            is suppressing it. */}
        {!isCache && (
          <InlineField label="Fast paging" labelWidth={20} tooltip={FAST_PAGING_TOOLTIP}>
            <InlineSwitch
              id="config-fast-paging"
              value={Boolean(jsonData.fastPagingNoTotals)}
              onChange={(e) => onJsonChange({ fastPagingNoTotals: e.currentTarget.checked })}
            />
          </InlineField>
        )}

        {/* Replica-cache only: the replica reports how current its data is,
            and this is the one dial on that. NetBox mode reads live data. */}
        {isCache && (
          <InlineField
            label="Max data age for alert rules"
            labelWidth={26}
            tooltip="Optional. A Go duration such as 15m or 2h. When set, alert rules and expression-fed queries refuse results older than this, or whose age the replica cannot report, instead of evaluating a stale inventory. Dashboards only show the age. Leave empty to never refuse on age; a replica still loading its initial snapshot is refused regardless."
          >
            <Input
              id="config-max-data-age"
              width={20}
              value={jsonData.maxDataAge ?? ''}
              placeholder="15m"
              onChange={(e: ChangeEvent<HTMLInputElement>) => onJsonChange({ maxDataAge: e.target.value })}
            />
          </InlineField>
        )}
      </FieldSet>
    </>
  );
}
