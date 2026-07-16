import React from 'react';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { QueryEditor } from './QueryEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({ replace: (s: string) => (s === '$branch' ? 'td5smq0f' : s) }),
}));

// Minimal datasource stub: the editor calls these in effects on mount.
const datasource = {
  getObjectTypes: jest
    .fn()
    .mockResolvedValue([{ value: 'dcim/devices', label: 'Devices', app: 'dcim', model: 'devices' }]),
  getFields: jest.fn().mockResolvedValue([{ name: 'name' }, { name: 'status' }]),
} as any;

function setup(queryOverrides: Record<string, unknown> = {}) {
  const onChange = jest.fn();
  const onRunQuery = jest.fn();
  const query = { refId: 'A', queryType: 'objects', objectType: 'dcim/devices', ...queryOverrides } as any;
  render(
    <QueryEditor query={query} onChange={onChange} onRunQuery={onRunQuery} datasource={datasource} />
  );
  return { onChange, onRunQuery };
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

    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ alertTable: false, valueField: undefined })
    );
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
