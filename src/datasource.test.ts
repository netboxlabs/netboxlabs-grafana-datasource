import { DataSource, prepareAnnotationQuery, scopeFormat } from './datasource';
import { NetBoxQuery } from './types';

const mockReplace = jest.fn((s: string, _scopedVars?: unknown, format?: string | ((value: unknown) => string)) => {
  if (s === '$empty') {
    return '';
  }
  // Variables whose values hold a comma and a backslash, interpolated with the
  // format the caller passes, as Grafana does.
  if (s === '$acme' && typeof format === 'function') {
    return format('Acme, West\\1');
  }
  if (s === '$acmes' && typeof format === 'function') {
    return format(['Acme, West', 'b\\c']);
  }
  if (s === '$type') {
    return 'dcim.interface';
  }
  if (s === '$types') {
    return 'a,b';
  }
  if (s === '$fields') {
    return 'role,status';
  }
  if (s === '$act') {
    return 'create,delete';
  }
  if (s === '$allActs') {
    return 'create,update,delete';
  }
  if (s === '$customAll') {
    return '$__all';
  }
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
});

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({ replace: mockReplace }),
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

  it('degrades for replica-cache, whose wording matches no regex', async () => {
    // The prose fallback was a contract nobody declared: the same condition
    // reads "not found" from NetBox and "Replica cache has no object type ..."
    // from the cache, so a dashboard with a branch variable threw on refresh
    // the moment a second provider existed. The backend sends its
    // classification now, and that is what this reads.
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockRejectedValue({
      status: 400,
      data: {
        error:
          'Replica cache has no object type "plugins/branching/branches" — it isn\'t one of the 74 types this deployment reports.',
        kind: 'unknown-object-type',
      },
    });
    const out = await ds.metricFindQuery({
      refId: 'v',
      objectType: 'plugins/branching/branches',
      valueField: 'schema_id',
      textField: 'name',
    });
    expect(out).toEqual([{ text: 'main', value: 'main' }]);
  });

  it('degrades when the cache reports the endpoint missing', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockRejectedValue({
      status: 502,
      data: { error: 'Replica cache has no such endpoint. Check the replica-cache URL.', kind: 'not-found' },
    });
    const out = await ds.metricFindQuery({
      refId: 'v',
      objectType: 'plugins/branching/branches',
      valueField: 'schema_id',
      textField: 'name',
    });
    expect(out).toEqual([{ text: 'main', value: 'main' }]);
  });

  it('still rethrows a classified failure that is NOT a missing endpoint', async () => {
    // The kind must not become a blanket "degrade on anything classified":
    // an auth failure hidden behind "main" is exactly what this guard prevents.
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockRejectedValue({
      status: 400,
      data: { error: 'Replica cache rejected the credentials.', kind: 'auth' },
    });
    await expect(
      ds.metricFindQuery({
        refId: 'v',
        objectType: 'plugins/branching/branches',
        valueField: 'schema_id',
        textField: 'name',
      })
    ).rejects.toBeDefined();
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
    await expect(ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'name' })).rejects.toThrow(
      'boom'
    );
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

