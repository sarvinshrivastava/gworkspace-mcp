/**
 * Daily housekeeping: prune the audit log, then back up the policy DB.
 *
 * Idempotent — backupDaily keeps a copy already written today (UTC) — so a
 * scheduler that fires twice, or retries, needs no run-id dedupe.
 *
 * Who schedules it is HOUSEKEEPING_SCHEDULER: `internal` (a boot-relative
 * 24 h interval in this process) or `external` (pipeline-runner calls
 * POST /internal/housekeeping). Either way it runs once at boot.
 */

import { DAY_MS, type Audit } from "./audit.js";
import { errorMessage } from "./errors.js";
import { backupDaily, type PolicyStore } from "./policy/store.js";

export type HousekeepingResult = { pruned: number; backup: string | null };

export type HousekeepingDeps = {
  audit: Audit;
  store: PolicyStore;
  backupDir: string;
  /** A seam for tests; the real backup otherwise. */
  backup?: typeof backupDaily;
};

// Single flight. Two overlapping runs (the boot run and a pipeline-runner
// call, say) would share one `<target>.tmp`, and the second run's rmSync of it
// deletes the first run's copy mid-backup. A caller that arrives during a run
// gets that run's result instead.
let inflight: Promise<HousekeepingResult> | null = null;

/** Rejects when the backup fails, so the endpoint answers 500 and the caller retries. */
export function runHousekeeping(deps: HousekeepingDeps): Promise<HousekeepingResult> {
  // Cleared by .finally on the promise, never inside the async body: a body
  // that throws before its first await would clear it BEFORE the assignment,
  // pinning a rejected promise here for good.
  inflight ??= (async () => {
    const pruned = deps.audit.prune();
    const backup = await (deps.backup ?? backupDaily)(deps.store, deps.backupDir);
    return { pruned, backup };
  })().finally(() => {
    inflight = null;
  });
  return inflight;
}

export function startHousekeeping({
  scheduler,
  schedule = (fn, ms) => setInterval(fn, ms).unref(),
  run,
}: {
  scheduler: "internal" | "external";
  schedule?: (fn: () => void, ms: number) => unknown;
  run: () => Promise<HousekeepingResult>;
}): void {
  const once = () => {
    run().catch((error) => console.error(`[db] housekeeping failed: ${errorMessage(error)}`));
  };
  once();
  if (scheduler === "internal") schedule(once, DAY_MS);
}
