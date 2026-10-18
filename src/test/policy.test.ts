import { test } from "node:test";
import assert from "node:assert/strict";
import { chmodSync, existsSync, mkdirSync, readFileSync, statSync, utimesSync, writeFileSync } from "node:fs";
import path from "node:path";
import { DatabaseSync } from "node:sqlite";

import {
  DEFAULT_GRANTS,
  SCOPED_WITHHELD,
  TOOL_MAP,
  deriveTools,
  enabledTools,
} from "../policy/derive.js";
import { makeAuthenticator, makeDegradedAuthenticator } from "../policy/auth.js";
import { hashToken, mintToken } from "../policy/tokens.js";
import {
  OpenError,
  PolicyStore,
  StoreError,
  backupDaily,
  openPolicyStore,
} from "../policy/store.js";
import { MIGRATIONS } from "../policy/schema.js";
import { TODAYS_TOOLS, quietly, tempDir, tempStore } from "./helpers.js";

const none = { scoped: new Set<string>(), all: new Set<string>() };
const code = (c: string) => (e: unknown) => e instanceof StoreError && e.code === c;

test("migrate brings an empty DB to v1, and reopening is a no-op", () => {
  const file = path.join(tempDir(), "p.sqlite");
  const store = openPolicyStore(file);
  assert.equal(store.schemaVersion, MIGRATIONS.length);
  store.close();
  const again = openPolicyStore(file);
  assert.equal(again.schemaVersion, MIGRATIONS.length);
  again.close();
});

test("a DB newer than this build opens with a warning", async () => {
  const file = path.join(tempDir(), "p.sqlite");
  const db = new DatabaseSync(file);
  db.exec("PRAGMA user_version = 99");
  db.close();
  const { result, logs } = await quietly(() => openPolicyStore(file));
  assert.equal(result.schemaVersion, 99);
  assert.ok(logs.some((l) => /newer than this build/.test(l)));
  result.close();
});

test("a failing migration rolls back and leaves user_version untouched", () => {
  const db = new DatabaseSync(path.join(tempDir(), "p.sqlite"));
  assert.throws(() => new PolicyStore(db, [MIGRATIONS[0]!, "CREATE TABLE broken ("]));
  assert.equal(Number((db.prepare("PRAGMA user_version").get() as { user_version: number }).user_version), 1);
});

test("ensureDefaultAgent seeds once and never re-seeds", () => {
  const store = tempStore();
  assert.equal(store.ensureDefaultAgent(DEFAULT_GRANTS), true);
  assert.equal(store.getAgent("default")?.scoped, false);
  store.setGrants("default", [{ area: "docs", op: "read" }]);
  assert.equal(store.ensureDefaultAgent(DEFAULT_GRANTS), false);
  assert.deepEqual(store.getGrants("default"), [{ area: "docs", op: "read" }]);
});

test("new agents are confined", () => {
  const store = tempStore();
  store.createAgent("ops-agent", "Ops");
  assert.equal(store.getAgent("ops-agent")?.scoped, true);
});

test("slug rules", () => {
  const store = tempStore();
  store.createAgent("ok-1", "fine");
  assert.throws(() => store.createAgent("-x", "n"), code("invalid_slug"));
  assert.throws(() => store.createAgent("A", "n"), code("invalid_slug"));
  assert.throws(() => store.createAgent("a".repeat(33), "n"), code("invalid_slug"));
  assert.throws(() => store.createAgent("ok-2", "  "), code("invalid_name"));
  assert.throws(() => store.createAgent("ok-1", "again"), code("exists"));
});

test("an archived slug stays reserved", () => {
  const store = tempStore();
  store.createAgent("old", "Old");
  store.archiveAgent("old");
  assert.throws(() => store.createAgent("old", "New"), code("exists"));
});

test("default cannot be archived or scoped", () => {
  const store = tempStore();
  store.ensureDefaultAgent(DEFAULT_GRANTS);
  assert.throws(() => store.archiveAgent("default"), code("reserved"));
  assert.throws(() => store.addScope("default", "f".repeat(20), "F", true), code("reserved"));
});

