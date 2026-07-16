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
    expect(spy).toHaveBeenCalledWith(expect.objectContaining({ branch: 'td5smq0f' }));
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
