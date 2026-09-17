import { test, expect } from '@grafana/plugin-e2e';

import { deleteDashboard, importDashboard } from './fixtures/dashboards';

// A variable whose Custom all value is $__all asks for "no filter" when All is
// selected, instead of every option spelled out as a repeated NetBox parameter.
// Two things have to hold for that, and only a browser can show the first: the
// token has to survive Grafana's interpolation and reach the backend as typed.
test('variables: a Custom all value of $__all reaches the backend and drops the filter', async ({
  gotoPanelEditPage,
  request,
}) => {
  const dashboard = await importDashboard(request, 'netbox-all-variable.json');
  const panelEditPage = await gotoPanelEditPage({ dashboard, id: '1' });
  const response = await panelEditPage.refreshPanel();
  expect(response.ok()).toBe(true);

  const sent = response.request().postDataJSON().queries[0].filters;
  expect(sent).toEqual([{ field: 'site', operator: '', value: '$__all' }]);

  // Sent on to NetBox as site=$__all it would match nothing; dropped, the panel
  // lists the stub's whole inventory.
  await expect(panelEditPage.panel.data).toContainText(['leaf1', 'dc1', 'leaf2', 'dc1']);
  await deleteDashboard(request, dashboard.uid);
});