describe('applyTemplateVariables scope', () => {
  it('interpolates the scope with the escaping scope format', () => {
    const ds = makeDS();
    const scopedVars = {};
    ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations', scope: '$device' }, scopedVars);
    expect(mockReplace).toHaveBeenCalledWith('$device', scopedVars, scopeFormat);
  });

  // A NetBox name can hold a comma, which the backend would otherwise split on.
  it('escapes commas and backslashes in variable values', () => {
    const ds = makeDS();
    const one = ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations', scope: '$acme' }, {});
    expect(one.scope).toBe('Acme\\, West\\\\1');
    const many = ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations', scope: '$acmes' }, {});
    expect(many.scope).toBe('Acme\\, West,b\\\\c');
  });

  it('leaves typed text as typed: a typed comma separates', () => {
    const ds = makeDS();
    const out = ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations', scope: 'a, b' }, {});
    expect(out.scope).toBe('a, b');
  });

  // Grafana hands a custom All value through unformatted; were it formatted,
  // $__all holds nothing to escape, so the backend still reads it as all.
  it('keeps $__all recognisable', () => {
    expect(scopeFormat('$__all')).toBe('$__all');
    const ds = makeDS();
    const out = ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations', scope: '$__all' }, {});
    expect(out.scope).toBe('$__all');
  });

  it('sets scopeSet from the raw scope, even when it interpolates to nothing', () => {
    const ds = makeDS();
    const out = ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations', scope: '$empty' }, {});
    expect(out.scope).toBe('');
    expect(out.scopeSet).toBe(true);
    const blank = ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations', scope: '  ' }, {});
    expect(blank.scopeSet).toBe(false);
  });

  it('leaves scope and scopeSet absent when no scope is configured', () => {
    const ds = makeDS();
    const out = ds.applyTemplateVariables({ refId: 'A', queryType: 'annotations' }, {});
    expect(out.scope).toBeUndefined();
    expect(out).not.toHaveProperty('scopeSet');
  });

  // A provisioned dashboard can carry "scope": null; it is no scope, as absent is.
  it('treats a null scope as no scope', () => {
    const ds = makeDS();
    const q = { refId: 'A', queryType: 'annotations', scope: null } as unknown as NetBoxQuery;
    let out: NetBoxQuery | undefined;
    expect(() => {
      out = ds.applyTemplateVariables(q, {});
    }).not.toThrow();
    expect(out?.scope ?? undefined).toBeUndefined();
    expect(out).not.toHaveProperty('scopeSet');
  });

  // A provisioned dashboard can carry a bare id: "scope": 17.
  it('sends a number scope as its string', () => {
    const ds = makeDS();
    const q = { refId: 'A', queryType: 'annotations', scope: 17 } as unknown as NetBoxQuery;
    const out = ds.applyTemplateVariables(q, {});
    expect(out.scope).toBe('17');
    expect(out.scopeSet).toBe(true);
  });
});

describe('applyTemplateVariables scope type, actions and changed fields', () => {
  const anno = (extra: Partial<NetBoxQuery>): NetBoxQuery => ({ refId: 'A', queryType: 'annotations', ...extra });

  it('interpolates the scope type with csv and flags it as set', () => {
    const ds = makeDS();
    const scopedVars = {};
    const out = ds.applyTemplateVariables(anno({ scope: 'x', scopeType: '$type' }), scopedVars);
    expect(mockReplace).toHaveBeenCalledWith('$type', scopedVars, 'csv');
    expect(out.scopeType).toBe('dcim.interface');
    expect(out.scopeTypeSet).toBe(true);
  });

  it('joins a multi-value scope type, for the backend to refuse', () => {
    const out = makeDS().applyTemplateVariables(anno({ scopeType: '$types' }), {});
    expect(out.scopeType).toBe('a,b');
  });

  it('keeps an empty scope type variable set', () => {
    const out = makeDS().applyTemplateVariables(anno({ scopeType: '$empty' }), {});
    expect(out.scopeType).toBe('');
    expect(out.scopeTypeSet).toBe(true);
  });

  it('leaves a blank or absent scope type unset', () => {
    const ds = makeDS();
    expect(ds.applyTemplateVariables(anno({ scopeType: '  ' }), {}).scopeTypeSet).toBe(false);
    expect(ds.applyTemplateVariables(anno({}), {})).not.toHaveProperty('scopeTypeSet');
  });

  it('splits interpolated changed fields, trimmed and deduped', () => {
    const out = makeDS().applyTemplateVariables(anno({ changedFields: ['$fields', ' status '] }), {});
    expect(out.changedFields).toEqual(['role', 'status']);
  });

  it('splits interpolated actions', () => {
    const out = makeDS().applyTemplateVariables(anno({ actions: ['$act'] }), {});
    expect(out.actions).toEqual(['create', 'delete']);
  });

  it('drops an entry that interpolates to nothing, and the list when none is left', () => {
    const ds = makeDS();
    expect(ds.applyTemplateVariables(anno({ actions: ['$empty', 'update'] }), {}).actions).toEqual(['update']);
    expect(ds.applyTemplateVariables(anno({ changedFields: ['$empty'] }), {}).changedFields).toBeUndefined();
  });

  it('passes all three actions on, which the backend reads as no filter', () => {
    const out = makeDS().applyTemplateVariables(anno({ actions: ['$allActs'] }), {});
    expect(out.actions).toEqual(['create', 'update', 'delete']);
  });

  // guard: a custom All value reaches the backend as typed, which reads it as no filter.
  it('passes a custom All value through', () => {
    const out = makeDS().applyTemplateVariables(anno({ actions: ['$customAll'], changedFields: ['$customAll'] }), {});
    expect(out.actions).toEqual(['$__all']);
    expect(out.changedFields).toEqual(['$__all']);
  });
});

