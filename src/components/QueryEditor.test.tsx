import React from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryEditor } from './QueryEditor';

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
