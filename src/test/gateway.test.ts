import { test, after } from "node:test";
import assert from "node:assert/strict";
import type { Server } from "node:http";

import { Audit } from "../audit.js";
import { ScopeResolver } from "../enforce/resolver.js";
import { SubtreeCache } from "../enforce/search.js";
import { createMcpApp, type GatewayDeps } from "../gateway.js";
import {
  runHousekeeping,
  type Housekeeping,
  type HousekeepingDeps,
  type HousekeepingResult,
} from "../housekeeping.js";
import { makeAuthenticator } from "../policy/auth.js";
import { DEFAULT_GRANTS } from "../policy/derive.js";
import { mintToken } from "../policy/tokens.js";
import {
  FakeDrive,
  retryable,
  FakeUpstream,
  TODAYS_TOOLS,
  callTool,
  fakeUpstreamState,
  listTools,
  listen,
  mcpCall,
  quietly,
  tempStore,
} from "./helpers.js";

const servers: Server[] = [];
after(() => servers.forEach((s) => s.close()));

async function boot(
  opts: {
    ready?: boolean;
    withUpstream?: boolean;
    withheld?: { scoped: Set<string>; all: Set<string> };
    housekeeping?: Housekeeping | null;
    plrToken?: string;
  } = {},
) {
  const store = tempStore();
  store.ensureDefaultAgent(DEFAULT_GRANTS);
  const audit = new Audit(store);
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", ["root-folder1"]);
  drive.add("doc-in-F-001", ["scope-F-0001"], { name: "inside doc" });
  drive.folder("other-folder", ["root-folder1"]);
  drive.add("doc-outside1", ["other-folder"], { name: "outside doc" });
  const upstreamClient = new FakeUpstream();
  const upstream = fakeUpstreamState(opts.withUpstream === false ? null : upstreamClient);
  if (opts.withheld) upstream.withheld = opts.withheld;
  const subtree = new SubtreeCache(() => drive);
  let ready = opts.ready ?? true;
  const deps: GatewayDeps = {
    authenticate: () => makeAuthenticator(store, () => "legacy-token"),
    audit: () => audit,
    pipeline: { resolver: new ScopeResolver(() => drive), subtree, drive: () => drive },
    upstream,
    ready: () => ready,
    health: () => ({ db: true, auditErrors: audit.errors }),
    housekeeping: () => opts.housekeeping ?? null,
    plrToken: () => opts.plrToken ?? "",
  };
  const { url, server } = await listen(createMcpApp(deps));
  servers.push(server);
  const agent = (slug: string, grants = DEFAULT_GRANTS, folder?: string) => {
    store.createAgent(slug, slug);
    store.setGrants(slug, grants);
    if (folder) store.addScope(slug, folder, "F", true);
    const t = mintToken();
    const id = store.insertToken(slug, t.hash, t.prefix, "");
    return { token: t.plaintext, id };
  };
  return { url, store, audit, drive, upstreamClient, upstream, deps, agent, setReady: (v: boolean) => (ready = v) };
}

const toolNames = (reply: { body: any }) => reply.body.result.tools.map((t: { name: string }) => t.name);

test("POST /mcp before boot finishes is 503 starting", async () => {
  const { url } = await boot({ ready: false });
  const reply = await listTools(url, "legacy-token");
  assert.equal(reply.status, 503);
  assert.deepEqual(reply.body, { error: "starting" });
});

test("the legacy bearer sees today's 20 tools, in today's order", async () => {
  const { url } = await boot();
  assert.deepEqual(toolNames(await listTools(url, "legacy-token")), TODAYS_TOOLS);
});

test("no bearer is 401, audited once per minute per source", async () => {
  const { url, store } = await boot();
  await quietly(async () => {
    assert.equal((await listTools(url, "")).status, 401);
    assert.equal((await listTools(url, "nope")).status, 401);
  });
  const rows = store.queryAudit({ limit: 10 });
  assert.equal(rows.length, 1);
  assert.equal(rows[0]!.kind, "auth");
});

