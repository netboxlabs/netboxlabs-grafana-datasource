import React from 'react';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { ORDERING_DISABLED_TOOLTIP, ORDERING_TOOLTIP, QueryEditor } from './QueryEditor';
import { FAST_PAGING_TOOLTIP } from './ConfigEditor';
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
  // DataSourceWithBackend keeps the instance settings here (it assigns them in
  // its constructor); the editor reads jsonData.fastPagingNoTotals from it.
  datasourceInstanceSettings: { jsonData: {} },
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

describe('QueryEditor — Branch field gating', () => {
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
      'Virtual machine',
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
    expect(DEFAULT_IP_CONTEXT_FIELDS).toContain('is_primary_ip');
  });

  it('carries is_primary_ip in the un-namespaced Identity group, not under Device', () => {
    // The flag answers "is this the address the poller talks to" for
    // whatever owns the address, and NetBox gives two owners that can answer:
    // dcim.device and virtualization.virtualmachine both expose
    // primary_ip4/primary_ip6. A device_ prefix was a lie on every VM row, so
    // the column lost the namespace along with the device-only reading.
    // The rename is hard and aliasless — the plugin is unreleased — and this
    // picker is a closed vocabulary, so still offering `device_is_primary_ip`
    // would render a permanently blank column rather than the old behaviour.
    const identity = IP_CONTEXT_FIELD_GROUPS.find((g) => g.label === 'Identity');
    expect(identity?.options.map((o) => o.value)).toContain('is_primary_ip');
    const all = IP_CONTEXT_FIELD_GROUPS.flatMap((g) => g.options.map((o) => o.value));
    expect(all).not.toContain('device_is_primary_ip');
  });

  it('labels every option by its full column name, so chips are unambiguous', () => {
    // @grafana/ui resolves a MultiSelect's selected chips against the option
    // list. With group-local labels the default selection rendered two adjacent
    // chips both reading "name" (device_name and interface_name), and a
    // nine-column panel read "match_count cidr scope tenant role vlan name
    // is_primary_ip name" — unreadable, and wrong about which columns are
    // shown. That render predates the rename: its "is_primary_ip" chip was
    // device_is_primary_ip shown group-locally, and is now the real name.
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

describe('QueryEditor — Sort by (NetBox-side ordering)', () => {
  // Opens a Select's menu and scopes the assertions to it. Unscoped queries
  // would match the same names elsewhere on the form (filter pickers, the
  // Return fields list) and pass without the menu offering anything.
  async function openMenu(el: HTMLElement) {
    fireEvent.focus(el);
    fireEvent.keyDown(el, { key: 'ArrowDown', keyCode: 40, code: 'ArrowDown' });
    return within(await screen.findByRole('listbox'));
  }

  it('offers only the fields NetBox is known to sort, not the object type’s columns', async () => {
    // getFields answers with the object type's real columns, and dcim/sites'
    // `device_count` is exactly the trap: NetBox sorts on it happily until a
    // `?fields=` projection that omits it is present, and then it is a 500 —
    // which of the two a query sends depends on the columns the panel selected,
    // not on anything this picker can see. A picker built from the schema (or
    // from a hand-extended list) hands the user that footgun.
    const ds = {
      ...datasource,
      uid: 'ds-sort-sites',
      getFields: jest
        .fn()
        .mockResolvedValue([{ name: 'device_count' }, { name: 'name' }, { name: 'asn' }, { name: 'time_zone' }]),
    } as any;
    setup({ objectType: 'dcim/sites' }, ds);

    const menu = await openMenu(await screen.findByLabelText('Sort by'));

    for (const allowed of ['id', 'name', 'slug', 'region', 'facility']) {
      expect(menu.getByText(allowed)).toBeInTheDocument();
    }
    for (const denied of ['device_count', 'asn', 'time_zone']) {
      expect(menu.queryByText(denied)).not.toBeInTheDocument();
    }
  });

  it('offers the list for the selected object type, not one fixed list', async () => {
    const ds = { ...datasource, uid: 'ds-sort-prefixes' } as any;
    setup({ objectType: 'ipam/prefixes' }, ds);

    const menu = await openMenu(await screen.findByLabelText('Sort by'));

    for (const allowed of ['prefix', 'vrf', 'tenant']) {
      expect(menu.getByText(allowed)).toBeInTheDocument();
    }
    // dcim/devices' list, which a picker ignoring objectType would show here.
    for (const otherType of ['name', 'role', 'device_type', 'last_updated']) {
      expect(menu.queryByText(otherType)).not.toBeInTheDocument();
    }
  });

  it('renders no sort control at all for an object type NetBox will not sort', async () => {
    const ds = { ...datasource, uid: 'ds-sort-none' } as any;
    setup({ objectType: 'plugins/bgp/bgp-sessions' }, ds);
    await screen.findByText('Add filter'); // mount effects settled

    expect(screen.queryByLabelText('Sort by')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('ordering-direction')).not.toBeInTheDocument();
  });

  it('stores the picked field bare — no ,id tiebreaker, no direction', async () => {
    const { onChange, onRunQuery } = setup({}, { ...datasource, uid: 'ds-sort-pick' } as any);

    const menu = await openMenu(await screen.findByLabelText('Sort by'));
    fireEvent.click(menu.getByText('name'));

    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ ordering: 'name' }));
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('asks for descending with a leading -', async () => {
    const { onChange, onRunQuery } = setup({ ordering: 'last_updated' }, { ...datasource, uid: 'ds-sort-desc' } as any);

    const menu = await openMenu(await screen.findByLabelText('ordering-direction'));
    fireEvent.click(menu.getByText('descending'));

    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ ordering: '-last_updated' }));
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('shows a stored descending sort as its field plus a direction', async () => {
    setup({ ordering: '-last_updated' }, { ...datasource, uid: 'ds-sort-stored' } as any);

    // Query only after the mount effects have settled: react-select replaces
    // its rendered value node when options change mid-mount, so an element
    // captured before that is detached by the time it is asserted on (the same
    // hazard the filter-row test above documents).
    await screen.findByLabelText('Sort by');
    expect(screen.getByText('last_updated')).toBeInTheDocument();
    expect(screen.getByText('descending')).toBeInTheDocument();
  });

  it('offers no direction until a field is chosen', async () => {
    setup({}, { ...datasource, uid: 'ds-sort-nodir' } as any);
    await screen.findByLabelText('Sort by');

    expect(screen.queryByLabelText('ordering-direction')).not.toBeInTheDocument();
  });

  it('clears back to NetBox’s own order', async () => {
    const { onChange } = setup({ ordering: 'name' }, { ...datasource, uid: 'ds-sort-clear' } as any);

    // Backspace on an empty input, rather than the clear icon: the scaffolded
    // react-inlinesvg mock (.config/jest/mocks) renders every Icon as a bare
    // <svg data-testid>, dropping the role and aria-label the real one carries,
    // so the icon is unreachable by role in jsdom. Backspace is the same
    // clearing gesture and goes through the same isClearable path.
    fireEvent.keyDown(await screen.findByLabelText('Sort by'), { key: 'Backspace', keyCode: 8, code: 'Backspace' });

    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ ordering: undefined }));
  });

  it('drops a stale sort when the object type changes', async () => {
    // `role` is sortable on devices and not on prefixes. Carrying it over would
    // be dropped upstream, so the panel would silently show unsorted rows.
    const ds = {
      ...datasource,
      uid: 'ds-sort-switch',
      getObjectTypes: jest.fn().mockResolvedValue([
        { value: 'dcim/devices', label: 'Devices', app: 'dcim', model: 'devices' },
        { value: 'ipam/prefixes', label: 'Prefixes', app: 'ipam', model: 'prefixes' },
      ]),
    } as any;
    const { onChange } = setup({ ordering: 'role' }, ds);

    const menu = await openMenu(await screen.findByLabelText('Object type'));
    fireEvent.click(menu.getByText('Prefixes'));

    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ objectType: 'ipam/prefixes', ordering: undefined })
    );
  });

  it('hides the sort control for a count query — a single number has no order', async () => {
    setup({ count: true }, { ...datasource, uid: 'ds-sort-count' } as any);
    await screen.findByText('Add filter');

    expect(screen.queryByLabelText('Sort by')).not.toBeInTheDocument();
  });

  it('hides the sort control for non-objects query types', async () => {
    setup({ queryType: 'topology' }, { ...datasource, uid: 'ds-sort-topology' } as any);
    await screen.findByText(/node graph/i);

    expect(screen.queryByLabelText('Sort by')).not.toBeInTheDocument();
  });

  it('disables the sort control when the data source pages by cursor', async () => {
    // NetBox 400s on ?ordering= together with ?start= ("Ordering cannot be
    // specified in conjunction with cursor pagination"), so with fast paging on
    // the sort can never reach NetBox. Offering it live would be a dead control.
    const ds = {
      ...datasource,
      uid: 'ds-sort-fastpaging',
      datasourceInstanceSettings: { jsonData: { fastPagingNoTotals: true } },
    } as any;
    setup({ ordering: 'name' }, ds);

    const picker = await screen.findByLabelText('Sort by');
    await waitFor(() => expect(picker).toBeDisabled());
    expect(await screen.findByLabelText('ordering-direction')).toBeDisabled();
  });

  it('keeps the sort control live for an alert-table query on a fast-paging data source', async () => {
    // The disable above is about the CURSOR walk, and an alert-table query never
    // takes it: pkg/plugin/query.go's alertTable branch leaves AllowUncounted
    // false, so netbox.go's `p.cursorPaging && spec.AllowUncounted &&
    // cursorLegal(q)` is false and ?ordering= is sent with no ?start= alongside
    // it. Disabling here made the picker dead for a query NetBox does sort, and
    // told the user the reason was a limit that does not apply to it.
    const ds = {
      ...datasource,
      uid: 'ds-sort-alerttable-fastpaging',
      datasourceInstanceSettings: { jsonData: { fastPagingNoTotals: true } },
    } as any;
    setup({ ordering: 'name', alertTable: true }, ds);

    // Assert the combination actually rendered before asserting on it. Both
    // halves have to be real: the switch proves this is the alert-table shape,
    // and the sibling test above proves the same jsonData disables the picker
    // for every other shape — so `not.toBeDisabled()` here cannot pass by the
    // fast-paging setting having quietly gone missing.
    expect(await screen.findByRole('switch', { name: /Alert table/i })).toBeChecked();
    expect(await screen.findByLabelText('Sort by')).not.toBeDisabled();
    expect(await screen.findByLabelText('ordering-direction')).not.toBeDisabled();
  });

  it('keeps the sort control live when fast paging is off', async () => {
    const ds = {
      ...datasource,
      uid: 'ds-sort-nofastpaging',
      datasourceInstanceSettings: { jsonData: { fastPagingNoTotals: false } },
    } as any;
    setup({ ordering: 'name' }, ds);

    expect(await screen.findByLabelText('Sort by')).not.toBeDisabled();
  });

  it('survives a datasource that exposes no instance settings', async () => {
    const ds = { ...datasource, uid: 'ds-sort-nosettings', datasourceInstanceSettings: undefined } as any;
    setup({}, ds);

    expect(await screen.findByLabelText('Sort by')).not.toBeDisabled();
  });

  // Tooltip copy is asserted on the constant, not the DOM: Grafana mounts a
  // tooltip's text only on hover, which jsdom does not reproduce, so a DOM test
  // here would pass for the wrong reason (see ConfigEditor.test.tsx).
  it('tells the user what a sort costs, briefly', () => {
    expect(ORDERING_TOOLTIP).toMatch(/6\.8M/);
    expect(ORDERING_TOOLTIP).toMatch(/27\.6s/);
    // A paragraph in this editor breaks the layout; the (i) tooltip is the
    // established vehicle and Fast paging's is the longest one worth having.
    expect(ORDERING_TOOLTIP.length).toBeLessThan(FAST_PAGING_TOOLTIP.length);
    // The disabled copy has to name the setting to change, since nothing else
    // on this screen explains why the control is dead.
    expect(ORDERING_DISABLED_TOOLTIP).toMatch(/fast paging/i);
    expect(ORDERING_DISABLED_TOOLTIP).toMatch(/data source settings/i);
    // ...and it has to blame THIS query, not the data source. The picker is live
    // for an alert-table query on that same data source (test above), so copy
    // saying the data source cannot sort is refuted one control away.
    expect(ORDERING_DISABLED_TOOLTIP).toMatch(/this query/i);
    expect(ORDERING_DISABLED_TOOLTIP).not.toMatch(/cannot sort a fast-paged query/i);
    // Same layout budget as the tooltip above: a paragraph here breaks the row.
    expect(ORDERING_DISABLED_TOOLTIP.length).toBeLessThan(FAST_PAGING_TOOLTIP.length);
  });
});

