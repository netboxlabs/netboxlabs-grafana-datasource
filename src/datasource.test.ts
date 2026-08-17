import { DataSource, prepareAnnotationQuery } from './datasource';
import { NetBoxQuery } from './types';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({
    replace: (s: string) => {
      if (s === '$site') {
        return 'dc1,dc2';
      }
      if (s === '$flow_ips') {
        return '10.112.128.1,203.0.113.7';
      }
      if (s === '$branch') {
        return 'td5smq0f';
      }
      return s;
    },
  }),
}));

// Build a DataSource without invoking the backend constructor.
function makeDS(): DataSource {
  const ds = Object.create(DataSource.prototype) as DataSource;
  return ds;
}

describe('metricFindQuery', () => {
  it('maps rows to unique {text,value} pairs', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({
      columns: ['name'],
      rows: [{ name: 'leaf1' }, { name: 'leaf1' }, { name: 'leaf2' }],
    });
    const out = await ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'name' });
    expect(out).toEqual([
      { text: 'leaf1', value: 'leaf1' },
      { text: 'leaf2', value: 'leaf2' },
    ]);
  });

  it('returns [] when no objectType', async () => {
    const ds = makeDS();
    expect(await ds.metricFindQuery({ refId: 'v' } as any)).toEqual([]);
  });

  it('passes the interpolated branch to the resource query', async () => {
    const ds = makeDS();
    const spy = jest.fn().mockResolvedValue({ columns: ['name'], rows: [{ name: 'leaf1' }] });
    (ds as any).runResourceQuery = spy;
    await ds.metricFindQuery({ refId: 'A', objectType: 'dcim/devices', branch: '$branch' } as any, {});
    expect(spy.mock.calls[0][0]).toEqual(expect.objectContaining({ branch: 'td5smq0f' }));
  });

  it('prepends a selectable "main" for the branch variable', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({
      columns: ['schema_id', 'name'],
      rows: [{ schema_id: 'kc4v9jtd', name: 'demo-branch' }],
    });
    const out = await ds.metricFindQuery({
      refId: 'v',
      objectType: 'plugins/branching/branches',
      valueField: 'schema_id',
      textField: 'name',
    });
    expect(out).toEqual([
      { text: 'main', value: 'main' },
      { text: 'demo-branch (kc4v9jtd)', value: 'kc4v9jtd' },
    ]);
  });

  it('degrades to just "main" for the branch variable when branching is not installed', async () => {
    const ds = makeDS();
    // Backend maps a NetBox 404 to a 502 with a "not found" message.
    (ds as any).runResourceQuery = jest
      .fn()
      .mockRejectedValue({ status: 502, data: { error: 'This object type was not found in NetBox (HTTP 404).' } });
    const out = await ds.metricFindQuery({
      refId: 'v',
      objectType: 'plugins/branching/branches',
      valueField: 'schema_id',
      textField: 'name',
    });
    expect(out).toEqual([{ text: 'main', value: 'main' }]);
  });

  it('rethrows a real branch-list failure (outage/auth) instead of hiding it behind "main"', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest
      .fn()
      .mockRejectedValue({ status: 502, data: { error: "Couldn't reach NetBox: dial tcp: connection refused" } });
    await expect(
      ds.metricFindQuery({
        refId: 'v',
        objectType: 'plugins/branching/branches',
        valueField: 'schema_id',
        textField: 'name',
      })
    ).rejects.toBeDefined();
  });

  it('does not prepend "main" for non-branch variables', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({ columns: ['name'], rows: [{ name: 'leaf1' }] });
    const out = await ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'name' });
    expect(out).toEqual([{ text: 'leaf1', value: 'leaf1' }]);
  });

  it('re-throws (does not swallow) errors for non-branch variables', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockRejectedValue(new Error('boom'));
    await expect(ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'name' })).rejects.toThrow('boom');
  });

  it('suppresses the global error toast for the branch probe (branching may be absent -> expected 404)', async () => {
    const ds = makeDS();
    const spy = jest.fn().mockResolvedValue({ columns: ['schema_id', 'name'], rows: [] });
    (ds as any).runResourceQuery = spy;
    await ds.metricFindQuery({
      refId: 'v',
      objectType: 'plugins/branching/branches',
      valueField: 'schema_id',
      textField: 'name',
    });
    expect(spy).toHaveBeenCalledWith(expect.anything(), { showErrorAlert: false });
  });

  it('does NOT suppress the error toast for non-branch variables', async () => {
    const ds = makeDS();
    const spy = jest.fn().mockResolvedValue({ columns: ['name'], rows: [] });
    (ds as any).runResourceQuery = spy;
    await ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'name' });
    expect(spy.mock.calls[0][1]?.showErrorAlert).not.toBe(false);
  });
});

