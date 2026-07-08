import { CustomVariableSupport, DataQueryRequest, DataQueryResponse, MetricFindValue } from '@grafana/data';
import { Observable, from } from 'rxjs';
import { map } from 'rxjs/operators';

import { DataSource } from './datasource';
import { NetBoxVariableQuery } from './types';
import { VariableQueryEditor } from './components/VariableQueryEditor';

/**
 * NetBoxVariableSupport exposes query-driven dashboard variables — e.g. a
 * "device" or "site" dropdown populated from NetBox that then filters every
 * metrics panel on the dashboard.
 */
export class NetBoxVariableSupport extends CustomVariableSupport<DataSource, NetBoxVariableQuery> {
  editor = VariableQueryEditor;

  constructor(private readonly ds: DataSource) {
    super();
  }

  query(request: DataQueryRequest<NetBoxVariableQuery>): Observable<DataQueryResponse> {
    const target = request.targets[0];
    const promise = this.ds.metricFindQuery(target, { scopedVars: request.scopedVars });
    return from(promise).pipe(map((values: MetricFindValue[]) => ({ data: values }) as unknown as DataQueryResponse));
  }
}
