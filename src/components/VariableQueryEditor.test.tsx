import { render, screen, fireEvent, waitFor } from '@testing-library/react';
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
