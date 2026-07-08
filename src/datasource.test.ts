import { DataSource } from './datasource';
import { NetBoxQuery } from './types';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({
    replace: (s: string) => (s === '$site' ? 'dc1,dc2' : s),
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