test("a batch request is 400", async () => {
  const { url } = await boot();
  const reply = await mcpCall(url, "legacy-token", [{ jsonrpc: "2.0", id: 1, method: "tools/list" }]);
  assert.equal(reply.status, 400);
});

test("a calendar-only agent sees 4 tools; a disabled call is refused and audited", async () => {
  const { url, agent, store, upstreamClient } = await boot();
  const { token } = agent("cal-agent", [
    { area: "calendar", op: "read" },
    { area: "calendar", op: "write" },
  ]);
  assert.deepEqual(toolNames(await listTools(url, token)), [
    "listEvents", "createEvent", "updateEvent", "quickAddEvent",
  ]);
  const reply = await callTool(url, token, "readDocument", { documentId: "doc-in-F-001" });
  assert.ok(reply.body.error || reply.body.result?.isError);
  assert.equal(upstreamClient.calls.length, 0);
  const row = store.queryAudit({ agent: "cal-agent", limit: 5 })[0]!;
  assert.equal(row.decision, "deny");
  assert.equal(row.reason, "not_enabled");
});

test("a token revoked between requests is 401 on the next one", async () => {
  const { url, agent, store } = await boot();
  const { token, id } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  assert.equal((await listTools(url, token)).status, 200);
  store.revokeToken(id);
  await quietly(async () => assert.equal((await listTools(url, token)).status, 401));
});

test("a grant removed between list and call is refused", async () => {
  const { url, agent, store, upstreamClient } = await boot();
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  assert.ok(toolNames(await listTools(url, token)).includes("readDocument"));
  store.setGrants("a1", [{ area: "calendar", op: "read" }]);
  const reply = await callTool(url, token, "readDocument", { documentId: "doc-in-F-001" });
  assert.ok(reply.body.error || reply.body.result?.isError);
  assert.equal(upstreamClient.calls.length, 0);
});

