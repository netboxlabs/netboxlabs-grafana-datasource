import { DataSourceJsonData, SelectableValue } from '@grafana/data';
import { DataQuery } from '@grafana/schema';

/** Enrichment backend. Only 'netbox' exists today; the type and the `mode`
 * option are kept as the seam for a planned second, high-volume backend
 * (no UI selector until then). */
export type ProviderMode = 'netbox';

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
  transform: string; // none|lower|upper|host|iphost|ifshort|regex
  regex?: string;
  replace?: string;
}

export interface NetBoxQuery extends DataQuery {
  /** Determines the result shape — see QueryType. Defaults to 'objects'. */
  queryType?: QueryType;
  /** Object type path, e.g. 'dcim/devices' or 'plugins/bgp/bgp-sessions'. */
  objectType?: string;
  /** Optional netbox-branching schema id (or a $variable resolving to one).
   * When set, the query targets that branch; empty targets main. Requires the
   * netbox-branching plugin on the NetBox side. */
  branch?: string;
  filters?: FilterRow[];
  /** Optional subset (and order) of columns to return. */
  fields?: string[];
  /** For objects queries: return a single numeric count of matching objects
   * instead of a table. Required to alert on object counts — Grafana alert
   * expressions evaluate a number, not a table. */
  count?: boolean;
  /** For objects queries: reshape the result for Grafana alerting — string
   * label columns plus one numeric `value` column, one alert instance per
   * row. Mutually exclusive with `count` (the editor enforces it). */
  alertTable?: boolean;
  /** Column supplying the numeric value in alert-table mode (e.g.
   * 'utilization'). Empty = constant 1 per row. */
  valueField?: string;
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
  /** For topology: edge derivation — 'logical' (NetBox cable paths; panels and
   * circuits resolve to the far device) or 'physical' (raw cables; panels
   * appear as nodes). Default 'logical'. */
  connections?: 'logical' | 'physical';
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
  /** Optional netbox-branching schema id (or $variable) to scope the variable's options. */
  branch?: string;
}

export interface NetBoxDataSourceOptions extends DataSourceJsonData {
  url?: string;
  /** Browser-facing NetBox base URL, if different from url (e.g. Grafana
   * reaches NetBox via internal service DNS). Used to rewrite deep links. */
  publicUrl?: string;
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

/** A valid filter parameter for an object type and the operators NetBox supports on it. */
export interface FilterField {
  name: string;
  operators: string[];
}

/** Join-key value transforms surfaced in the query editor. */
export const JOIN_KEY_TRANSFORMS: Array<{ label: string; value: string; description?: string }> = [
  { label: 'none', value: 'none', description: 'Use the value as-is' },
  { label: 'lowercase', value: 'lower' },
  { label: 'UPPERCASE', value: 'upper' },
  { label: 'strip domain', value: 'host', description: 'leaf1.dc.com → leaf1' },
  { label: 'IP host', value: 'iphost', description: 'strip CIDR mask: 10.0.0.1/24 → 10.0.0.1' },
  { label: 'interface short name', value: 'ifshort', description: 'GigabitEthernet0/1 → Gi0/1 (netutils standard)' },
  { label: 'regex', value: 'regex', description: 'extract/replace with a regular expression' },
];

/** Default prefix columns offered for IP enrichment. */
export const IP_CONTEXT_FIELDS: string[] = ['prefix', 'site', 'tenant', 'role', 'vrf', 'vlan', 'description'];

/** NetBox filter lookup operators surfaced in the query editor. */
// NetBox lookup operators. `value` is the lookup suffix sent to the API
// (empty = exact); the backend maps `field__<value>`. Keep this in sync with
// suffixToken/operatorOrder in pkg/provider/netbox/schema.go — the query editor
// only offers operators present in BOTH this list and the object type's schema.
export const FILTER_OPERATORS: Array<{ label: string; value: string }> = [
  { label: '=', value: '' },
  { label: 'not', value: 'n' },
  { label: '= (ci)', value: 'ie' },
  { label: 'not (ci)', value: 'nie' },
  { label: 'contains', value: 'ic' },
  { label: 'not contains', value: 'nic' },
  { label: 'starts with', value: 'isw' },
  { label: 'not starts with', value: 'nisw' },
  { label: 'ends with', value: 'iew' },
  { label: 'not ends with', value: 'niew' },
  { label: 'regex', value: 'regex' },
  { label: 'regex (ci)', value: 'iregex' },
  { label: '>=', value: 'gte' },
  { label: '<=', value: 'lte' },
  { label: '>', value: 'gt' },
  { label: '<', value: 'lt' },
  { label: 'empty', value: 'empty' },
];

type OpOption = { label: string; value: string };

/** Filter-field dropdown options: schema filter fields when available, else the
 * fallback column options (schema unavailable). */
export function filterFieldOptionsFrom(
  filterFields: FilterField[],
  fallback: Array<SelectableValue<string>>
): Array<SelectableValue<string>> {
  return filterFields.length > 0 ? filterFields.map((f) => ({ label: f.name, value: f.name })) : fallback;
}

/** Operators offered for a filter field. Schema-restricted when the field is
 * known; falls back to all operators when the schema is unavailable
 * (filterFields empty) or the field isn't in the schema (legacy/custom), so
 * saved queries are never blocked. A stored operator not in the allowed set is
 * appended so it stays visible/editable. */
export function filterOperatorsFor(filterFields: FilterField[], field: string, storedOperator: string): OpOption[] {
  const ff = filterFields.find((f) => f.name === field);
  let ops: OpOption[] =
    filterFields.length === 0 || !ff ? FILTER_OPERATORS : FILTER_OPERATORS.filter((o) => ff.operators.includes(o.value));
  if (!ops.some((o) => o.value === storedOperator)) {
    ops = [...ops, FILTER_OPERATORS.find((o) => o.value === storedOperator) ?? { label: storedOperator || '=', value: storedOperator }];
  }
  return ops;
}

/** Whether an operator is valid for a filter field per the schema. Unknown
 * fields and the empty-schema fallback are permissive (true) so nothing is
 * blocked; a schema-known field validates against its operator set. Used to
 * reset a stale operator when the user switches a filter's field. */
export function isOperatorValidForField(filterFields: FilterField[], field: string, operator: string): boolean {
  const ff = filterFields.find((f) => f.name === field);
  return !ff || ff.operators.includes(operator);
}