test("archiving revokes the agent's tokens", () => {
  const store = tempStore();
  store.createAgent("a1", "A");
  const t = mintToken();
  store.insertToken("a1", t.hash, t.prefix, "");
  store.archiveAgent("a1");
  assert.equal(store.agentForTokenHash(t.hash), null);
  assert.notEqual(store.listTokens("a1")[0]!.revokedAt, null);
});

test("the first scope is primary, and there is only ever one primary", () => {
  const store = tempStore();
  store.createAgent("a1", "A");
  store.addScope("a1", "folder-one-xxxx", "One", false);
  store.addScope("a1", "folder-two-xxxx", "Two", true);
  store.addScope("a1", "folder-three-xx", "Three", false);
  const primaries = store.getScopes("a1").filter((s) => s.isPrimary);
  assert.deepEqual(primaries.map((s) => s.folderId), ["folder-two-xxxx"]);
  store.setPrimary("a1", "folder-three-xx");
  assert.deepEqual(
    store.getScopes("a1").filter((s) => s.isPrimary).map((s) => s.folderId),
    ["folder-three-xx"],
  );
  assert.throws(() => store.addScope("a1", "folder-two-xxxx", "Dup", false), code("exists"));
  assert.throws(() => store.setPrimary("a1", "missing-folder"), code("not_found"));
});

test("removing the last scope keeps the agent confined; unconfine needs zero scopes", () => {
  const store = tempStore();
  store.createAgent("a1", "A");
  store.addScope("a1", "folder-one-xxxx", "One", true);
  assert.throws(() => store.setUnconfined("a1"), code("has_scopes"));
  store.removeScope("a1", "folder-one-xxxx");
  assert.equal(store.getAgent("a1")?.scoped, true);
  store.setUnconfined("a1");
  assert.equal(store.getAgent("a1")?.scoped, false);
});

test("DEFAULT_GRANTS derive exactly today's 20 tools", () => {
  assert.deepEqual([...deriveTools(DEFAULT_GRANTS)].sort(), [...TODAYS_TOOLS].sort());
  // The image build stage has no compose file; the source checkout does.
  if (!existsSync("docker-compose.yml")) return;
  const fromCompose = readFileSync("docker-compose.yml", "utf8").match(/TOOL_ALLOWLIST:-([^}]*)\}/)![1]!;
  assert.deepEqual([...deriveTools(DEFAULT_GRANTS)].sort(), fromCompose.split(",").sort());
});

test("compose publishes MCP and admin on host loopback only", () => {
  // The image build stage has no compose file; the source checkout does.
  if (!existsSync("docker-compose.yml")) return;
  const compose = readFileSync("docker-compose.yml", "utf8");
  // No YAML dependency: slice the service block, then its ports list.
  const start = compose.search(/^  gworkspace-mcp:\n/m);
  assert.ok(start >= 0, "no gworkspace-mcp service block");
  const rest = compose.slice(start + 1);
  const next = rest.search(/^  \S/m);
  const service = next < 0 ? rest : rest.slice(0, next);
  const portsAt = service.search(/^    ports:\n/m);
  assert.ok(portsAt >= 0, "no ports: in the service block");
  const afterPorts = service.slice(portsAt).split("\n").slice(1);
  const end = afterPorts.findIndex((line) => line.trim() !== "" && !/^ {5,}/.test(line));
  const block = (end < 0 ? afterPorts : afterPorts.slice(0, end)).join("\n");
  const entries = [...block.matchAll(/^\s*- '([^']+)'/gm)].map((m) => m[1]!);
  // 3014 is the nginx vhost gworkspace-mcp.vps's upstream; 3017 the admin tunnel.
  assert.deepEqual(entries.sort(), ["127.0.0.1:3014:3014", "127.0.0.1:3017:3015"]);
  assert.ok(entries.every((e) => e.startsWith("127.0.0.1:")));
});

test("calendar-only grants derive 4 tools", () => {
  const tools = deriveTools([
    { area: "calendar", op: "read" },
    { area: "calendar", op: "write" },
  ]);
  assert.equal(tools.size, 4);
});

test("enabledTools keeps the exposed order", () => {
  const got = enabledTools(DEFAULT_GRANTS, false, TODAYS_TOOLS, none);
  assert.deepEqual(got, TODAYS_TOOLS);
});

