import { test } from "node:test";
import assert from "node:assert/strict";

import { Audit } from "../audit.js";
import { runHousekeeping, type HousekeepingDeps } from "../housekeeping.js";
import { tempStore } from "./helpers.js";

function delayedDeps() {
  const store = tempStore();
  let runs = 0;
  const deps: HousekeepingDeps = {
    audit: new Audit(store),
    store,
    backupDir: "/unused",
    backup: async () => {
      runs++;
      await new Promise((r) => setTimeout(r, 30));
      return `/state/backups/policy-${runs}.sqlite`;
    },
  };
  return { deps, runs: () => runs };
}

test("overlapping runs share one backup and one result", async () => {
  const { deps, runs } = delayedDeps();
  const [a, b] = await Promise.all([runHousekeeping(deps), runHousekeeping(deps)]);
  assert.equal(runs(), 1);
  assert.equal(a, b);
  assert.deepEqual(a, { pruned: 0, backup: "/state/backups/policy-1.sqlite" });

  // Settled: the next call is a new run.
  const c = await runHousekeeping(deps);
  assert.equal(runs(), 2);
  assert.equal(c.backup, "/state/backups/policy-2.sqlite");
});

test("a failed run rejects every waiter and does not stick", async () => {
  const { deps } = delayedDeps();
  const failing = { ...deps, backup: async () => Promise.reject(new Error("disk full")) };
  await Promise.all([
    assert.rejects(runHousekeeping(failing), /disk full/),
    assert.rejects(runHousekeeping(failing), /disk full/),
  ]);
  // A synchronous throw before the first await must not pin the rejection either.
  const throwing = {
    ...deps,
    backup: () => {
      throw new Error("sync");
    },
  } as unknown as HousekeepingDeps;
  await assert.rejects(runHousekeeping(throwing), /sync/);
  assert.equal((await runHousekeeping(deps)).backup, "/state/backups/policy-1.sqlite");
});
