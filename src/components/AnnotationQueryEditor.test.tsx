import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import React from 'react';
import { AnnotationQueryEditor } from './AnnotationQueryEditor';

// jsdom lacks IntersectionObserver, which @grafana/ui's Select menu needs to open.
class IntersectionObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
(globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = IntersectionObserverStub;

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

describe('scope fields', () => {
  it('commits the scope on blur, not on every keystroke', async () => {
    const onChange = jest.fn();
    render(<AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
    const input = await screen.findByLabelText('Scope');
    fireEvent.change(input, { target: { value: '$device' } });
    expect(onChange).not.toHaveBeenCalled();
    fireEvent.blur(input);
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ scope: '$device' }));
  });

  it('commits the scope on Enter', async () => {
    const onChange = jest.fn();
    render(<AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
    const input = await screen.findByLabelText('Scope');
    fireEvent.change(input, { target: { value: 'leaf1' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ scope: 'leaf1' }));
  });

  it('sets scopeMatch when ID is chosen', async () => {
    const onChange = jest.fn();
    render(<AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
    fireEvent.click(await screen.findByLabelText('ID'));
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ scopeMatch: 'id' }));
  });

  it('sets journal from the switch', async () => {
    const onChange = jest.fn();
    render(<AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
    fireEvent.click(await screen.findByLabelText('Journal entries'));
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ journal: true }));
  });

  it('shows Include descendants, on by default, for a scoped device', async () => {
    render(
      <AnnotationQueryEditor query={{ refId: 'A', scope: 'leaf1' } as any} onChange={jest.fn()} datasource={dsMock} />
    );
    expect(await screen.findByLabelText('Include descendants')).toBeChecked();
  });

  it('turns descendants off from the switch', async () => {
    const onChange = jest.fn();
    render(
      <AnnotationQueryEditor query={{ refId: 'A', scope: 'leaf1' } as any} onChange={onChange} datasource={dsMock} />
    );
    fireEvent.click(await screen.findByLabelText('Include descendants'));
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ includeDescendants: false }));
  });

  it('shows the switch unchecked when includeDescendants is false', async () => {
    const q = { refId: 'A', scope: 'x', includeDescendants: false } as any;
    render(<AnnotationQueryEditor query={q} onChange={jest.fn()} datasource={dsMock} />);
    expect(await screen.findByLabelText('Include descendants')).not.toBeChecked();
  });

  it('hides Include descendants without a scope or for other scope types', async () => {
    const { rerender } = render(
      <AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={jest.fn()} datasource={dsMock} />
    );
    await screen.findByLabelText('Journal entries');
    expect(screen.queryByLabelText('Include descendants')).toBeNull();
    rerender(
      <AnnotationQueryEditor
        query={{ refId: 'A', scope: 'x', scopeType: 'dcim.site' } as any}
        onChange={jest.fn()}
        datasource={dsMock}
      />
    );
    expect(screen.queryByLabelText('Include descendants')).toBeNull();
  });

  it('normalises the scope type before deciding to show Include descendants', async () => {
    const q = { refId: 'A', scope: 'x', scopeType: ' Virtualization.VMInterface ' } as any;
    render(<AnnotationQueryEditor query={q} onChange={jest.fn()} datasource={dsMock} />);
    expect(await screen.findByLabelText('Include descendants')).toBeInTheDocument();
  });

  it('sets actions from the Actions select', async () => {
    const onChange = jest.fn();
    render(<AnnotationQueryEditor query={{ refId: 'A' } as any} onChange={onChange} datasource={dsMock} />);
    const input = await screen.findByLabelText('Actions');
    fireEvent.focus(input);
    fireEvent.keyDown(input, { key: 'ArrowDown', keyCode: 40 });
    fireEvent.click(await screen.findByText('Updated'));
    expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ actions: ['update'] }));
  });
});