test("a confined agent never sees the withheld list tools, even with no scopes", () => {
  const got = enabledTools(DEFAULT_GRANTS, true, TODAYS_TOOLS, none);
  for (const name of SCOPED_WITHHELD) assert.ok(!got.includes(name));
  assert.equal(got.length, 17);
});

test("tripwire sets are honoured per population", () => {
  const withheld = { scoped: new Set(["readDocument"]), all: new Set(["quickAddEvent"]) };
  const unscoped = enabledTools(DEFAULT_GRANTS, false, TODAYS_TOOLS, withheld);
  assert.ok(unscoped.includes("readDocument"));
  assert.ok(!unscoped.includes("quickAddEvent"));
  const scoped = enabledTools(DEFAULT_GRANTS, true, TODAYS_TOOLS, withheld);
  assert.ok(!scoped.includes("readDocument"));
});

test("TOOL_MAP has no gmail and no sharing tool", () => {
  const all = Object.values(TOOL_MAP).flat();
  for (const banned of ["setFilePermission", "appendTableRows", "sendEmail", "deleteFile"]) {
    assert.ok(!all.includes(banned), banned);
  }
});

test("mintToken: prefix, entropy, hash roundtrip", () => {
  const t = mintToken();
  assert.match(t.plaintext, /^gws_[A-Za-z0-9_-]{43}$/);
  assert.equal(t.prefix, t.plaintext.slice(0, 12));
  assert.equal(t.hash, hashToken(t.plaintext));
  assert.notEqual(mintToken().plaintext, t.plaintext);
});

test("authenticator: legacy bearer, rotation, minted, revoked, archived", () => {
  const store = tempStore();
  store.ensureDefaultAgent(DEFAULT_GRANTS);
  let legacy = "old-legacy";
  const auth = makeAuthenticator(store, () => legacy);

  assert.equal(auth("old-legacy")?.slug, "default");
  legacy = "new-legacy";
  assert.equal(auth("old-legacy"), null);
  assert.equal(auth("new-legacy")?.slug, "default");

  store.createAgent("a1", "A");
  const t = mintToken();
  const id = store.insertToken("a1", t.hash, t.prefix, "");
  const ctx = auth(t.plaintext);
  assert.equal(ctx?.slug, "a1");
  assert.equal(ctx?.scoped, true);

  store.revokeToken(id);
  assert.equal(auth(t.plaintext), null);

  const t2 = mintToken();
  store.insertToken("a1", t2.hash, t2.prefix, "");
  store.archiveAgent("a1");
  assert.equal(auth(t2.plaintext), null);
});

test("an empty legacy token never matches", () => {
  const store = tempStore();
  store.ensureDefaultAgent(DEFAULT_GRANTS);
  const auth = makeAuthenticator(store, () => "");
  assert.equal(auth(""), null);
  assert.equal(makeDegradedAuthenticator(() => "")(""), null);
});

test("degraded authenticator: legacy gets the seed, anything else is refused", () => {
  const auth = makeDegradedAuthenticator(() => "legacy");
  const ctx = auth("legacy");
  assert.deepEqual(ctx?.grants, DEFAULT_GRANTS);
  assert.equal(ctx?.scoped, false);
  assert.equal(auth(mintToken().plaintext), null);
});

test("openPolicyStore: a missing directory is 'did not exist'", () => {
  assert.throws(
    () => openPolicyStore(path.join(tempDir(), "missing", "p.sqlite")),
    (e: unknown) => e instanceof OpenError && e.existed === false,
  );
});

test("openPolicyStore: a corrupt existing file is 'existed'", () => {
  const file = path.join(tempDir(), "p.sqlite");
  writeFileSync(file, Buffer.alloc(4096, 7));
  assert.throws(
    () => openPolicyStore(file),
    (e: unknown) => e instanceof OpenError && e.existed === true,
  );
});

