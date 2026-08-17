import { ORDERING_FIELDS, composeOrdering, orderingField, orderingFieldsFor, orderingIsDescending } from './types';

describe('ORDERING_FIELDS', () => {
  it('covers exactly the object types measured on both supported NetBox versions', () => {
    expect(Object.keys(ORDERING_FIELDS).sort()).toEqual([
      'dcim/devices',
      'dcim/interfaces',
      'dcim/sites',
      'ipam/ip-addresses',
      'ipam/prefixes',
      'virtualization/virtual-machines',
    ]);
  });

  it('offers no field measured to break the query or drop the sort', () => {
    // Each of these was probed, not guessed. device_count is the sharp one: it
    // answers 200 unprojected and 500 alongside a `?fields=` projection that
    // does not name it, so it looks healthy to anyone probing by hand and its
    // fate depends on the panel's column selection rather than on the sort.
    // scope and assigned_object fail differently by version — 500 on 4.6.4,
    // accepted and silently ignored on 4.4.10, where the DESCENDING probe is
    // what proves it (`-scope` returns the ascending walk unchanged) — so
    // neither one ever sorts.
    expect(ORDERING_FIELDS['dcim/sites']).not.toContain('device_count');
    expect(ORDERING_FIELDS['ipam/prefixes']).not.toContain('scope');
    expect(ORDERING_FIELDS['ipam/ip-addresses']).not.toContain('assigned_object');
  });

  it('offers id everywhere, the one column every NetBox model has', () => {
    for (const [objectType, fields] of Object.entries(ORDERING_FIELDS)) {
      expect(fields).toContain('id');
      // Bare names only: the backend appends the ",id" tiebreaker and the "-"
      // is the editor's to add, so anything pre-composed here would be sent
      // twice over.
      for (const f of fields) {
        expect(f).not.toMatch(/[,\s-]/);
      }
      expect(new Set(fields).size).toBe(fields.length);
      expect(objectType).toMatch(/^[a-z-]+\/[a-z-]+$/);
    }
  });
});

describe('orderingFieldsFor', () => {
  it('answers with the fields for a type this plugin will sort', () => {
    expect(orderingFieldsFor('dcim/devices')).toEqual([
      'id',
      'name',
      'site',
      'role',
      'device_type',
      'status',
      'last_updated',
    ]);
  });

  it('answers with nothing for a type it will not sort — including plugin models', () => {
    expect(orderingFieldsFor('dcim/racks')).toEqual([]);
    expect(orderingFieldsFor('plugins/bgp/bgp-sessions')).toEqual([]);
    expect(orderingFieldsFor(undefined)).toEqual([]);
    expect(orderingFieldsFor('')).toEqual([]);
  });

  it('cannot be used to edit the allow-list it guards', () => {
    orderingFieldsFor('dcim/sites').push('device_count');
    expect(orderingFieldsFor('dcim/sites')).not.toContain('device_count');
  });
});

describe('orderingField / orderingIsDescending', () => {
  it('splits a stored value into field and direction', () => {
    expect(orderingField('name')).toBe('name');
    expect(orderingIsDescending('name')).toBe(false);
    expect(orderingField('-last_updated')).toBe('last_updated');
    expect(orderingIsDescending('-last_updated')).toBe(true);
  });

  it('reads an absent or blank sort as no sort at all', () => {
    expect(orderingField(undefined)).toBe('');
    expect(orderingField('')).toBe('');
    expect(orderingField('  ')).toBe('');
    expect(orderingField('-')).toBe('');
    expect(orderingIsDescending(undefined)).toBe(false);
    expect(orderingIsDescending('')).toBe(false);
  });

  it('tolerates the stray whitespace a provisioned dashboard can carry', () => {
    // Mirrors orderingValue's strings.TrimSpace in
    // pkg/provider/netbox/ordering.go: a leading space is not a different field
    // to a human, and NetBox agrees (DRF strips each ordering term). It is the
    // allow-list that matches literally, so an untrimmed ' -name ' would lose a
    // sort NetBox would have accepted.
    expect(orderingField(' -name ')).toBe('name');
    expect(orderingIsDescending(' -name ')).toBe(true);
  });
});

describe('composeOrdering', () => {
  it('sends a bare field name, and a leading - for descending', () => {
    expect(composeOrdering('name', false)).toBe('name');
    expect(composeOrdering('name', true)).toBe('-name');
  });

  it("never sends the ,id tiebreaker — that is the backend's to append", () => {
    // Sending it here would produce "name,id,id" upstream.
    expect(composeOrdering('name', true)).not.toContain(',');
    expect(composeOrdering('last_updated', false)).not.toContain('id,');
  });

  it('drops the sort entirely when no field is chosen', () => {
    expect(composeOrdering('', false)).toBeUndefined();
    expect(composeOrdering('', true)).toBeUndefined();
  });

  it('round-trips with the readers', () => {
    for (const descending of [false, true]) {
      for (const field of ORDERING_FIELDS['dcim/devices']) {
        const stored = composeOrdering(field, descending)!;
        expect(orderingField(stored)).toBe(field);
        expect(orderingIsDescending(stored)).toBe(descending);
      }
    }
  });
});
