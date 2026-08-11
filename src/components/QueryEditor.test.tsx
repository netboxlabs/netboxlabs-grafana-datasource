import React from 'react';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { QueryEditor } from './QueryEditor';
import { IP_CONTEXT_FIELD_GROUPS, IP_CONTEXT_FIELD_OPTIONS, DEFAULT_IP_CONTEXT_FIELDS } from '../types';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({ replace: (s: string) => (s === '$branch' ? 'td5smq0f' : s) }),
}));

// @grafana/ui's Select menu (via ScrollIndicators) uses IntersectionObserver to
// decide when to show scroll shadows; jsdom doesn't implement it, so opening a
// Select's dropdown throws without this stub.
class IntersectionObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
(globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = IntersectionObserverStub;

// Minimal datasource stub: the editor calls these in effects on mount.
const datasource = {
  uid: 'ds-query-editor',
  getObjectTypes: jest
    .fn()
    .mockResolvedValue([{ value: 'dcim/devices', label: 'Devices', app: 'dcim', model: 'devices' }]),
  getFields: jest.fn().mockResolvedValue([{ name: 'name' }, { name: 'status' }]),
  getFilterFields: jest.fn().mockResolvedValue([
    { name: 'prefix', operators: [''] },
    { name: 'status', operators: ['', 'ic', 'isw', 'n', 'empty'] },
  ]),
  getBranchingInstalled: jest.fn().mockResolvedValue(true),
} as any;

// The branching probe is cached per datasource INSTANCE (WeakMap); gating tests
// use distinct stub objects, so no cache reset is needed between tests.
function setup(queryOverrides: Record<string, unknown> = {}, ds: any = datasource) {
  const onChange = jest.fn();
  const onRunQuery = jest.fn();
  const query = { refId: 'A', queryType: 'objects', objectType: 'dcim/devices', ...queryOverrides } as any;
  const { container } = render(
    <QueryEditor query={query} onChange={onChange} onRunQuery={onRunQuery} datasource={ds} />
  );
  return { onChange, onRunQuery, container };
}

// Each filter row is rendered as its own Stack that is a direct child of the
// editor's single root Stack (see QueryEditor.tsx's per-row `<Stack key={i}
// direction="column">` wrapper). Walking up from any element inside a row
// until its parent is that root returns exactly that row's container,
// without depending on how many internal divs react-select happens to
// nest `el` under.
function rowContainerOf(container: HTMLElement, el: HTMLElement): HTMLElement {
  const root = container.firstElementChild as HTMLElement;
  let node: HTMLElement = el;
  while (node.parentElement && node.parentElement !== root) {
    node = node.parentElement;
  }
  return node;
}

describe('QueryEditor — Return count only', () => {
  it('shows the count switch for objects queries and sets query.count when toggled', async () => {
    const { onChange, onRunQuery } = setup();
    const sw = await screen.findByRole('switch', { name: /Return count only/i });
    expect(sw).toBeInTheDocument();
    expect(sw).not.toBeChecked();

    fireEvent.click(sw);

    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ count: true }));
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('does not show the count switch for non-objects queries', async () => {
    setup({ queryType: 'topology' });
    // let mount effects resolve
    await screen.findByText(/node graph/i);
    expect(screen.queryByRole('switch', { name: /Return count only/i })).not.toBeInTheDocument();
  });
});

describe('QueryEditor — Branch', () => {
  it('sets the branch field', async () => {
    const { onChange } = setup({ queryType: 'objects', objectType: 'dcim/devices' });

    const input = await screen.findByLabelText('Branch');
    fireEvent.change(input, { target: { value: 'td5smq0f' } });

    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ branch: 'td5smq0f' }));
  });

  it('loads fields scoped to the query branch (interpolating a variable)', async () => {
    // '$branch' → 'td5smq0f' via the getTemplateSrv mock, so this proves the
    // branch is interpolated (not passed raw) before reaching getFields.
    setup({ queryType: 'objects', objectType: 'dcim/devices', branch: '$branch' });
    await waitFor(() => expect(datasource.getFields).toHaveBeenCalledWith('dcim/devices', 'td5smq0f'));
  });
});