describe('prepareAnnotationQuery', () => {
  it('carries the scope, filter and journal fields from the target', () => {
    const out = prepareAnnotationQuery({
      target: {
        scopeType: 'dcim.device',
        scope: '$device',
        scopeMatch: 'id',
        actions: ['update'],
        changedFields: ['status'],
        journal: true,
        includeDescendants: false,
      },
    });
    expect(out).toEqual(
      expect.objectContaining({
        scopeType: 'dcim.device',
        scope: '$device',
        scopeMatch: 'id',
        actions: ['update'],
        changedFields: ['status'],
        journal: true,
        includeDescendants: false,
      })
    );
  });

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
    // Both topology shapes scope themselves with filters, not an object type.
    // Requiring one dropped every newly created edges query in the frontend,
    // before the backend ever saw it — the query simply did nothing.
    expect(ds.filterQuery({ refId: 'A', queryType: 'topology-edges' })).toBe(true);
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ips: '10.0.0.1' })).toBe(true);
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ips: '' })).toBe(false);
    expect(ds.filterQuery({ refId: 'A', objectType: 'dcim/devices' })).toBe(true);
    expect(ds.filterQuery({ refId: 'A' })).toBe(false);
  });

  it('runs a scope-sourced ip-enrichment query once it has a filter row, and never a list-sourced one without IPs', () => {
    const ds = makeDS();
    const parent = [{ field: 'parent', operator: '', value: '10.0.0.0/24' }];
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ipSource: 'scope', filters: parent })).toBe(true);
    expect(
      ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ipSource: 'scope', ips: '', filters: parent })
    ).toBe(true);
    // No filter row yet: not run, or it would list the whole address table.
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ipSource: 'scope' })).toBe(false);
    expect(
      ds.filterQuery({
        refId: 'A',
        queryType: 'ip-enrichment',
        ipSource: 'scope',
        filters: [{ field: '', operator: '', value: '' }],
      })
    ).toBe(false);
    // A field with no value yet is dropped by the backend, so it narrows nothing either.
    const blank = [{ field: 'parent', operator: '', value: '' }];
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ipSource: 'scope', filters: blank })).toBe(false);
    // An empty-family operator needs no value.
    const isEmpty = [{ field: 'tenant', operator: 'empty', value: '' }];
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ipSource: 'scope', filters: isEmpty })).toBe(true);
    expect(ds.filterQuery({ refId: 'A', queryType: 'ip-enrichment', ipSource: 'list', ips: '' })).toBe(false);
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

describe('metricFindQuery degradation', () => {
  // A degraded result is not a shorter list, it is a DIFFERENT one: a lookup
  // that could not read some site names produces a variable missing those
  // options, which silently rescopes every panel that depends on it.
  it('refuses a degraded variable list instead of offering a partial one', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({
      columns: ['site'],
      rows: [{ site: 'DC-Northeast' }],
      warnings: ['Related names from dcim/sites are missing for 3 of the 5 objects referenced here.'],
    });
    await expect(
      ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'site' })
    ).rejects.toThrow(/incomplete list/i);
  });

  it('returns the list when nothing was degraded', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest
      .fn()
      .mockResolvedValue({ columns: ['site'], rows: [{ site: 'DC-Northeast' }] });
    expect(await ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'site' })).toEqual([
      { text: 'DC-Northeast', value: 'DC-Northeast' },
    ]);
  });
});

