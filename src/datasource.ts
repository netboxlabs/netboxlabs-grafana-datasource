import { CoreApp, DataSourceInstanceSettings, MetricFindValue, ScopedVars } from '@grafana/data';
import { DataSourceWithBackend, getTemplateSrv } from '@grafana/runtime';

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

  runResourceQuery(body: {
    objectType: string;
    filters?: FilterRow[];
    fields?: string[];
    limit?: number;
    branch?: string;
  }): Promise<{ columns: string[]; rows: Array<Record<string, unknown>> }> {
    return this.postResource('query', body);
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

    const res = await this.runResourceQuery({ objectType: query.objectType, filters, fields, limit: 1000, branch });

    const seen = new Set<string>();
    const out: MetricFindValue[] = [];
    for (const row of res.rows ?? []) {
      const value = String(row[valueField] ?? '');
      if (!value || seen.has(value)) {
        continue;
      }
      seen.add(value);
      out.push({ text: String(row[textField] ?? value), value });
    }
    return out;
  }
}
