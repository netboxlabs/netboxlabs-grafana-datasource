import { DataSourceJsonData } from '@grafana/data';
import { DataQuery } from '@grafana/schema';

export type ProviderMode = 'netbox' | 'ncs';

/** A single field/operator/value constraint applied to a query. */
export interface FilterRow {
  field: string;
  operator: string; // NetBox lookup: '' (exact), 'ic', 'n', 'gte', 'lte', ...
  value: string;
}

export type QueryType = 'objects' | 'ip-enrichment' | 'topology' | 'annotations';

/** One source→output join-key mapping with an optional value transform. */
export interface JoinKeyMapping {
  source: string;
  output: string;
  transform: string; // none|lower|upper|host|iphost|regex
  regex?: string;
  replace?: string;
}

export interface NetBoxQuery extends DataQuery {
  /** Determines the result shape — see QueryType. Defaults to 'objects'. */
  queryType?: QueryType;
  /** Object type path, e.g. 'dcim/devices' or 'plugins/bgp/bgp-sessions'. */
  objectType?: string;
  filters?: FilterRow[];
  /** Optional subset (and order) of columns to return. */
  fields?: string[];
  /** For objects queries: return a single numeric count of matching objects
   * instead of a table. Required to alert on object counts — Grafana alert
   * expressions evaluate a number, not a table. */
  count?: boolean;
  limit?: number;
  /** Derived key columns so the result lines up with metric labels. */
  joinKeys?: JoinKeyMapping[];
  /** For annotation queries: restrict to NetBox content types, e.g. 'dcim.device'. */
  objectTypes?: string[];
  /** For ip-enrichment: IPs (comma/space/newline separated; supports $variables). */
  ips?: string;
  /** For ip-enrichment: which prefix columns to return. */
  contextFields?: string[];
  /** For topology: drop devices with no inter-device cable (default true). */
  connectedOnly?: boolean;
}

export const DEFAULT_QUERY: Partial<NetBoxQuery> = {
  queryType: 'objects',
  filters: [],
  limit: 100,
};

/** Variable query — populates a dashboard variable from NetBox. */
export interface NetBoxVariableQuery extends DataQuery {
  objectType?: string;
  valueField?: string;
  textField?: string;
  filters?: FilterRow[];
}

export interface NetBoxDataSourceOptions extends DataSourceJsonData {
  url?: string;
  mode?: ProviderMode;
  tlsSkipVerify?: boolean;
  timeoutSeconds?: number;
}

/** Secret values — never returned to the frontend after being set. */
export interface NetBoxSecureJsonData {
  apiToken?: string;
}

export interface ObjectTypeOption {
  value: string;
  label: string;
  app: string;
  model: string;
}

export interface FieldOption {
  name: string;
  type: string;
}

/** Join-key value transforms surfaced in the query editor. */
export const JOIN_KEY_TRANSFORMS: Array<{ label: string; value: string; description?: string }> = [
  { label: 'none', value: 'none', description: 'Use the value as-is' },
  { label: 'lowercase', value: 'lower' },
  { label: 'UPPERCASE', value: 'upper' },
  { label: 'strip domain', value: 'host', description: 'leaf1.dc.com → leaf1' },
  { label: 'IP host', value: 'iphost', description: 'strip CIDR mask: 10.0.0.1/24 → 10.0.0.1' },
  { label: 'regex', value: 'regex', description: 'extract/replace with a regular expression' },
];

/** Default prefix columns offered for IP enrichment. */
export const IP_CONTEXT_FIELDS: string[] = ['prefix', 'site', 'tenant', 'role', 'vrf', 'vlan', 'description'];

/** NetBox filter lookup operators surfaced in the query editor. */
export const FILTER_OPERATORS: Array<{ label: string; value: string }> = [
  { label: '=', value: '' },
  { label: 'contains', value: 'ic' },
  { label: 'not', value: 'n' },
  { label: 'starts with', value: 'isw' },
  { label: 'ends with', value: 'iew' },
  { label: '>=', value: 'gte' },
  { label: '<=', value: 'lte' },
  { label: '>', value: 'gt' },
  { label: '<', value: 'lt' },
  { label: 'empty', value: 'empty' },
];
