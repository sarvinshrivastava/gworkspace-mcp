import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { DAY_MS } from "../audit.js";
import { vaultFetchOptional } from "../config.js";
import { startHousekeeping } from "../housekeeping.js";
import { quietly, tempDir } from "./helpers.js";

const entry = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "index.js");

/** Runs the real entry point until it exits; returns its exit code and output. */
function boot(env: Record<string, string>): Promise<{ code: number | null; output: string }> {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [entry], {
      env: { PATH: process.env.PATH ?? "", PORT: "0", ADMIN_PORT: "0", ...env },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    child.stdout.on("data", (d) => (output += d));
    child.stderr.on("data", (d) => (output += d));
    const timer = setTimeout(() => child.kill("SIGKILL"), 20_000);
    child.on("exit", (code) => {
      clearTimeout(timer);
      resolve({ code, output });
    });
  });
}

test("a database that cannot be created boots degraded", { timeout: 30_000 }, async () => {
  const dir = tempDir();
  const { code, output } = await boot({ DB_PATH: path.join(dir, "missing", "p.sqlite"), BACKUP_DIR: path.join(dir, "b") });
  assert.match(output, /\[db\] DEGRADED/);
  // It then reaches the vault, which this test does not provide.
  assert.match(output, /SECRETS_MANAGER_TOKEN is empty/);
  assert.doesNotMatch(output, /policy DB exists/);
  assert.equal(code, 1);
});

test("an existing database that cannot be opened is fatal", { timeout: 30_000 }, async () => {
  const dir = tempDir();
  const file = path.join(dir, "p.sqlite");
  writeFileSync(file, Buffer.alloc(8192, 7));
  const { code, output } = await boot({ DB_PATH: file, BACKUP_DIR: path.join(dir, "b") });
  assert.match(output, /FATAL: policy DB exists/);
  assert.doesNotMatch(output, /listening/);
  assert.equal(code, 1);
});

test("vaultFetchOptional: 404 is empty, 200 is the value, 401 throws", async () => {
  const { config } = await import("../config.js");
  const original = globalThis.fetch;
  config.vault.token = "t";
  try {
    globalThis.fetch = (async () => new Response("", { status: 404 })) as typeof fetch;
    const warn = console.warn;
    console.warn = () => undefined;
    try {
      assert.equal(await vaultFetchOptional("K"), "");
    } finally {
      console.warn = warn;
    }
    globalThis.fetch = (async () => new Response(JSON.stringify({ value: "v" }), { status: 200 })) as typeof fetch;
    assert.equal(await vaultFetchOptional("K"), "v");
    globalThis.fetch = (async () => new Response("", { status: 401 })) as typeof fetch;
    await assert.rejects(vaultFetchOptional("K"), /HTTP 401/);
  } finally {
    globalThis.fetch = original;
    config.vault.token = "";
  }
});

test("HOUSEKEEPING_SCHEDULER outside internal|external is fatal at boot", { timeout: 30_000 }, async () => {
  const dir = tempDir();
  const { code, output } = await boot({
    DB_PATH: path.join(dir, "p.sqlite"),
    BACKUP_DIR: path.join(dir, "b"),
    HOUSEKEEPING_SCHEDULER: "bogus",
  });
  assert.match(output, /FATAL: HOUSEKEEPING_SCHEDULER must be one of internal, external/);
  assert.doesNotMatch(output, /listening/);
  assert.equal(code, 1);
});

const RESULT = { pruned: 0, backup: null };

test("startHousekeeping external: the boot run only, no interval", () => {
  let runs = 0;
  const scheduled: number[] = [];
  startHousekeeping({
    scheduler: "external",
    schedule: (_fn, ms) => scheduled.push(ms),
    run: async () => (runs++, RESULT),
  });
  assert.equal(runs, 1);
  assert.deepEqual(scheduled, []);
});

test("startHousekeeping internal: the boot run plus a daily interval that runs it", async () => {
  let runs = 0;
  const scheduled: { fn: () => void; ms: number }[] = [];
  const { logs } = await quietly(async () => {
    startHousekeeping({
      scheduler: "internal",
      schedule: (fn, ms) => scheduled.push({ fn, ms }),
      run: async () => {
        runs++;
        if (runs > 1) throw new Error("disk full");
        return RESULT;
      },
    });
    // The boot run happens at once; the interval only after it.
    assert.equal(runs, 1);
    assert.deepEqual(scheduled.map((s) => s.ms), [DAY_MS]);
    // The daily tick: a failure is logged, never an unhandled rejection.
    scheduled[0]!.fn();
    await new Promise((r) => setImmediate(r));
  });
  assert.equal(runs, 2);
  assert.ok(logs.some((l) => l.includes("housekeeping failed: disk full")), logs.join("\n"));
});

test("startHousekeeping: a failing run is logged, never thrown", async () => {
  const { logs } = await quietly(async () => {
    startHousekeeping({ scheduler: "external", run: () => Promise.reject(new Error("disk full")) });
    await new Promise((r) => setImmediate(r));
  });
  assert.ok(logs.some((l) => l.includes("housekeeping failed: disk full")), logs.join("\n"));
});
