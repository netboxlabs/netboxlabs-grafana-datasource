import { test, expect } from '@grafana/plugin-e2e';

test('smoke: should render query editor', async ({ panelEditPage, readProvisionedDataSource }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  // The object-type selector is the entry point of the NetBox query editor.
  await expect(panelEditPage.getQueryEditorRow('A').getByText('Object type', { exact: true })).toBeVisible();
});
