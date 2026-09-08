import { CoreApp, DataSourceInstanceSettings, MetricFindValue, ScopedVars } from '@grafana/data';
import { DataSourceWithBackend, getTemplateSrv, type BackendSrvRequest } from '@grafana/runtime';

import {
  DEFAULT_QUERY,
  FieldOption,
  FilterField,
  FilterRow,
  NetBoxDataSourceOptions,
  NetBoxQuery,
  NetBoxVariableQuery,
  ObjectTypeOption,
} from './types';
import { NetBoxVariableSupport } from './variables';
import { AnnotationQueryEditor } from './components/AnnotationQueryEditor';

/**
 * Builds the NetBoxQuery sent for an annotation query from the annotation's target.
 * Exported standalone (rather than kept as a constructor closure) so it can be unit-tested
 * directly. `branch` is carried raw here — DataSourceWithBackend.query() runs
 * applyTemplateVariables (which interpolates it) on annotation targets before dispatch.
 */
export function prepareAnnotationQuery(anno: any): NetBoxQuery {
  const target: Partial<NetBoxQuery> = anno?.target ?? {};
  return {
    refId: target.refId || 'Annotation',
    queryType: 'annotations',
    objectTypes: target.objectTypes ?? [],
    limit: target.limit,
    branch: target.branch,
  };
}

export class DataSource extends DataSourceWithBackend<NetBoxQuery, NetBoxDataSourceOptions> {
  constructor(instanceSettings: DataSourceInstanceSettings<NetBoxDataSourceOptions>) {
    super(instanceSettings);

    // Query-driven dashboard variables (device/site/role/tenant dropdowns).
    this.variables = new NetBoxVariableSupport(this);

    // Annotations from the NetBox change log. The backend returns frames whose
    // fields follow Grafana's time/title/text/tags convention.
    this.annotations = {
      QueryEditor: AnnotationQueryEditor,
      prepareQuery: prepareAnnotationQuery,
    };
  }

  getDefaultQuery(_: CoreApp): Partial<NetBoxQuery> {
    return DEFAULT_QUERY;
  }

  /** Skip executing incomplete queries (saves a backend round trip). */
  filterQuery(query: NetBoxQuery): boolean {
    switch (query.queryType) {
      case 'annotations':
      case 'topology':
      case 'topology-edges':
        // Both topology shapes scope themselves with filters rather than an
        // object type, so requiring one would drop every newly created query
        // before it reached the backend.
        return true;
      case 'ip-enrichment':
        return !!query.ips && query.ips.trim().length > 0;
      default:
        return !!query.objectType;
    }
  }

  /** Interpolate dashboard variables into filter values before querying. */
  applyTemplateVariables(query: NetBoxQuery, scopedVars: ScopedVars): NetBoxQuery {
    const srv = getTemplateSrv();
    const filters = (query.filters ?? []).map((f) => ({
      ...f,
      // 'csv' so multi-value variables become comma-joined OR filters, which the
      // backend expands into repeated NetBox query params.
      value: srv.replace(f.value ?? '', scopedVars, 'csv'),
    }));
    return {
      ...query,
      filters,
      ips: query.ips === undefined ? undefined : srv.replace(query.ips, scopedVars, 'csv'),
      branch: query.branch === undefined ? undefined : srv.replace(query.branch, scopedVars),
    };
  }

  // --- Resource helpers (backed by the Go CallResource router) ---

  getObjectTypes(): Promise<ObjectTypeOption[]> {
    return this.getResource('object-types');
  }

  getFields(objectType: string, branch?: string): Promise<FieldOption[]> {
    return this.getResource('fields', { type: objectType, ...(branch ? { branch } : {}) });
  }

  getFieldValues(objectType: string, field: string, q = '', branch?: string): Promise<string[]> {
    return this.getResource('field-values', { type: objectType, field, q, ...(branch ? { branch } : {}) });
  }

  getFilterFields(objectType: string, branch?: string): Promise<FilterField[]> {
    return this.getResource('filter-fields', { type: objectType, ...(branch ? { branch } : {}) });
  }

