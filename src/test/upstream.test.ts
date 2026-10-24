import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { createRequire } from "node:module";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

import { existsSync, readFileSync } from "node:fs";

import { GoogleHealth, type BrokenKind } from "../drive/health.js";
import {
  EXPECTED_KEYS,
  applyProbe,
  childEnv,
  normaliseToken,
  refreshTokenOf,
  selectTools,
  tripwire,
  upstreamState,
  watchClose,
  type UpstreamTool,
} from "../upstream.js";
import { quietly } from "./helpers.js";

const tool = (name: string, keys: string[] = []): UpstreamTool => ({
  name,
  inputSchema: {
    type: "object",
    properties: Object.fromEntries(keys.map((k) => [k, { type: "string" }])),
  },
});

test("selectTools keeps the upstream's order, not the allowlist's", () => {
  const got = selectTools([tool("a"), tool("b"), tool("c")], ["c", "a"]);
  assert.deepEqual(got.map((t) => t.name), ["a", "c"]);
});

test("selectTools keeps the first of a duplicate name", () => {
  const first = tool("a", ["x"]);
  const got = selectTools([first, tool("a", ["y"])], ["a"]);
  assert.equal(got.length, 1);
  assert.equal(got[0], first);
});

test("selectTools warns about an allowlisted name the upstream lacks", async () => {
  const { result, logs } = await quietly(() => selectTools([tool("a")], ["a", "typo"]));
  assert.equal(result.length, 1);
  assert.ok(logs.some((l) => /not found upstream, ignored: typo/.test(l)));
});

test("selectTools exposes nothing on an empty allowlist", () => {
  assert.deepEqual(selectTools([tool("a")], []), []);
});

test("normaliseToken wraps a bare refresh token", () => {
  const doc = normaliseToken("1//abc-DEF_1.2");
  assert.equal(refreshTokenOf(doc), "1//abc-DEF_1.2");
});

test("normaliseToken describes bad JSON by shape only", () => {
  assert.throws(() => normaliseToken('{"refresh_token": "1//secret"'), (error: Error) => {
    assert.match(error.message, /not valid JSON/);
    assert.doesNotMatch(error.message, /secret/);
    return true;
  });
});

test("normaliseToken refuses a document with no refresh_token", () => {
  assert.throws(() => normaliseToken('{"client_id":"x"}'), /no refresh_token.*client_id/);
});

test("tripwire passes the measured argument sets", () => {
  const tools = Object.entries(EXPECTED_KEYS).map(([name, e]) => tool(name, [...e.keys]));
  const withheld = tripwire(tools);
  assert.equal(withheld.scoped.size, 0);
  assert.equal(withheld.all.size, 0);
});

test("tripwire withholds a tool that lost an argument", () => {
  const withheld = tripwire([tool("createDocument", ["title", "initialContent", "contentFormat"])]);
  assert.deepEqual([...withheld.scoped], ["createDocument"]);
  assert.equal(withheld.all.size, 0);
});

test("tripwire withholds a tool that gained an argument", () => {
  const keys = [...EXPECTED_KEYS.listDriveFiles!.keys, "q"];
  assert.deepEqual([...tripwire([tool("listDriveFiles", keys)]).scoped], ["listDriveFiles"]);
});

test("tripwire routes calendar guards to every agent", () => {
  const withheld = tripwire([tool("quickAddEvent", ["calendarId", "text"])]);
  assert.deepEqual([...withheld.all], ["quickAddEvent"]);
  assert.equal(withheld.scoped.size, 0);
});

test("tripwire ignores tools it does not police", () => {
  const withheld = tripwire([tool("listEvents", ["anything"])]);
  assert.equal(withheld.scoped.size + withheld.all.size, 0);
});

// The installed upstream must match what the policy engine was written against.
// Booting it needs no network: authorize() only reads token.json.
test("EXPECTED_KEYS match the installed upstream", { timeout: 60_000 }, async () => {
  const home = mkdtempSync(path.join(tmpdir(), "gws-upstream-"));
  mkdirSync(path.join(home, "google-docs-mcp"), { recursive: true });
  // Without a token file the upstream falls into the interactive consent flow
  // and never answers initialize.
  writeFileSync(
    path.join(home, "google-docs-mcp", "token.json"),
    JSON.stringify({ refresh_token: "1//fake" }),
  );
  const entry = createRequire(import.meta.url).resolve(
    "@a-bonus/google-docs-mcp/dist/index.js",
  );
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [entry],
    env: {
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      GOOGLE_CLIENT_ID: "x",
      GOOGLE_CLIENT_SECRET: "y",
      XDG_CONFIG_HOME: home,
      HOME: home,
    },
    stderr: "ignore",
  });
  const client = new Client({ name: "tripwire-test", version: "0" }, { capabilities: {} });
  await client.connect(transport);
  try {
    const { tools } = await client.listTools();
    const policed = (tools as UpstreamTool[]).filter((t) => t.name in EXPECTED_KEYS);
    assert.equal(policed.length, Object.keys(EXPECTED_KEYS).length);
    const withheld = tripwire(policed);
    assert.deepEqual([...withheld.scoped, ...withheld.all], []);
  } finally {
    await client.close();
  }
});