test("openPolicyStore: an unreadable directory counts as existing", { skip: process.getuid?.() === 0 }, () => {
  const dir = tempDir();
  mkdirSync(path.join(dir, "locked"));
  const file = path.join(dir, "locked", "p.sqlite");
  writeFileSync(file, "");
  chmodSync(path.join(dir, "locked"), 0o000);
  try {
    assert.throws(
      () => openPolicyStore(file),
      (e: unknown) => e instanceof OpenError && e.existed === true,
    );
  } finally {
    chmodSync(path.join(dir, "locked"), 0o700);
  }
});

test("openPolicyStore: a 0-byte file is opened and migrated", () => {
  const file = path.join(tempDir(), "p.sqlite");
  writeFileSync(file, "");
  const store = openPolicyStore(file);
  assert.equal(store.schemaVersion, MIGRATIONS.length);
  store.close();
});

test("audit rows round-trip, capped at 50 resources, pruned by age", () => {
  const store = tempStore();
  const resources = Array.from({ length: 60 }, (_, i) => `id-${i}`);
  store.insertAudit({ ts: 1, kind: "tool", agent: "a", tool: "t", decision: "deny", reason: "r", resources, source: "s" });
  store.insertAudit({ ts: 5, kind: "auth", agent: null, tool: null, decision: "deny", reason: "r", resources: [], source: "s" });
  const rows = store.queryAudit({ limit: 10 });
  assert.equal(rows.length, 2);
  assert.equal(rows[0]!.ts, 5);
  assert.equal(rows[1]!.resources.length, 50);
  assert.equal(store.queryAudit({ agent: "a", limit: 10 }).length, 1);
  assert.equal(store.pruneAudit(3), 1);
  assert.equal(store.queryAudit({ limit: 10 }).length, 1);
});

test("backupDaily writes one file per weekday and overwrites a week later", async () => {
  const store = tempStore();
  store.ensureDefaultAgent(DEFAULT_GRANTS);
  const dir = path.join(tempDir(), "backups");
  const monday = new Date(Date.UTC(2026, 9, 5));
  const first = (await backupDaily(store, dir, monday))!;
  assert.equal(path.basename(first), "policy-1.sqlite");
  // The skip-if-written-today check reads the real mtime; pin it to "monday".
  utimesSync(first, monday, monday);
  store.createAgent("later", "Later");
  const again = (await backupDaily(store, dir, new Date(Date.UTC(2026, 9, 12))))!;
  assert.equal(again, first);
  assert.ok(existsSync(again));
  const restored = openPolicyStore(again);
  assert.ok(restored.getAgent("later"));
  restored.close();
});

test("backupDaily keeps a copy already written today", async () => {
  const store = tempStore();
  const dir = path.join(tempDir(), "backups");
  const today = new Date();
  assert.ok(await backupDaily(store, dir, today));
  assert.equal(await backupDaily(store, dir, today), null);
  assert.ok(!existsSync(`${path.join(dir, `policy-${today.getUTCDay()}.sqlite`)}.tmp`));
});

test("removing the primary promotes the oldest remaining folder", () => {
  let now = 0;
  const store = new PolicyStore(new DatabaseSync(":memory:"), MIGRATIONS, () => ++now);
  store.createAgent("a1", "A");
  store.addScope("a1", "folder-one-xxxx", "One", false);
  store.addScope("a1", "folder-two-xxxx", "Two", false);
  store.addScope("a1", "folder-three-xx", "Three", true);
  store.removeScope("a1", "folder-three-xx");
  assert.deepEqual(store.getScopes("a1").filter((s) => s.isPrimary).map((s) => s.folderId), ["folder-one-xxxx"]);
  assert.throws(() => store.removeScope("a1", "missing-folder"), code("not_found"));
});

test("a failed backup leaves the previous copy and no temp file", async () => {
  const store = tempStore();
  const dir = path.join(tempDir(), "backups");
  const old = new Date(Date.UTC(2026, 0, 1));
  const target = (await backupDaily(store, dir, old))!;
  utimesSync(target, old, old);
  const before = readFileSync(target);
  store.close();
  await assert.rejects(backupDaily(store, dir, new Date(Date.UTC(2026, 0, 8))));
  assert.deepEqual(readFileSync(target), before);
  assert.equal(statSync(target).mtime.getTime(), old.getTime());
  assert.ok(!existsSync(`${target}.tmp`));
});
