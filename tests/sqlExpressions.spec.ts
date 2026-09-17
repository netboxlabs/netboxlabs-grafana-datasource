import { test, expect } from '@grafana/plugin-e2e';
import { readFile } from 'node:fs/promises';
import { lt } from 'semver';

import { deleteDashboard, importDashboard } from './fixtures/dashboards';

// The SQL-expression path: NetBox inventory joined to a metrics table inside
// Grafana. Nothing else guards it, and it is where a truncated NetBox result does
// the most damage — the expression drops the frame's notices, so the panel shows a
// plausible wrong number. TestData's CSV scenario stands in for Prometheus; the
// e2e stack runs no telemetry services.
//
// The alert rule is e2e-only (docker-compose.e2e.yaml mounts it into its own
// provisioning root); the dashboard is imported per spec and removed after.
test.use({ provisioningRootDir: 'e2e/provisioning' });
const DASHBOARD = 'netbox-sql-join.json';

// The rows the NetBox stub serves (e2e/netbox-stub/main.py): leaf1 and leaf2, both
// at site dc1. "ghost9" in the CSV matches nothing and must fall out of the join.
const JOINED = ['leaf1', '41', 'dc1', 'leaf2', '87', 'dc1'];

test.beforeEach(async ({ grafanaVersion }) => {
  test.skip(lt(grafanaVersion, '12.3.0'), 'SQL expressions are not available before the minimum supported version');
});

test('sql expression: a panel joins a metrics table to NetBox inventory', async ({ gotoPanelEditPage, request }) => {
  const dashboard = await importDashboard(request, DASHBOARD);
  const panelEditPage = await gotoPanelEditPage({ dashboard, id: '1' });
  const response = await panelEditPage.refreshPanel();
  expect(response.ok()).toBe(true);
  // What the truncation refusal below hangs on: Grafana marks the queries of a
  // panel that has expressions. If a Grafana release stops sending this, the
  // datasource can no longer tell and the fix silently stops applying.
  expect(response.request().headers()['x-grafana-from-expr']).toBe('true');
  await expect(panelEditPage.panel.fieldNames).toContainText(['device', 'cpu', 'site']);
  await expect(panelEditPage.panel.data).toContainText(JOINED);
  await deleteDashboard(request, dashboard.uid);
});

// The regression this file exists for. Grafana's frontend marks every query of a
// panel that has expressions with X-Grafana-From-Expr, and a backend posting to
// /api/ds/query can do the same. With it, a NetBox input cut off by its row limit
// fails the join instead of feeding it a subset.
test('sql expression: a truncated NetBox input fails the join instead of feeding it a subset', async ({ request }) => {
  // The panel's queries, sent as the browser would; the dashboard itself is not needed.
  const dashboard = JSON.parse(await readFile(`e2e/dashboards/${DASHBOARD}`, 'utf8'));
  const panel = dashboard.panels[0] as { targets: Array<Record<string, unknown>> };
  // Unhidden so each query's own result comes back, and the stub's two devices cut to one.
  const queries = panel.targets.map((t) => ({ ...t, hide: false, ...(t.refId === 'NB' ? { limit: 1 } : {}) }));
  const body = { from: 'now-15m', to: 'now', queries };

  // Asserted on the per-query results, not the HTTP status: whether a response
  // that mixes a success (CPU) with errors is a 4xx or a 207 is Grafana's policy
  // and differs by version.
  const strict = await request.post('/api/ds/query', { data: body, headers: { 'X-Grafana-From-Expr': 'true' } });
  const results = (await strict.json()).results;
  expect(results.NB.error).toContain('1 of 2 matching objects');
  expect(results.NB.error).toContain('expression');
  expect(results.J.error).toBeTruthy();
  expect(results.J.frames ?? []).toHaveLength(0);

  // The same NetBox query on its own is an ordinary dashboard query: partial
  // beats none, with the gap stated in a notice.
  const plain = await request.post('/api/ds/query', { data: { ...body, queries: [queries[1]] } });
  expect(plain.ok()).toBe(true);
  const frame = (await plain.json()).results.NB.frames[0];
  expect(frame.data.values[0]).toHaveLength(1);
  expect(JSON.stringify(frame.schema.meta.notices)).toContain('Showing 1 of 2');
});

// An alert rule whose condition is the same join. Health and lastError are the
// assertion, not firing state: whether leaf2 is over the threshold is the rule's
// business, whether the rule can evaluate at all is ours. A broken join shows up
// here as health "error" with the reason in lastError, and nowhere else.
test('sql expression: an alert rule evaluates the join', async ({ readProvisionedAlertRule, request }) => {
  // The first evaluation can be a full rule interval away on a fresh stack.
  test.setTimeout(90_000);
  const alertRule = await readProvisionedAlertRule({ fileName: 'netbox-sql-join.yml' });

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
        // A rule that has never run reports health "ok" with a zero timestamp, so
        // wait for a real evaluation before reading anything into it.
        if (!rule?.lastEvaluation || rule.lastEvaluation.startsWith('0001-')) {
          return undefined;
        }
        return {
          health: rule.health,
          lastError: rule.lastError ?? '',
          // The join's NetBox column arriving as an instance label is what shows
          // the rule evaluated the joined table and not just one side of it.
          sites: (rule.alerts ?? []).map((a) => `${a.labels.device}@${a.labels.site}`),
        };
      },
      { timeout: 60_000, intervals: [2_000] }
    )
    .toEqual({ health: 'ok', lastError: '', sites: ['leaf2@dc1'] });
});
