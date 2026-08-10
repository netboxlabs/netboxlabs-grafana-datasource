import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import React from 'react';
import { VariableQueryEditor } from './VariableQueryEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({ replace: (s: string) => (s === '$branch' ? 'td5smq0f' : s) }),
}));

const dsMock = {
  uid: 'ds-variable-editor',
  getObjectTypes: jest.fn().mockResolvedValue([]),
  getFields: jest.fn().mockResolvedValue([]),
  getBranchingInstalled: jest.fn().mockResolvedValue(true),
} as any;

// Each filter row is rendered as its own Stack that is a direct child of the
// editor's single root Stack (see VariableQueryEditor.tsx's per-row `<Stack
// key={i} direction="column">` wrapper). Walking up from any element inside a
// row until its parent is that root returns exactly that row's container,
// without depending on how many internal divs react-select nests `el` under.
function rowContainerOf(container: HTMLElement, el: HTMLElement): HTMLElement {
  const root = container.firstElementChild as HTMLElement;
  let node: HTMLElement = el;
  while (node.parentElement && node.parentElement !== root) {
    node = node.parentElement;
  }
  return node;
}

it('sets the branch on the variable query', async () => {
  const onChange = jest.fn();
  render(<VariableQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
  // await find* so the mount effects (getObjectTypes/getFields) flush inside act()
  const input = await screen.findByLabelText('Branch');
  fireEvent.change(input, { target: { value: 'td5smq0f' } });
  expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ branch: 'td5smq0f' }));
});

it('disables the Branch field when branching is not installed', async () => {
  const ds = { ...dsMock, uid: 'ds-var-absent', getBranchingInstalled: jest.fn().mockResolvedValue(false) };
  render(<VariableQueryEditor query={{ refId: 'A' } as any} onChange={jest.fn()} datasource={ds} />);
  const input = await screen.findByLabelText('Branch');
  await waitFor(() => expect(input).toBeDisabled());
});

it('warns when a variable filter row has a field but no value', async () => {
  render(
    <VariableQueryEditor
      query={{ refId: 'A', filters: [{ field: 'site', operator: '', value: '' }] } as any}
      onChange={jest.fn()}
      datasource={dsMock}
    />
  );
  expect(await screen.findByText(/isn't applied/i)).toBeInTheDocument();

  // Severity must come from issue.severity, not be hardcoded: @grafana/ui's
  // Alert renders info/success with role="status" and warning/error with
  // role="alert" (see Alert.mjs's `rolesBySeverity` map), so a not-yet-filled
  // row's message must be reachable via the "status" role and NOT "alert".
  expect(await screen.findByRole('status', { name: /isn't applied/i })).toBeInTheDocument();
  expect(screen.queryByRole('alert', { name: /isn't applied/i })).not.toBeInTheDocument();
});

it('warns that colliding variable filter rows are OR-ed, naming the param', async () => {
  render(
    <VariableQueryEditor
      query={
        {
          refId: 'A',
          filters: [
            { field: 'site', operator: '', value: 'ams01' },
            { field: 'site', operator: '', value: 'ams02' },
          ],
        } as any
      }
      onChange={jest.fn()}
      datasource={dsMock}
    />
  );
  const msgs = await screen.findAllByText(/site/i);
  expect(msgs.length).toBeGreaterThan(0);

  // A collision is a 'warning', the opposite end of the severity ladder from
  // the no-value case above: it must render via role="alert", not "status".
  const alerts = await screen.findAllByRole('alert', { name: /site/i });
  expect(alerts.length).toBeGreaterThan(0);
  expect(alerts[0]).toHaveTextContent(/OR/);
  expect(screen.queryByRole('status', { name: /site/i })).not.toBeInTheDocument();
});

it('attributes a variable filter row message to that row only, not to every row', async () => {
  const { container } = render(
    <VariableQueryEditor
      query={
        {
          refId: 'A',
          filters: [
            { field: 'site', operator: '', value: '' }, // has an issue
            { field: 'tenant', operator: '', value: 'acme' }, // valid, no issue
          ],
        } as any
      }
      onChange={jest.fn()}
      datasource={dsMock}
    />
  );
  await screen.findByText(/isn't applied/i);
  // Anchor on each row's selected field label (there's no per-index aria-label
  // on this editor's field Select, unlike QueryEditor's `filter-field-{i}`).
  const row0Anchor = await screen.findByText('site');
  const row1Anchor = await screen.findByText('tenant');

  const row0 = rowContainerOf(container, row0Anchor);
  const row1 = rowContainerOf(container, row1Anchor);

  expect(within(row0).getByText(/isn't applied/i)).toBeInTheDocument();
  expect(within(row1).queryByText(/isn't applied/i)).not.toBeInTheDocument();
});
