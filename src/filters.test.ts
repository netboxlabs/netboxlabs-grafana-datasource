import {
  filterOperatorsFor,
  filterFieldOptionsFrom,
  isOperatorValidForField,
  FILTER_OPERATORS,
  filterWireKey,
  validateFilters,
  type FilterRow,
} from './types';

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

describe('filterWireKey', () => {
  it('mirrors the backend key rule', () => {
    expect(filterWireKey({ field: 'site', operator: '', value: 'a' })).toBe('site');
    expect(filterWireKey({ field: 'name', operator: 'ic', value: 'a' })).toBe('name__ic');
    // Both empty-family operators target the same boolean param.
    expect(filterWireKey({ field: 'serial', operator: 'empty', value: '' })).toBe('serial__empty');
    expect(filterWireKey({ field: 'serial', operator: 'nempty', value: '' })).toBe('serial__empty');
  });

  it('treats the legacy "exact" operator the same as "" (backend does too)', () => {
    expect(filterWireKey({ field: 'name', operator: 'exact', value: 'a' })).toBe('name');
  });
});

describe('validateFilters', () => {
  it('flags a row with a field but no value', () => {
    const issues = validateFilters([{ field: 'status', operator: '', value: '' }]);
    expect(issues).toHaveLength(1);
    expect(issues[0].index).toBe(0);
    expect(issues[0].message).toMatch(/value/i);
  });

  it('does not require a value for empty-family operators', () => {
    expect(validateFilters([{ field: 'serial', operator: 'empty', value: '' }])).toEqual([]);
    expect(validateFilters([{ field: 'serial', operator: 'nempty', value: '' }])).toEqual([]);
  });

  it.each([',', ' , ', ',,'])(
    'flags a value of only separators (%j) the same as blank — buildFilterValues drops every comma segment',
    (value) => {
      const issues = validateFilters([{ field: 'site', operator: '', value }]);
      expect(issues).toHaveLength(1);
      expect(issues[0].index).toBe(0);
      expect(issues[0].severity).toBe('info');
      expect(issues[0].message).toMatch(/value/i);
    }
  );

  it('ignores a wholly blank row (the user is still filling it in)', () => {
    expect(validateFilters([{ field: '', operator: '', value: '' }])).toEqual([]);
  });

  it('treats a row with no value key at all as blank instead of throwing', () => {
    // A provisioned or hand-edited dashboard can omit `value`; f.value.trim()
    // would throw and blank the entire editor.
    const issues = validateFilters([{ field: 'status', operator: '' } as unknown as FilterRow]);
    expect(issues).toHaveLength(1);
    expect(issues[0].message).toMatch(/value/i);
  });

  it('flags a collision when the operator key is entirely absent (provisioned payload)', () => {
    // TypeScript's FilterRow.operator is non-optional, but a provisioned or
    // hand-edited dashboard's JSON can omit it, making it `undefined` at
    // runtime. A missing operator means exact match, same as '' — so this
    // must warn exactly like the explicit-'' collision test above.
    const issues = validateFilters([
      { field: 'site', value: 'ams1' },
      { field: 'site', value: 'nyc1' },
    ] as unknown as FilterRow[]);
    expect(issues).toHaveLength(2);
    expect(issues.every((i) => i.severity === 'warning')).toBe(true);
    expect(issues[0].message).toMatch(/site/);
  });

  it('still flags the no-value info issue when the operator key is entirely absent', () => {
    // The missing-operator normalisation must not break the blank/comma
    // logic from an earlier round.
    const issues = validateFilters([{ field: 'site' } as unknown as FilterRow]);
    expect(issues).toHaveLength(1);
    expect(issues[0].severity).toBe('info');
    expect(issues[0].message).toMatch(/value/i);
  });

  it('flags two rows that collide on the same wire key (NetBox ORs them)', () => {
    const issues = validateFilters([
      { field: 'name', operator: 'ic', value: 'spine' },
      { field: 'name', operator: 'ic', value: '01' },
    ]);
    // Both rows are flagged so the user can see the pair.
    expect(issues.map((i) => i.index).sort()).toEqual([0, 1]);
    expect(issues[0].message).toMatch(/OR/);
  });

  it('does not flag two negated rows — repeated __n params are NOR, i.e. the stacked AND', () => {
    // NetBox runs qs.exclude(Q(a) | Q(b)) = NOT a AND NOT b. Verified live:
    // ?site__n=ams1&site__n=nyc1 returns only the device in neither site. So
    // "devices that are neither offline nor planned" is already correct.
    expect(
      validateFilters([
        { field: 'status', operator: 'n', value: 'offline' },
        { field: 'status', operator: 'n', value: 'planned' },
      ])
    ).toEqual([]);
  });

  it('uses info for a not-yet-filled row and warning for a real mistake', () => {
    // Severity ladder: picking a field before typing a value is normal authoring,
    // and many users never filter at all — that must not read as an error.
    const blank = validateFilters([{ field: 'status', operator: '', value: '' }]);
    expect(blank[0].severity).toBe('info');
    const collision = validateFilters([
      { field: 'name', operator: 'ic', value: 'spine' },
      { field: 'name', operator: 'ic', value: '01' },
    ]);
    expect(collision[0].severity).toBe('warning');
  });

  it('still flags a positive collision as OR', () => {
    const issues = validateFilters([
      { field: 'name', operator: 'ic', value: 'spine' },
      { field: 'name', operator: 'ic', value: '01' },
    ]);
    expect(issues).toHaveLength(2);
    expect(issues[0].message).toMatch(/OR/);
  });

  it('does not warn of a collision when one of the two rows is still blank', () => {
    // buildFilterValues only emits a param when strings.TrimSpace(v) != "", so
    // a blank-value row contributes nothing to the URL and cannot collide.
    // Only the info issue for the blank row should appear — no OR warning for
    // either index.
    const issues = validateFilters([
      { field: 'name', operator: 'ic', value: '' },
      { field: 'name', operator: 'ic', value: 'x' },
    ]);
    expect(issues).toHaveLength(1);
    expect(issues[0].index).toBe(0);
    expect(issues[0].severity).toBe('info');
    expect(issues[0].message).toMatch(/value/i);
  });

  it('does not warn of a collision when both rows on the same field are blank', () => {
    const issues = validateFilters([
      { field: 'name', operator: 'ic', value: '' },
      { field: 'name', operator: 'ic', value: '' },
    ]);
    expect(issues).toHaveLength(2);
    expect(issues.every((i) => i.severity === 'info')).toBe(true);
    expect(issues.every((i) => i.message.match(/value/i))).toBe(true);
  });

  it('does not warn of a collision when one row is a separators-only value (a disguised blank)', () => {
    // strings.Split(",", ",") -> ["", ""] -> nothing survives TrimSpace, so
    // this row emits no param on the wire and can't OR with the other row.
    const issues = validateFilters([
      { field: 'site', operator: '', value: ',' },
      { field: 'site', operator: '', value: 'ams1' },
    ]);
    expect(issues).toHaveLength(1);
    expect(issues[0].index).toBe(0);
    expect(issues[0].severity).toBe('info');
  });

  it('still flags a genuine collision when a CSV value has a trailing empty segment', () => {
    // "ams1," has one real segment ("ams1") alongside the empty one, so this
    // row DOES emit ?site=ams1 — guard against over-fixing into never warning
    // when a value merely contains a comma.
    const issues = validateFilters([
      { field: 'site', operator: '', value: 'ams1,' },
      { field: 'site', operator: '', value: 'lhr2' },
    ]);
    expect(issues).toHaveLength(2);
    expect(issues.every((i) => i.severity === 'warning')).toBe(true);
  });

  it('does not flag stacked tag rows — NetBox ANDs repeated tag params', () => {
    // TagFilter/TagIDFilter set conjoined=True. Verified on NetBox 4.4.10:
    // ?tag=crit&tag=prod returns only devices carrying BOTH tags. Warning "OR"
    // here would be the opposite of the truth.
    expect(
      validateFilters([
        { field: 'tag', operator: '', value: 'crit' },
        { field: 'tag', operator: '', value: 'prod' },
      ])
    ).toEqual([]);
    expect(
      validateFilters([
        { field: 'tag_id', operator: '', value: '1' },
        { field: 'tag_id', operator: '', value: '2' },
      ])
    ).toEqual([]);
  });

  it('does not flag different operators on the same field', () => {
    expect(
      validateFilters([
        { field: 'name', operator: 'ic', value: 'spine' },
        { field: 'name', operator: 'nic', value: 'old' },
      ])
    ).toEqual([]);
  });

  it('flags a collision between "" and the legacy "exact" operator (same wire key, both OR)', () => {
    // 'exact' is byte-identical to '' in buildFilterValues (same key, same
    // q.Add), so it belongs in OR_COMBINING_OPERATORS alongside ''. Only
    // reachable via a provisioned/hand-edited dashboard.
    const issues = validateFilters([
      { field: 'site', operator: '', value: 'a' },
      { field: 'site', operator: 'exact', value: 'b' },
    ]);
    expect(issues).toHaveLength(2);
    expect(issues.every((i) => i.severity === 'warning')).toBe(true);
    expect(issues[0].message).toMatch(/site/);
  });

  it('flags conflicting empty-family rows with last-wins wording, not OR', () => {
    const issues = validateFilters([
      { field: 'serial', operator: 'empty', value: '' },
      { field: 'serial', operator: 'nempty', value: '' },
    ]);
    expect(issues.map((i) => i.index).sort()).toEqual([0, 1]);
    // __empty is set, not appended, so the rows do not OR — they overwrite.
    expect(issues[0].message).toMatch(/only the last one is applied/i);
    expect(issues[0].message).not.toMatch(/OR/);
  });
});