// --- credential probe, child exit, child env -------------------------------------

const probeRig = () => {
  const sent: BrokenKind[] = [];
  const google = new GoogleHealth();
  google.setBroadcast((kind) => sent.push(kind));
  return { google, sent };
};
const failed = (text: string) => ({ isError: true, content: [{ type: "text", text }] });

test("childEnv has exactly the explicit keys and nothing from the parent's secrets", () => {
  const saved = { token: process.env.SECRETS_MANAGER_TOKEN, base: process.env.NTFY_BASE };
  process.env.SECRETS_MANAGER_TOKEN = "vault-secret";
  process.env.NTFY_BASE = "https://ntfy.example";
  try {
    const env = childEnv({ clientId: "cid", clientSecret: "csecret" });
    assert.deepEqual(Object.keys(env).sort(), [
      "GOOGLE_CLIENT_ID",
      "GOOGLE_CLIENT_SECRET",
      "HOME",
      "NODE_ENV",
      "NODE_OPTIONS",
      "PATH",
      "XDG_CONFIG_HOME",
    ]);
    assert.ok(!Object.values(env).some((v) => v.includes("vault-secret") || v.includes("ntfy.example")));
  } finally {
    if (saved.token === undefined) delete process.env.SECRETS_MANAGER_TOKEN;
    else process.env.SECRETS_MANAGER_TOKEN = saved.token;
    if (saved.base === undefined) delete process.env.NTFY_BASE;
    else process.env.NTFY_BASE = saved.base;
  }
});

test("applyProbe: a missing probe logs skipped, never verified, and seeds nothing", async () => {
  const { google, sent } = probeRig();
  for (const missing of [undefined, null]) {
    const { logs } = await quietly(() => applyProbe(missing, google));
    assert.deepEqual(logs, ["[upstream] CREDENTIAL CHECK skipped: the Workspace server is not connected"]);
  }
  assert.deepEqual(google.snapshot(), { state: "unknown", since: null, consecutiveFailures: 0, probe: "unknown" });
  assert.deepEqual(sent, []);
});

test("applyProbe: a good probe is verified and pushes nothing", async () => {
  const { google, sent } = probeRig();
  const lines: string[] = [];
  const { log } = console;
  console.log = (...args: unknown[]) => lines.push(args.map(String).join(" "));
  try {
    applyProbe({ content: [{ type: "text", text: "[]" }] }, google);
  } finally {
    console.log = log;
  }
  assert.deepEqual(lines, ["[upstream] credential verified"]);
  assert.equal(google.snapshot().probe, "ok");
  assert.deepEqual(sent, []);
});

test("applyProbe: invalid_grant past character 300 still rejects", async () => {
  const { google, sent } = probeRig();
  const { logs } = await quietly(() => applyProbe(failed(`${"x".repeat(400)} invalid_grant`), google));
  assert.ok(logs[0]!.startsWith("[upstream] CREDENTIAL CHECK FAILED"));
  assert.ok(logs[0]!.length < 450);
  assert.equal(google.snapshot().state, "rejected");
  assert.deepEqual(sent, ["rejected"]);
});

test("applyProbe: any other probe failure marks the probe failed and leaves the state alone", async () => {
  const { google, sent } = probeRig();
  await quietly(() => applyProbe(failed("HTTP 403 accessNotConfigured"), google));
  assert.equal(google.snapshot().probe, "failed");
  assert.equal(google.snapshot().state, "unknown");
  assert.deepEqual(sent, ["probe"]);
});

test("watchClose: an exit clears the shared client and calls onExit exactly once", async () => {
  const before = upstreamState.client;
  upstreamState.client = {} as never;
  let exits = 0;
  const client: { onclose?: () => void } = {};
  try {
    watchClose(client, () => exits++);
    await quietly(() => client.onclose!());
    assert.equal(upstreamState.client, null);
    assert.equal(exits, 1);
  } finally {
    upstreamState.client = before;
  }
});

test("watchClose works without an onExit", async () => {
  const before = upstreamState.client;
  const client: { onclose?: () => void } = {};
  try {
    watchClose(client);
    await quietly(() => client.onclose!());
  } finally {
    upstreamState.client = before;
  }
});

test("compose carries NTFY_BASE with the public default", () => {
  if (!existsSync("docker-compose.yml")) return;
  assert.match(readFileSync("docker-compose.yml", "utf8"), /NTFY_BASE: '\$\{NTFY_BASE:-https:\/\/ntfy\.sh\}'/);
});
