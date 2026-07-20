import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import React from 'react';
import { AnnotationQueryEditor } from './AnnotationQueryEditor';

const dsMock = {
  uid: 'ds-annotation-editor',
  getBranchingInstalled: jest.fn().mockResolvedValue(true),
} as any;

it('sets the branch on the annotation query', async () => {
  const onChange = jest.fn();
  render(<AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
  const input = await screen.findByLabelText('Branch');
  fireEvent.change(input, { target: { value: 'td5smq0f' } });
  expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ branch: 'td5smq0f' }));
});

it('disables the Branch field when branching is not installed', async () => {
  const ds = { ...dsMock, uid: 'ds-anno-absent', getBranchingInstalled: jest.fn().mockResolvedValue(false) };
  render(<AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={jest.fn()} datasource={ds} />);
  const input = await screen.findByLabelText('Branch');
  await waitFor(() => expect(input).toBeDisabled());
});
