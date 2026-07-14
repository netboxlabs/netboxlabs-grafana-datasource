import { test, expect } from '@grafana/plugin-e2e';

// Proves the NetBox data source is consumable by a Grafana-managed alert rule:
// the provisioned object-count rule (query (count) -> Threshold) evaluates and the
// data source response is shaped correctly. Requires a running stack with a
// reachable NetBox (npm run server with NETBOX_URL/NETBOX_API_TOKEN set).
test('alerting: NetBox object-count rule evaluates', async ({
  readProvisionedAlertRule,
  gotoAlertRuleEditPage,
}) => {
  const alertRule = await readProvisionedAlertRule({ fileName: 'netbox-object-count.yml' });
  const alertRuleEditPage = await gotoAlertRuleEditPage(alertRule);
  await expect(alertRuleEditPage.evaluate()).toBeOK();
});

// Proves the alertTable-shaped rule also evaluates: the provisioned enriched
// rule (query (alertTable) -> Threshold) reshapes offline devices into one
// alert instance per row, carrying NetBox context as labels.
test('alerting: NetBox alert-enriched rule evaluates', async ({
  readProvisionedAlertRule,
  gotoAlertRuleEditPage,
}) => {
  const alertRule = await readProvisionedAlertRule({ fileName: 'netbox-alert-enriched.yml' });
  const alertRuleEditPage = await gotoAlertRuleEditPage(alertRule);
  await expect(alertRuleEditPage.evaluate()).toBeOK();
});
