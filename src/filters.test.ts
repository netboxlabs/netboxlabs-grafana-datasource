import { filterOperatorsFor, filterFieldOptionsFrom, isOperatorValidForField, FILTER_OPERATORS } from './types';

const schema = [
  { name: 'prefix', operators: [''] },
  { name: 'status', operators: ['', 'ic', 'isw', 'n', 'empty'] },
];

describe('filterOperatorsFor', () => {
  it('restricts to the schema operators for a known field', () => {
    expect(filterOperatorsFor(schema, 'prefix', '').map((o) => o.value)).toEqual(['']); // only exact
    expect(filterOperatorsFor(schema, 'status', '').map((o) => o.value)).toContain('ic');
    expect(filterOperatorsFor(schema, 'prefix', '').map((o) => o.value)).not.toContain('ic');
  });
  it('falls back to all operators when the schema is empty (unavailable)', () => {
    expect(filterOperatorsFor([], 'anything', '')).toEqual(FILTER_OPERATORS);
  });
  it('does not block an unknown/legacy field', () => {
    expect(filterOperatorsFor(schema, 'not_in_schema', '')).toEqual(FILTER_OPERATORS);
  });
  it('keeps a stored operator visible even if the schema disallows it', () => {
    expect(filterOperatorsFor(schema, 'prefix', 'ic').map((o) => o.value)).toContain('ic');
  });
});

describe('isOperatorValidForField', () => {
  it('is false when a schema-known field does not support the operator', () => {
    expect(isOperatorValidForField(schema, 'prefix', 'ic')).toBe(false); // prefix supports only exact
    expect(isOperatorValidForField(schema, 'status', 'ic')).toBe(true);
  });
  it('is permissive for unknown fields and the empty-schema fallback', () => {
    expect(isOperatorValidForField(schema, 'not_in_schema', 'ic')).toBe(true);
    expect(isOperatorValidForField([], 'prefix', 'ic')).toBe(true);
  });
});

describe('filterFieldOptionsFrom', () => {
  it('uses schema field names when available', () => {
    expect(filterFieldOptionsFrom(schema, [{ label: 'col', value: 'col' }]).map((o) => o.value)).toEqual(['prefix', 'status']);
  });
  it('falls back to columns when the schema is empty', () => {
    expect(filterFieldOptionsFrom([], [{ label: 'col', value: 'col' }]).map((o) => o.value)).toEqual(['col']);
  });
});