describe('applyTemplateVariables', () => {
  it('interpolates multi-value variables into filter values (csv)', () => {
    const ds = makeDS();
    const q: NetBoxQuery = {
      refId: 'A',
      objectType: 'dcim/devices',
      filters: [{ field: 'site', operator: '', value: '$site' }],
    };
    const out = ds.applyTemplateVariables(q, {});
    expect(out.filters![0].value).toBe('dc1,dc2');
  });

  it('interpolates variables in the ip-enrichment ips field (csv)', () => {
    const ds = makeDS();
    const q: NetBoxQuery = { refId: 'A', queryType: 'ip-enrichment', ips: '$flow_ips' };
    const out = ds.applyTemplateVariables(q, {});
    expect(out.ips).toBe('10.112.128.1,203.0.113.7');
  });

  it('interpolates the branch variable', () => {
    const ds = makeDS();
    const out = ds.applyTemplateVariables(
      { refId: 'A', queryType: 'objects', objectType: 'dcim/devices', branch: '$branch' } as any,
      {}
    );
    expect(out.branch).toBe('td5smq0f');
  });
});

describe('prepareAnnotationQuery', () => {
  it('carries the branch from the annotation target', () => {
    const out = prepareAnnotationQuery({ target: { objectTypes: ['dcim.device'], branch: 'td5smq0f' } });
    expect(out.branch).toBe('td5smq0f');
    expect(out.queryType).toBe('annotations');
  });
});

describe('filterQuery', () => {
  it('runs annotation/topology/objects/ip queries, skips empty', () => {
    const ds = makeDS();
    expect(ds.filterQuery({ refId: 'A', queryType: 'annotations' })).toBe(true);
    expect(ds.filterQuery({ refId: 'A', queryType: 'topology' })).toBe(true);
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ips: '10.0.0.1' })).toBe(true);
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ips: '' })).toBe(false);
    expect(ds.filterQuery({ refId: 'A', objectType: 'dcim/devices' })).toBe(true);
    expect(ds.filterQuery({ refId: 'A' })).toBe(false);
  });
});

describe('resource helpers', () => {
  it('getFilterFields calls the /filter-fields resource with type and branch', async () => {
    const ds = makeDS();
    const spy = jest.fn().mockResolvedValue([{ name: 'status', operators: ['', 'ic'] }]);
    (ds as any).getResource = spy;
    const out = await ds.getFilterFields('ipam/prefixes', 'td5smq0f');
    expect(spy).toHaveBeenCalledWith('filter-fields', { type: 'ipam/prefixes', branch: 'td5smq0f' });
    expect(out[0].operators).toEqual(['', 'ic']);
  });

  it('runResourceQuery forwards request options (e.g. showErrorAlert) to postResource', async () => {
    const ds = makeDS();
    const spy = jest.fn().mockResolvedValue({ columns: [], rows: [] });
    (ds as any).postResource = spy;
    await ds.runResourceQuery({ objectType: 'dcim/devices' }, { showErrorAlert: false });
    expect(spy).toHaveBeenCalledWith('query', { objectType: 'dcim/devices' }, { showErrorAlert: false });
  });

  it('getBranchingInstalled returns the installed flag and suppresses the error toast', async () => {
    const ds = makeDS();
    const spy = jest.fn().mockResolvedValue({ installed: false });
    (ds as any).getResource = spy;
    expect(await ds.getBranchingInstalled()).toBe(false);
    expect(spy).toHaveBeenCalledWith('branching', undefined, { showErrorAlert: false });
  });

  it('getBranchingInstalled fails open (true) when the response omits installed', async () => {
    const ds = makeDS();
    (ds as any).getResource = jest.fn().mockResolvedValue({});
    expect(await ds.getBranchingInstalled()).toBe(true);
  });
});