describe('QueryEditor — Alert table', () => {
  it('toggles alert table mode and clears count', async () => {
    const { onChange, onRunQuery } = setup({ count: true });
    const sw = await screen.findByRole('switch', { name: /Alert table/i });
    expect(sw).toBeInTheDocument();
    expect(sw).not.toBeChecked();

    fireEvent.click(sw);

    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ alertTable: true, count: false }));
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('clears valueField when alert table mode is toggled off', async () => {
    const { onChange } = setup({ alertTable: true, valueField: 'utilization' });
    const sw = await screen.findByRole('switch', { name: /Alert table/i });
    expect(sw).toBeChecked();

    fireEvent.click(sw);

    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ alertTable: false, valueField: undefined }));
  });

  it('clears valueField when Return count only is toggled on', async () => {
    const { onChange } = setup({ alertTable: true, valueField: 'utilization' });
    const sw = await screen.findByRole('switch', { name: /Return count only/i });

    fireEvent.click(sw);

    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ count: true, alertTable: false, valueField: undefined })
    );
  });
});

describe('QueryEditor — Branch field gating (OBS-3651)', () => {
  it('disables the Branch field when branching is not installed', async () => {
    const ds = { ...datasource, uid: 'ds-absent', getBranchingInstalled: jest.fn().mockResolvedValue(false) };
    setup({}, ds);
    const input = await screen.findByLabelText('Branch');
    await waitFor(() => expect(input).toBeDisabled());
  });

  it('keeps the Branch field enabled when branching is installed', async () => {
    const ds = { ...datasource, uid: 'ds-present', getBranchingInstalled: jest.fn().mockResolvedValue(true) };
    setup({}, ds);
    const input = await screen.findByLabelText('Branch');
    await waitFor(() => expect(ds.getBranchingInstalled).toHaveBeenCalled());
    expect(input).not.toBeDisabled();
  });

  it('fails open (Branch enabled) when the probe errors', async () => {
    const ds = {
      ...datasource,
      uid: 'ds-error',
      getBranchingInstalled: jest.fn().mockRejectedValue(new Error('boom')),
    };
    setup({}, ds);
    const input = await screen.findByLabelText('Branch');
    await waitFor(() => expect(ds.getBranchingInstalled).toHaveBeenCalled());
    expect(input).not.toBeDisabled();
  });
});

describe('QueryEditor — schema filters', () => {
  it('loads filter fields for the object type and shows them', async () => {
    setup({
      queryType: 'objects',
      objectType: 'ipam/prefixes',
      filters: [{ field: 'prefix', operator: '', value: '' }],
    });
    await waitFor(() => expect(datasource.getFilterFields).toHaveBeenCalledWith('ipam/prefixes', undefined));
    expect(await screen.findByText('prefix')).toBeInTheDocument();
  });
});

describe('QueryEditor — empty-family operators', () => {
  it('hides the value input when the operator is is-empty or has-any-value', async () => {
    setup({
      filters: [
        { field: 'serial', operator: 'empty', value: '' },
        { field: 'serial', operator: 'nempty', value: '' },
      ],
    });
    await screen.findByLabelText('filter-field-0');
    expect(screen.queryByPlaceholderText('value or $variable')).not.toBeInTheDocument();
  });

  it('shows the value input for ordinary operators', async () => {
    setup({ filters: [{ field: 'name', operator: 'ic', value: 'spine' }] });
    await screen.findByLabelText('filter-field-0');
    expect(screen.getByPlaceholderText('value or $variable')).toBeInTheDocument();
  });

  it('runs the query immediately when an empty-family operator is selected', async () => {
    const { onRunQuery } = setup({ filters: [{ field: 'serial', operator: '', value: 'abc' }] });
    await screen.findByLabelText('filter-field-0');

    const operatorSelect = screen.getByLabelText('filter-operator-0');
    fireEvent.keyDown(operatorSelect, { key: 'ArrowDown' });
    const option = await screen.findByText('is empty');
    fireEvent.click(option);

    expect(onRunQuery).toHaveBeenCalled();
  });
});

