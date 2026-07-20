import { useEffect, useState } from 'react';

import { DataSource } from '../datasource';

/** Tooltip for the Branch field when branching is available (the normal case). */
export const BRANCH_FIELD_TOOLTIP =
  'Optional netbox-branching schema id, or a $variable (e.g. from a variable querying plugins/branching/branches). Empty targets the main branch. Requires the netbox-branching plugin.';

/** Tooltip shown while the Branch field is disabled (branching not installed). */
export const BRANCH_FIELD_DISABLED_TOOLTIP =
  'The netbox-branching plugin is not installed on the connected NetBox, so branch selection is unavailable.';

/**
 * Probes whether netbox-branching is installed on the datasource's NetBox and
 * returns a tri-state:
 *   - undefined: unknown (probe in flight) — callers should treat as enabled
 *   - true:      installed
 *   - false:     not installed
 *
 * Fails OPEN: any error resolves to `true`, so a transient failure never
 * disables branch UI. Callers should disable the Branch field only on an
 * explicit `false` (never on `undefined`), which also avoids a mount-time
 * enable→disable flicker.
 *
 * No client-side result cache on purpose: `getBranchingInstalled()` always hits
 * the backend, which caches per datasource instance and rebuilds that instance
 * (fresh probe) whenever the datasource config changes — so re-probing here is
 * cheap and, crucially, never serves a stale verdict after a config edit. A
 * frontend cache keyed by uid or instance couldn't be invalidated reliably when
 * the same in-session instance is repointed at a different NetBox (it would keep
 * the field wrongly disabled/enabled until a full page reload). The effect is
 * keyed on the datasource only (not the query), so it probes once per editor
 * mount / datasource change, not per keystroke.
 */
export function useBranchingInstalled(datasource: DataSource): boolean | undefined {
  const [installed, setInstalled] = useState<boolean | undefined>(undefined);
  const [probed, setProbed] = useState<DataSource | undefined>(undefined);

  // Reset to "unknown" the moment the datasource changes, so switching reads as
  // unknown => ENABLED (fail open) rather than inheriting the previous
  // datasource's verdict while the new probe is in flight. React's documented
  // "reset state when a prop changes" pattern: adjusting state during render
  // re-renders synchronously, so no stale (wrongly-disabled) frame paints.
  if (datasource !== probed) {
    setProbed(datasource);
    setInstalled(undefined);
  }

  useEffect(() => {
    let active = true;
    datasource
      .getBranchingInstalled()
      .then((v) => {
        if (active) {
          setInstalled(v);
        }
      })
      .catch(() => {
        if (active) {
          setInstalled(true); // fail open
        }
      });
    return () => {
      active = false;
    };
  }, [datasource]);

  return installed;
}