test("a confined agent's read outside its folder never reaches upstream", async () => {
  const { url, agent, store, upstreamClient } = await boot();
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  const reply = await callTool(url, token, "readDocument", { documentId: "doc-outside1" });
  assert.equal(reply.body.result.isError, true);
  assert.match(reply.body.result.content[0].text, /outside this agent's folder scope/);
  assert.equal(upstreamClient.calls.length, 0);
  const row = store.queryAudit({ agent: "a1", limit: 1 })[0]!;
  assert.deepEqual([row.decision, row.reason, row.resources], ["deny", "outside", ["doc-outside1"]]);
});

test("a confined create is pinned to the primary and audited as mutate", async () => {
  const { url, agent, store, upstreamClient } = await boot();
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  const reply = await callTool(url, token, "createDocument", { title: "new" });
  assert.equal(reply.body.result.isError, undefined);
  assert.deepEqual(upstreamClient.calls[0], {
    name: "createDocument",
    arguments: { title: "new", parentFolderId: "scope-F-0001" },
  });
  assert.equal(store.queryAudit({ agent: "a1", limit: 1 })[0]!.decision, "mutate");
});

test("an upstream isError is passed through and audited as error; after() does not run", async () => {
  const { url, agent, store, upstreamClient, drive } = await boot();
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  upstreamClient.failing.add("createFolder");
  await quietly(async () => {
    // Warm the subtree, then a failing createFolder must not flush it.
    await callTool(url, token, "searchDriveFiles", { query: "doc" });
    const calls = drive.listCalls.length;
    const reply = await callTool(url, token, "createFolder", { name: "x" });
    assert.equal(reply.body.result.isError, true);
    assert.equal(reply.body.result.content[0].text, "upstream said no");
    await callTool(url, token, "searchDriveFiles", { query: "doc" });
    // Only the search chunk itself; the subtree came from cache.
    assert.equal(drive.listCalls.length, calls + 1);
  });
  assert.equal(store.queryAudit({ agent: "a1", decision: "error", limit: 1 })[0]!.tool, "createFolder");
});

test("searchDriveFiles with only {query}, through the real SDK path, applies defaults", async () => {
  const { url, agent, drive } = await boot();
  for (let i = 0; i < 15; i++) {
    const day = String(i + 1).padStart(2, "0");
    drive.add(`many-doc-${day}xx`, ["scope-F-0001"], { name: `doc ${day}`, modifiedTime: `2026-04-${day}T00:00:00.000Z` });
  }
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  const reply = await callTool(url, token, "searchDriveFiles", { query: "doc" });
  const out = JSON.parse(reply.body.result.content[0].text);
  assert.equal(out.files.length, 10);
  assert.equal(out.hasMore, true);
  assert.ok(!out.files.some((f: { id: string }) => f.id === "doc-outside1"));
  const times = out.files.map((f: { modifiedTime: string }) => f.modifiedTime);
  assert.deepEqual(times, [...times].sort().reverse());
});

test("a folder created by a confined agent is searchable at once", async () => {
  const { url, agent, drive, upstreamClient } = await boot();
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  await callTool(url, token, "searchDriveFiles", { query: "fresh" });
  // What the upstream would have done on Google's side:
  upstreamClient.callTool = async (p) => {
    drive.folder("new-folder-01", ["scope-F-0001"]);
    drive.add("fresh-doc-001", ["new-folder-01"], { name: "fresh doc" });
    return { content: [{ type: "text", text: `ok:${p.name}` }] };
  };
  await callTool(url, token, "createFolder", { name: "new" });
  const out = JSON.parse((await callTool(url, token, "searchDriveFiles", { query: "fresh" })).body.result.content[0].text);
  assert.deepEqual(out.files.map((f: { id: string }) => f.id), ["fresh-doc-001"]);
});

test("a confined agent never sees the withheld list tools", async () => {
  const { url, agent } = await boot();
  const { token } = agent("a1", DEFAULT_GRANTS);
  const names = toolNames(await listTools(url, token));
  assert.ok(!names.includes("listDocuments"));
  assert.equal(names.length, 17);
});

test("healthz: 503 without upstream, 200 with, and reports the db", async () => {
  const down = await boot({ withUpstream: false });
  const res = await fetch(`${down.url}/healthz`);
  assert.equal(res.status, 503);
  const up = await boot();
  const ok = await fetch(`${up.url}/healthz`);
  assert.equal(ok.status, 200);
  const body = (await ok.json()) as { db: boolean; tools: number };
  assert.equal(body.db, true);
  assert.equal(body.tools, 20);
});

test("tripwire-withheld tools disappear from tools/list and never reach upstream", async () => {
  const { url, agent, upstreamClient } = await boot({
    withheld: { scoped: new Set(["readDocument"]), all: new Set(["quickAddEvent"]) },
  });
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  const confinedTools = toolNames(await listTools(url, token));
  assert.ok(!confinedTools.includes("readDocument"));
  assert.ok(!confinedTools.includes("quickAddEvent"));
  const legacyTools = toolNames(await listTools(url, "legacy-token"));
  assert.ok(legacyTools.includes("readDocument"));
  assert.ok(!legacyTools.includes("quickAddEvent"));
  const reply = await callTool(url, token, "readDocument", { documentId: "doc-in-F-001" });
  assert.ok(reply.body.error || reply.body.result?.isError);
  assert.equal(upstreamClient.calls.length, 0);
});

test("a Drive outage during a served search is 'try again', audited as drive_error", async () => {
  const { url, agent, drive, store } = await boot();
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  drive.listError = retryable();
  const reply = await callTool(url, token, "searchDriveFiles", { query: "doc" });
  assert.equal(reply.body.result.isError, true);
  assert.match(reply.body.result.content[0].text, /try again/);
  assert.doesNotMatch(reply.body.result.content[0].text, /503/);
  assert.equal(store.queryAudit({ agent: "a1", limit: 1 })[0]!.reason, "drive_error");
});

test("an oversized scope is denied as too_large", async () => {
  const { url, agent, drive, store } = await boot();
  for (let i = 0; i < 501; i++) drive.folder(`big-${String(i).padStart(4, "0")}-xxxx`, ["scope-F-0001"]);
  const { token } = agent("a1", DEFAULT_GRANTS, "scope-F-0001");
  const reply = await callTool(url, token, "searchDriveFiles", { query: "doc" });
  assert.match(reply.body.result.content[0].text, /over 500 folders/);
  assert.equal(store.queryAudit({ agent: "a1", limit: 1 })[0]!.reason, "too_large");
});

test("a call while the child is down is still audited", async () => {
  const { url, store } = await boot({ withUpstream: false });
  await callTool(url, "legacy-token", "readDocument", { documentId: "doc-in-F-001" });
  assert.equal(store.queryAudit({ agent: "default", limit: 1 })[0]!.reason, "upstream_down");
});

test("a policy DB that cannot answer is 503, not 401 or a stack trace", async () => {
  const { url, deps } = await boot();
  deps.authenticate = () => () => {
    throw new Error("SQLITE_BUSY");
  };
  const { result } = await quietly(() => listTools(url, "legacy-token"));
  assert.equal(result.status, 503);
  assert.deepEqual(result.body, { error: "policy unavailable" });
});

test("the legacy agent's audit names the file it touched", async () => {
  const { url, store } = await boot();
  await callTool(url, "legacy-token", "readDocument", { documentId: "doc-outside1" });
  assert.deepEqual(store.queryAudit({ agent: "default", limit: 1 })[0]!.resources, ["doc-outside1"]);
});

test("a malformed JSON body is a JSON-RPC parse error, not a 500", async () => {
  const { url } = await boot();
  const res = await fetch(`${url}/mcp`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", Authorization: "Bearer legacy-token" },
    body: "{not json",
  });
  assert.equal(res.status, 400);
  const body = (await res.json()) as { error: { code: number } };
  assert.equal(body.error.code, -32700);
});

