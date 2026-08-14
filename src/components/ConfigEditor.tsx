import React, { ChangeEvent, useEffect } from 'react';
import { InlineField, Input, SecretInput, InlineSwitch, FieldSet } from '@grafana/ui';
import { DataSourcePluginOptionsEditorProps } from '@grafana/data';
import { NetBoxDataSourceOptions, NetBoxSecureJsonData } from '../types';

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

  return (
    <>
      <FieldSet label="Connection">
        <InlineField label="NetBox URL" labelWidth={20} tooltip="Base URL of the NetBox instance, without /api">
          <Input
            id="config-url"
            width={40}
            value={jsonData.url ?? ''}
            placeholder="https://netbox.example.com"
            onChange={(e: ChangeEvent<HTMLInputElement>) => onUrlChange(e.target.value)}
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

        {/* Off by default, and the description says what is given up rather than
            only what is gained: on anything smaller than a few million objects
            this trades two visible behaviours for a saving the user will not
            notice. The counts that alert rules evaluate are NOT affected — an
            alert evaluation never takes this path, whatever shape its query has
            (pkg/plugin/query.go gates it on the FromAlert header, not on the
            Alert table switch) — which is the one thing a reader of this switch
            most needs to be sure of. */}
        <InlineField label="Fast paging" labelWidth={20} tooltip={FAST_PAGING_TOOLTIP}>
          <InlineSwitch
            id="config-fast-paging"
            value={Boolean(jsonData.fastPagingNoTotals)}
            onChange={(e) => onJsonChange({ fastPagingNoTotals: e.currentTarget.checked })}
          />
        </InlineField>
      </FieldSet>
    </>
  );
}
