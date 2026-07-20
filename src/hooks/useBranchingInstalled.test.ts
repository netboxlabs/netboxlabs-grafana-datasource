import { renderHook, waitFor } from '@testing-library/react';
import { useBranchingInstalled } from './useBranchingInstalled';

// No client-side result cache: each test's stub is probed on mount / on change.
function ds(uid: string, getBranchingInstalled: jest.Mock): any {
  return { uid, getBranchingInstalled };
}

// The datasource prop must be a STABLE reference across renders (Grafana
// memoizes it); create each stub once, outside the render callback.
it('resolves true when branching is installed', async () => {
  const d = ds('a', jest.fn().mockResolvedValue(true));
  const { result } = renderHook(() => useBranchingInstalled(d));
  expect(result.current).toBeUndefined(); // unknown while the probe is in flight
  await waitFor(() => expect(result.current).toBe(true));
});

it('resolves false when branching is not installed', async () => {
  const d = ds('b', jest.fn().mockResolvedValue(false));
  const { result } = renderHook(() => useBranchingInstalled(d));
  await waitFor(() => expect(result.current).toBe(false));
});

it('fails open to true when the probe rejects', async () => {
  const d = ds('c', jest.fn().mockRejectedValue(new Error('boom')));
  const { result } = renderHook(() => useBranchingInstalled(d));
  await waitFor(() => expect(result.current).toBe(true));
});

it('does not inherit the previous datasource verdict on switch (fail-open)', async () => {
  const { result, rerender } = renderHook(({ d }) => useBranchingInstalled(d), {
    initialProps: { d: ds('sw-a', jest.fn().mockResolvedValue(false)) },
  });
  await waitFor(() => expect(result.current).toBe(false)); // A: branching absent -> disabled
  // Switch to a different (uncached) datasource. Must NOT keep reading A's
  // `false` — that would disable the field while B's status is unknown.
  rerender({ d: ds('sw-b', jest.fn().mockResolvedValue(true)) });
  expect(result.current).not.toBe(false); // fail-open: unknown (or true), never stale false
  await waitFor(() => expect(result.current).toBe(true)); // B resolves -> installed
});

it('re-probes when the datasource is repointed at a different NetBox (same uid)', async () => {
  // The reported bug: repoint a datasource (config edit) from branching-absent
  // to branching-present, then reopen an editor. There is no client-side result
  // cache to go stale, so the new probe wins — not a leftover `false`.
  const oldFn = jest.fn().mockResolvedValue(false);
  const { result, rerender } = renderHook(({ d }) => useBranchingInstalled(d), {
    initialProps: { d: ds('same-uid', oldFn) },
  });
  await waitFor(() => expect(result.current).toBe(false));
  const newFn = jest.fn().mockResolvedValue(true);
  rerender({ d: ds('same-uid', newFn) }); // repointed datasource
  expect(result.current).not.toBe(false); // fail-open, not stale false
  await waitFor(() => expect(result.current).toBe(true));
  expect(newFn).toHaveBeenCalledTimes(1);
});