test("an error past the route is a JSON-RPC internal error without a stack", async () => {
  const { url, deps } = await boot();
  // The audit peek runs inside the route; a throw there is unhandled by it.
  deps.audit = () => {
    throw new Error("boom inside the route");
  };
  const { result } = await quietly(() => callTool(url, "legacy-token", "notARealTool", {}));
  assert.equal(result.status, 500);
  assert.equal(result.body.error.code, -32603);
  assert.doesNotMatch(JSON.stringify(result.body), /boom/);
});

// ---------------------------------------------------------------------------
// POST /internal/housekeeping — pipeline-runner's daily call.

const PLR = "plr-token-0123456789";
const RESULT: HousekeepingResult = { pruned: 2, backup: "/state/backups/policy-3.sqlite" };

function spyHousekeeping(impl: Housekeeping = async () => RESULT) {
  const spy = Object.assign(() => {
    spy.calls++;
    return impl();
  }, { calls: 0 });
  return spy;
}

async function internal(
  url: string,
  opts: { bearer?: string; path?: string; headers?: Record<string, string>; body?: string } = {},
) {
  const res = await fetch(`${url}${opts.path ?? "/internal/housekeeping"}`, {
    method: "POST",
    headers: {
      ...(opts.bearer !== undefined ? { Authorization: `Bearer ${opts.bearer}` } : {}),
      ...opts.headers,
    },
    body: opts.body,
  });
  const text = await res.text();
  let body: any = text;
  try {
    body = JSON.parse(text);
  } catch {
    // Not JSON (a bare 404).
  }
  return { status: res.status, body };
}

test("internal: no bearer is 401 and housekeeping does not run", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  const reply = await quietly(() => internal(url));
  assert.equal(reply.result.status, 401);
  assert.equal(spy.calls, 0);
});

test("internal: a wrong bearer is 401", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  const reply = await quietly(() => internal(url, { bearer: "legacy-token" }));
  assert.equal(reply.result.status, 401);
  assert.equal(spy.calls, 0);
});

