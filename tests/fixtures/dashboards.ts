import { readFile } from 'node:fs/promises';
import { APIRequestContext } from '@playwright/test';

// Imports a dashboard from ./e2e/dashboards for one spec and removes it after.
// The e2e dashboards are not provisioned, so no dev or demo stack ever lists
// them; they are fixtures, not product.
export async function importDashboard(request: APIRequestContext, fileName: string): Promise<{ uid: string }> {
  const dashboard = JSON.parse(await readFile(`e2e/dashboards/${fileName}`, 'utf8'));
  const res = await request.post('/api/dashboards/db', { data: { dashboard, overwrite: true } });
  if (!res.ok()) {
    throw new Error(`import ${fileName}: ${res.status()} ${await res.text()}`);
  }
  return { uid: dashboard.uid };
}

export async function deleteDashboard(request: APIRequestContext, uid: string): Promise<void> {
  await request.delete(`/api/dashboards/uid/${uid}`);
}