  /**
   * Reports whether the netbox-branching plugin is installed on the connected
   * NetBox. Backed by the /branching resource, which always answers 200 with
   * {installed} and fails open (installed:true) on an inconclusive upstream
   * probe. showErrorAlert:false so a backend/plugin hiccup can't pop a global
   * toast; a malformed/missing response also defaults to enabled (fail open).
   */
  getBranchingInstalled(): Promise<boolean> {
    return this.getResource<{ installed?: boolean }>('branching', undefined, { showErrorAlert: false }).then(
      (r) => r?.installed ?? true
    );
  }

  runResourceQuery(
    body: {
      objectType: string;
      filters?: FilterRow[];
      fields?: string[];
      limit?: number;
      branch?: string;
    },
    options?: Partial<BackendSrvRequest>
  ): Promise<{ columns: string[]; rows: Array<Record<string, unknown>> }> {
    return this.postResource('query', body, options);
  }

  /** Populate a dashboard variable from a NetBox object field. */
  async metricFindQuery(query: NetBoxVariableQuery, options?: { scopedVars?: ScopedVars }): Promise<MetricFindValue[]> {
    if (!query?.objectType) {
      return [];
    }
    const srv = getTemplateSrv();
    const filters = (query.filters ?? []).map((f) => ({
      ...f,
      value: srv.replace(f.value ?? '', options?.scopedVars, 'csv'),
    }));
    const valueField = query.valueField || 'name';
    const textField = query.textField || valueField;
    const fields = Array.from(new Set([valueField, textField]));
    const branch = query.branch ? srv.replace(query.branch, options?.scopedVars) : undefined;

    // The branch selector always offers "main" (the default branch, sent as no
    // X-NetBox-Branch header). Prepend it so a picker can switch back to main,
    // and so the variable still yields "main" when netbox-branching isn't
    // installed (the branches endpoint 404s below — degrade to just "main"
    // instead of breaking the dashboard).
    const isBranches = query.objectType === 'plugins/branching/branches';
    const out: MetricFindValue[] = isBranches ? [{ text: 'main', value: 'main' }] : [];

    let res: { columns: string[]; rows: Array<Record<string, unknown>> };
    try {
      // For the branch probe, suppress Grafana's default error toast: on a NetBox
      // without netbox-branching the branches endpoint 404s, which is expected and
      // handled below (degrade to "main"). Without this, that expected 404 pops a
      // toast on every variable refresh and won't clear. Other variables keep their
      // error alerts; a genuine branch-list outage still rethrows (surfacing inline
      // on the variable rather than as a recurring global toast).
      res = await this.runResourceQuery(
        { objectType: query.objectType, filters, fields, limit: 1000, branch },
        isBranches ? { showErrorAlert: false } : undefined
      );
    } catch (err) {
      // Degrade the branch variable to a "main"-only list ONLY when branching
      // isn't installed (the branches endpoint 404s, surfaced by the backend as
      // a "not found" message). Any other failure (auth, 5xx, network) is
      // rethrown so the outage/misconfig surfaces instead of being hidden behind
      // main. If the message can't be matched, we rethrow — the safe default.
      const detail = String(
        (err as { data?: { error?: string }; message?: string })?.data?.error ?? (err as Error)?.message ?? ''
      );
      if (isBranches && /not found|404/i.test(detail)) {
        return out;
      }
      throw err;
    }

    const seen = new Set<string>(isBranches ? ['main'] : []);
    for (const row of res.rows ?? []) {
      const value = String(row[valueField] ?? '');
      if (!value || seen.has(value)) {
        continue;
      }
      seen.add(value);
      const label = String(row[textField] ?? value);
      // Branch names are NOT unique in NetBox, so show "name (schema_id)" to
      // disambiguate same-named branches (the schema id is the value sent).
      out.push({ text: isBranches ? `${label} (${value})` : label, value });
    }
    return out;
  }
}