// The edges shape runs the same traversal as the node-graph one and takes the
// same controls; only the output differs. Gating them on `queryType ===
// 'topology'` alone would leave the new type with no way to scope its device
// set — an unfiltered fleet-wide query on an alert path.
describe('QueryEditor — Topology edges', () => {
  it('offers the traversal controls, as the node-graph topology query does', () => {
    setup({ queryType: 'topology-edges', objectType: undefined });
    expect(screen.getByText(/Connections/i)).toBeInTheDocument();
    expect(screen.getByText(/Connected only/i)).toBeInTheDocument();
  });

  it('labels its filters as device filters and can scope the device set', () => {
    setup({
      queryType: 'topology-edges',
      objectType: undefined,
      filters: [{ field: 'site', operator: '', value: 'AMS1' }],
    });
    // "Device filter" rather than plain "Filter": the rows scope the devices the
    // traversal starts from, not an object query.
    expect(screen.getByText(/Device filter/i)).toBeInTheDocument();
  });
});

// The edges query returns a joinable table, not node/edge frames. Telling the
// user to reach for the Node Graph visualisation would send them to one that
// cannot render it.
describe('QueryEditor — Topology edges guidance', () => {
  it('points at a table and the alert recipe, not the node graph', () => {
    setup({ queryType: 'topology-edges', objectType: undefined });
    expect(screen.getByText(/Table visualization/i)).toBeInTheDocument();
    expect(screen.queryByText(/Node\s*Graph visualization/i)).not.toBeInTheDocument();
  });

  it('still points the node-graph query at the node graph', () => {
    setup({ queryType: 'topology', objectType: undefined });
    expect(screen.getByText(/Node\s*Graph visualization/i)).toBeInTheDocument();
  });
});