describe('QueryEditor — filter validation messages', () => {
  it('warns when a filter row has a field but no value', async () => {
    setup({ filters: [{ field: 'status', operator: '', value: '' }] });
    expect(await screen.findByText(/isn't applied/i)).toBeInTheDocument();

    // Severity must come from issue.severity, not be hardcoded: @grafana/ui's
    // Alert renders info/success with role="status" and warning/error with
    // role="alert" (see Alert.mjs's `rolesBySeverity` map), so a not-yet-filled
    // row's message must be reachable via the "status" role and NOT "alert".
    expect(await screen.findByRole('status', { name: /isn't applied/i })).toBeInTheDocument();
    expect(screen.queryByRole('alert', { name: /isn't applied/i })).not.toBeInTheDocument();
  });

  it('warns that colliding rows are OR-ed, naming the param', async () => {
    setup({
      filters: [
        { field: 'name', operator: 'ic', value: 'spine' },
        { field: 'name', operator: 'ic', value: '01' },
      ],
    });
    const msgs = await screen.findAllByText(/name__ic/);
    expect(msgs.length).toBeGreaterThan(0);
    expect(msgs[0]).toHaveTextContent(/OR/);

    // A collision is a 'warning', the opposite end of the severity ladder from
    // the no-value case above: it must render via role="alert", not "status".
    const alerts = await screen.findAllByRole('alert', { name: /name__ic/i });
    expect(alerts.length).toBeGreaterThan(0);
    expect(screen.queryByRole('status', { name: /name__ic/i })).not.toBeInTheDocument();
  });

  it('shows no message for a valid filter row', async () => {
    setup({ filters: [{ field: 'name', operator: 'ic', value: 'spine' }] });
    await screen.findByLabelText('filter-field-0');
    expect(screen.queryByText(/isn't applied/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/not AND/i)).not.toBeInTheDocument();
  });

  it('attributes a row message to that row only, not to every row', async () => {
    const { container } = setup({
      filters: [
        { field: 'status', operator: '', value: '' }, // has an issue
        { field: 'name', operator: 'ic', value: 'spine' }, // valid, no issue
      ],
    });
    await screen.findByText(/isn't applied/i);
    // Grab the row anchors only after all mount effects (getFilterFields etc.)
    // have settled — react-select can replace its internal input node when
    // filterFields/options change mid-mount, which would leave an
    // earlier-captured reference detached from the live tree.
    const row0Field = await screen.findByLabelText('filter-field-0');
    const row1Field = await screen.findByLabelText('filter-field-1');

    const row0 = rowContainerOf(container, row0Field);
    const row1 = rowContainerOf(container, row1Field);

    expect(within(row0).getByText(/isn't applied/i)).toBeInTheDocument();
    expect(within(row1).queryByText(/isn't applied/i)).not.toBeInTheDocument();
  });
});

describe('IP context field groups', () => {
  it('groups every field under a namespace heading', () => {
    expect(IP_CONTEXT_FIELD_GROUPS.map((g) => g.label)).toEqual([
      'Identity',
      'Prefix',
      'Address',
      'Interface',
      'Device',
    ]);
  });

  it('offers no bare legacy names and no unfillable columns', () => {
    const all = IP_CONTEXT_FIELD_GROUPS.flatMap((g) => g.options.map((o) => o.value));
    for (const banned of ['site', 'tenant', 'role', 'vrf', 'vlan', 'prefix', 'prefix_site']) {
      expect(all).not.toContain(banned);
    }
    // The full unfillable set the Go guard (TestIPEnrichColumnsAreNamespaced)
    // checks. The interface_* five are cut because assigned_object is the brief
    // interface serializer; the *_dns pair because NetBox 4.4's NestedIPAddress
    // serializer has no dns_name property at all. Either one offered here would
    // render a permanently blank column.
    for (const unfillable of [
      'interface_enabled',
      'interface_type',
      'interface_mtu',
      'interface_mac_address',
      'interface_lag',
      'address_nat_inside_dns',
      'address_nat_outside_dns',
    ]) {
      expect(all).not.toContain(unfillable);
    }
  });

  // The mechanical half of this guard — asserting the picker's vocabulary is
  // exactly IPEnrichColumns() — lives in Go, in
  // TestIPContextFieldsMatchFrontend (pkg/provider/netbox/ipenrich_test.go),
  // which reads this file. It is on that side because reading a file from a
  // Jest test needs Node's fs typings, and this project's tsconfig does not
  // include @types/node; a build-config change would be a steep price for a
  // guard Go can run for free.

  it('defaults to a useful handful including match_count', () => {
    const all = IP_CONTEXT_FIELD_GROUPS.flatMap((g) => g.options);
    expect(DEFAULT_IP_CONTEXT_FIELDS.length).toBeLessThan(all.length / 2);
    expect(DEFAULT_IP_CONTEXT_FIELDS).toContain('match_count');
    expect(DEFAULT_IP_CONTEXT_FIELDS).toContain('device_is_primary_ip');
  });

  it('labels every option by its full column name, so chips are unambiguous', () => {
    // @grafana/ui resolves a MultiSelect's selected chips against the option
    // list. With group-local labels the default selection rendered two adjacent
    // chips both reading "name" (device_name and interface_name), and a
    // nine-column panel read "match_count cidr scope tenant role vlan name
    // is_primary_ip name" — unreadable, and wrong about which columns are shown.
    for (const o of IP_CONTEXT_FIELD_OPTIONS) {
      expect(o.label).toBe(o.value);
    }
    const labels = IP_CONTEXT_FIELD_OPTIONS.map((o) => o.label);
    expect(new Set(labels).size).toBe(labels.length);
  });
});

describe('IP-enrichment join keys', () => {
  it("offers the enrichment columns, not a NetBox object type's fields", async () => {
    // getFields would answer with ipam/prefixes' schema; the join-key picker
    // must not be built from it. Live, that offered `scope` (which yields an
    // empty key) while omitting `prefix_scope` and `device_name`, which work.
    const ds = {
      ...datasource,
      getFields: jest.fn().mockResolvedValue([{ name: 'scope' }, { name: 'prefix' }, { name: 'vlan' }]),
      getFilterFields: jest.fn().mockResolvedValue([]),
    } as any;
    // The row is seeded rather than added by clicking: onChange is a spy, so the
    // component never re-renders with the new joinKeys.
    setup(
      { queryType: 'ip-enrichment', ips: '10.20.0.1', joinKeys: [{ source: '', output: '', transform: 'none' }] },
      ds
    );

    const picker = await screen.findByLabelText('join-key-source-0');
    fireEvent.focus(picker);
    fireEvent.keyDown(picker, { key: 'ArrowDown', keyCode: 40, code: 'ArrowDown' });

    // Scoped to the open menu: several of these names also appear as context-field
    // chips elsewhere on the form, so an unscoped query would match those instead
    // and pass without the dropdown offering anything at all.
    const menu = within(await screen.findByRole('listbox'));

    // The two columns verified live to produce a working join key.
    expect(menu.getByText('device_name')).toBeInTheDocument();
    expect(menu.getByText('prefix_scope')).toBeInTheDocument();
    // And nothing from the object-type schema, every one of which derived an
    // empty key. `scope` is the one the editor used to suggest by default.
    for (const fromSchema of ['scope', 'prefix', 'vlan']) {
      expect(menu.queryByText(fromSchema)).not.toBeInTheDocument();
    }
  });

  it('renders selected context fields as full, distinguishable chip names', async () => {
    setup({ queryType: 'ip-enrichment', ips: '10.20.0.1' });
    // The default selection contains device_name AND interface_name. With
    // group-local labels both chips read "name".
    expect(await screen.findByText('device_name')).toBeInTheDocument();
    expect(screen.getByText('interface_name')).toBeInTheDocument();
    expect(screen.queryByText('name')).not.toBeInTheDocument();
  });

  it('does not fetch object-type fields for an ip-enrichment query at all', async () => {
    const ds = {
      ...datasource,
      getFields: jest.fn().mockResolvedValue([{ name: 'scope' }]),
      getFilterFields: jest.fn().mockResolvedValue([]),
    } as any;
    setup({ queryType: 'ip-enrichment', ips: '10.20.0.1' }, ds);
    await screen.findByText('Add join key');
    expect(ds.getFields).not.toHaveBeenCalled();
  });
});
