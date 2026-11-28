import { test, expect } from '@grafana/plugin-e2e';

test('smoke: should render config editor', async ({ createDataSourceConfigPage, page }) => {
  await createDataSourceConfigPage({ type: 'netboxlabs-netbox-datasource' });
  await expect(page.getByLabel('URL', { exact: true })).toBeVisible();
  await expect(page.getByText('API token')).toBeVisible();
});

test('"Save & test" should fail when the API token is missing', async ({ createDataSourceConfigPage, page }) => {
  const configPage = await createDataSourceConfigPage({ type: 'netboxlabs-netbox-datasource' });
  await page.getByPlaceholder('https://netbox.example.com').fill('https://netbox.example.com');
  await expect(configPage.saveAndTest()).not.toBeOK();
  await expect(configPage).toHaveAlert('error', { hasText: 'token is missing' });
});