describe('metricFindQuery truncation', () => {
  // Measured on a 6.8M-device instance: a `site` variable over devices reads
  // the first 1,000 devices and finds ONE distinct site out of 4,030 that
  // exist. Every panel scoped by it would show a single site while looking
  // like the whole estate.
  it('refuses a list that did not cover the match count', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({
      columns: ['site'],
      rows: [{ site: 'DC-Northeast' }],
      total: 6824570,
    });
    await expect(
      ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'site' })
    ).rejects.toThrow(/may be missing values/i);
  });

  it('names what to do about it', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest
      .fn()
      .mockResolvedValue({ columns: ['site'], rows: [{ site: 'A' }], total: 4030 });
    await expect(
      ds.metricFindQuery({ refId: 'v', objectType: 'dcim/devices', valueField: 'site' })
    ).rejects.toThrow(/Query the object type that owns this field/i);
  });

  it('accepts a list that covers every matching object', async () => {
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({
      columns: ['name'],
      rows: [{ name: 'a' }, { name: 'b' }],
      total: 2,
    });
    expect(await ds.metricFindQuery({ refId: 'v', objectType: 'dcim/sites', valueField: 'name' })).toEqual([
      { text: 'a', value: 'a' },
      { text: 'b', value: 'b' },
    ]);
  });

  it('accepts a source that reports no total at all', async () => {
    // Result.Total is documented as 0 when the source cannot report one, so an
    // absent total must not be read as "zero matches, therefore truncated".
    const ds = makeDS();
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({ columns: ['name'], rows: [{ name: 'a' }] });
    expect(await ds.metricFindQuery({ refId: 'v', objectType: 'dcim/sites', valueField: 'name' })).toEqual([
      { text: 'a', value: 'a' },
    ]);
  });
});

describe('branch variable in replica-cache mode', () => {
  // The cache mirrors the main dataset only and refuses a branch-scoped query
  // outright, so if it happens to replicate the branches table every schema it
  // listed would be an option that breaks every panel selecting it.
  it('offers only main, without relying on the endpoint being absent', async () => {
    const ds = makeDS();
    (ds as any).datasourceInstanceSettings = { jsonData: { mode: 'replica-cache' } };
    const run = jest.fn().mockResolvedValue({
      columns: ['schema_id', 'name'],
      rows: [{ schema_id: 'schema_abc', name: 'feature-x' }],
      total: 1,
    });
    (ds as any).runResourceQuery = run;

    const out = await ds.metricFindQuery({
      refId: 'v',
      objectType: 'plugins/branching/branches',
      valueField: 'schema_id',
      textField: 'name',
    });
    expect(out).toEqual([{ text: 'main', value: 'main' }]);
    // And it does not even ask: the answer cannot change what is offerable.
    expect(run).not.toHaveBeenCalled();
  });

  it('still lists real branches in NetBox mode', async () => {
    const ds = makeDS();
    (ds as any).datasourceInstanceSettings = { jsonData: {} };
    (ds as any).runResourceQuery = jest.fn().mockResolvedValue({
      columns: ['schema_id', 'name'],
      rows: [{ schema_id: 'schema_abc', name: 'feature-x' }],
      total: 1,
    });
    const out = await ds.metricFindQuery({
      refId: 'v',
      objectType: 'plugins/branching/branches',
      valueField: 'schema_id',
      textField: 'name',
    });
    expect(out).toEqual([
      { text: 'main', value: 'main' },
      { text: 'feature-x (schema_abc)', value: 'schema_abc' },
    ]);
  });
});
