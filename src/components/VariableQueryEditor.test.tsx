import { render, screen, fireEvent } from '@testing-library/react';
import React from 'react';
import { VariableQueryEditor } from './VariableQueryEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => ({ replace: (s: string) => (s === '$branch' ? 'td5smq0f' : s) }),
}));

const dsMock = {
  getObjectTypes: jest.fn().mockResolvedValue([]),
  getFields: jest.fn().mockResolvedValue([]),
} as any;

it('sets the branch on the variable query', async () => {
  const onChange = jest.fn();
  render(<VariableQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
  // await find* so the mount effects (getObjectTypes/getFields) flush inside act()
  const input = await screen.findByLabelText('Branch');
  fireEvent.change(input, { target: { value: 'td5smq0f' } });
  expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ branch: 'td5smq0f' }));
});
