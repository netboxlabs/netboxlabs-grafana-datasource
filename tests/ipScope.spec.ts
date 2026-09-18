import { test, expect } from '@grafana/plugin-e2e';
import { lt } from 'semver';

import { deleteDashboard, importDashboard } from './fixtures/dashboards';

// The scope source of ip-enrichment: one row per address NetBox holds in a
// prefix, joined against a flow table's source IP in a SQL expression. This is
// the alert-rule path, so the rule is the assertion that matters; the panel
// shows the same join where a person can read it.
test.use({ provisioningRootDir: 'e2e/provisioning' });

test.beforeEach(async ({ grafanaVersion }) => {
  test.skip(lt(grafanaVersion, '12.3.0'), 'SQL expressions are not available before the minimum supported version');
});

test('ip scope: a panel joins flows to every address in a NetBox prefix', async ({ gotoPanelEditPage, request }) => {
  const dashboard = await importDashboard(request, 'netbox-ip-scope.json');
  const panelEditPage = await gotoPanelEditPage({ dashboard, id: '1' });
  await expect(panelEditPage.refreshPanel()).toBeOK();
  await expect(panelEditPage.panel.fieldNames).toContainText(['src_ip', 'bps', 'device', 'interface']);
  // 10.0.0.1 and .2 are leaf1's; 203.0.113.7 is outside the scope and survives
  // the LEFT JOIN with blank NetBox columns instead of failing the expression.
  await expect(panelEditPage.panel.data).toContainText(['10.0.0.1', '500', 'leaf1', 'Ethernet1']);
  await expect(panelEditPage.panel.data).toContainText(['10.0.0.2', '300', 'leaf1', 'Loopback0']);
  await expect(panelEditPage.panel.data).toContainText(['203.0.113.7', '900']);
  await deleteDashboard(request, dashboard.uid);
});

test('ip scope: the scope query alone lists the prefix, one row per address', async ({ request }) => {
  const res = await request.post('/api/ds/query', {
    data: {
      from: 'now-15m',
      to: 'now',
      queries: [
        {
          refId: 'IPS',
          datasource: { uid: 'netboxlabs-netbox' },
          queryType: 'ip-enrichment',
          ipSource: 'scope',
          filters: [{ field: 'parent', operator: '', value: '10.0.0.0/24' }],
          contextFields: ['ip', 'device_name', 'is_primary_ip'],
          limit: 100,
        },
      ],
    },
  });
  expect(res.ok()).toBe(true);
  const frame = (await res.json()).results.IPS.frames[0];
  const names: string[] = frame.schema.fields.map((f: { name: string }) => f.name);
  const col = (n: string) => frame.data.values[names.indexOf(n)];
  expect(col('ip')).toEqual(['10.0.0.1', '10.0.0.2', '10.0.0.9']);
  // String columns are non-nullable ('' for no device), booleans are nullable.
  expect(col('device_name')).toEqual(['leaf1', 'leaf1', '']);
  expect(col('is_primary_ip')).toEqual([true, false, null]);
});

test('ip scope: an alert rule evaluates the scope join', async ({ readProvisionedAlertRule, request }) => {
  test.setTimeout(90_000);
  const alertRule = await readProvisionedAlertRule({ fileName: 'netbox-ip-scope-join.yml' });
  type Rule = {
    uid: string;
    health: string;
    lastError?: string;
    lastEvaluation?: string;
    alerts?: Array<{ labels: Record<string, string> }>;
  };
  await expect
    .poll(
      async () => {
        const res = await request.get('/api/prometheus/grafana/api/v1/rules', { params: { rule_uid: alertRule.uid } });
        const groups: Array<{ rules: Rule[] }> = (await res.json()).data?.groups ?? [];
        const rule = groups.flatMap((g) => g.rules).find((r) => r.uid === alertRule.uid);
        if (!rule?.lastEvaluation || rule.lastEvaluation.startsWith('0001-')) {
          return undefined;
        }
        return {
          health: rule.health,
          lastError: rule.lastError ?? '',
          // The one flow outside the prefix is the one instance.
          outside: (rule.alerts ?? []).map((a) => a.labels.src_ip),
        };
      },
      { timeout: 60_000, intervals: [2_000] }
    )
    .toEqual({ health: 'ok', lastError: '', outside: ['203.0.113.7'] });
});