test("internal: no configured token is 503, even for an empty bearer", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: "" });
  const reply = await internal(url, { bearer: "" });
  assert.equal(reply.status, 503);
  assert.deepEqual(reply.body, { error: "not_configured" });
  assert.equal(spy.calls, 0);
});

test("internal: a proxied request is 404, whatever the bearer", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  for (const header of ["X-Forwarded-For", "X-Real-IP"]) {
    const reply = await internal(url, { bearer: PLR, headers: { [header]: "203.0.113.9" } });
    assert.equal(reply.status, 404, header);
  }
  assert.equal(spy.calls, 0);
});

test("internal: a case variant of the path is 404", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  for (const path of ["/INTERNAL/housekeeping", "/Internal/housekeeping"]) {
    assert.equal((await internal(url, { bearer: PLR, path })).status, 404, path);
  }
  assert.equal(spy.calls, 0);
});

test("internal: runs while upstream is down (gated on the store, not ready)", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ ready: false, withUpstream: false, housekeeping: spy, plrToken: PLR });
  const reply = await internal(url, { bearer: PLR });
  assert.equal(reply.status, 200);
  assert.equal(spy.calls, 1);
});

test("internal: no store is 503 no_store", async () => {
  const { url } = await boot({ housekeeping: null, plrToken: PLR });
  const reply = await internal(url, { bearer: PLR });
  assert.equal(reply.status, 503);
  assert.deepEqual(reply.body, { error: "no_store" });
});

test("internal: the happy path answers the result", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  const reply = await internal(url, { bearer: PLR });
  assert.equal(reply.status, 200);
  assert.deepEqual(reply.body, RESULT);
  assert.equal(spy.calls, 1);
});

test("internal: a failing housekeeping run is 500", async () => {
  const spy = spyHousekeeping(async () => {
    throw new Error("disk full");
  });
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  const reply = await quietly(() => internal(url, { bearer: PLR }));
  assert.equal(reply.result.status, 500);
  assert.deepEqual(reply.result.body, { error: "disk full" });
});

test("internal: the 11th request in a minute is 429 before the bearer; one log line for 10 failures", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  const { result, logs } = await quietly(async () => {
    const statuses: number[] = [];
    for (let i = 0; i < 10; i++) statuses.push((await internal(url, { bearer: "wrong" })).status);
    // The right bearer, still refused: the limit comes first.
    statuses.push((await internal(url, { bearer: PLR })).status);
    return statuses;
  });
  assert.deepEqual(result, [...Array(10).fill(401), 429]);
  assert.equal(logs.filter((l) => l.includes("[internal] rejected")).length, 1);
  assert.equal(spy.calls, 0);
});

test("internal: a 5 MB body without auth is refused without being read", async () => {
  const spy = spyHousekeeping();
  const { url } = await boot({ housekeeping: spy, plrToken: PLR });
  const started = performance.now();
  const reply = await quietly(() =>
    internal(url, { body: "x".repeat(5 * 1024 * 1024), headers: { "Content-Type": "application/json" } }),
  );
  assert.equal(reply.result.status, 401);
  assert.ok(performance.now() - started < 100, `took ${performance.now() - started} ms`);
});

test("internal: a call during the boot run gets the boot run's result", async () => {
  let deps!: HousekeepingDeps;
  const { url, store, audit } = await boot({ plrToken: PLR, housekeeping: () => runHousekeeping(deps) });
  let backups = 0;
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  deps = {
    audit,
    store,
    backupDir: "/unused",
    backup: async () => {
      backups++;
      await gate;
      return "/state/backups/policy-1.sqlite";
    },
  };

  const bootRun = runHousekeeping(deps);
  const call = internal(url, { bearer: PLR });
  await new Promise((r) => setTimeout(r, 50));
  release();
  const [fromBoot, reply] = await Promise.all([bootRun, call]);
  assert.equal(reply.status, 200);
  assert.deepEqual(reply.body, fromBoot);
  assert.equal(backups, 1);
});
