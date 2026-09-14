/**
 * gworkspace-mcp — puts the Google Workspace MCP server on Streamable HTTP.
 *
 * The upstream server (@a-bonus/google-docs-mcp) speaks stdio. Open WebUI's MCP
 * client speaks Streamable HTTP and nothing else — its utils/mcp/client.py
 * imports streamablehttp_client alone — so the two cannot talk directly. This
 * process runs the stdio server as a child and relays between them.
 *
 * It is a relay, not a reimplementation: whatever tools that server exposes are
 * what appear in chat, so it keeps working when the upstream adds tools.
 *
 * Two SDK majors on purpose: v2 has no stdio client transport, v1 does, and the
 * two speak the same protocol over the wire.
 */

import crypto from "node:crypto";
import { promises as fs } from "node:fs";
import path from "node:path";
import express from "express";
import { McpServer, fromJsonSchema } from "@modelcontextprotocol/server";
import type { CallToolResult, JsonSchemaType } from "@modelcontextprotocol/server";
import { NodeStreamableHTTPServerTransport } from "@modelcontextprotocol/node";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

import { config, vaultFetch } from "./config.js";

type UpstreamTool = {
  name: string;
  description?: string;
  inputSchema: JsonSchemaType;
};

let upstream: Client | null = null;
/** The tools actually registered — deduplicated and allowlisted at boot. */
let exposedTools: UpstreamTool[] = [];
let upstreamToolCount = 0;

/**
 * Reads the child's stderr instead of letting it fill a pipe nobody drains.
 *
 * Two reasons this is not just `pipe(process.stderr)`. The SDK attaches a
 * PassThrough to the child's stderr the moment the transport is constructed, so
 * with no reader the buffers fill — roughly 96 KB of lifetime stderr across the
 * stream and the OS pipe — and the child's writes then queue in its own heap
 * inside a container with a hard memory cap. And the upstream logs whole error
 * objects: a google-auth-library token refresh failure throws a GaxiosError
 * whose config carries the POST body, i.e. the client secret and the refresh
 * token. Piping it raw would write both into the container's json-file log.
 */
function drainStderr(raw: unknown): void {
  // The SDK types this as Stream; it is a PassThrough at runtime.
  const stream = raw as NodeJS.ReadableStream | null;
  if (!stream?.on) return;
  const SECRETISH = /refresh_token|client_secret|access_token|Bearer\s|"token"/i;
  let carry = "";
  let dropped = 0;
  stream.setEncoding("utf8");
  stream.on("data", (chunk: string) => {
    const lines = (carry + chunk).split("\n");
    carry = lines.pop() ?? "";
    for (const line of lines) {
      if (!line.trim()) continue;
      if (SECRETISH.test(line)) {
        dropped++;
        console.warn(
          `[upstream] suppressed a log line containing credential-shaped text (${dropped} so far)`,
        );
        continue;
      }
      console.log(`[upstream] ${line.slice(0, 500)}`);
    }
    // A single unterminated line must not grow without bound either.
    if (carry.length > 4096) carry = "";
  });
  stream.on("error", () => undefined);
}

/**
 * Turns whatever is in the vault into the document the upstream expects.
 *
 * Validated here rather than left to the child, because the child swallows every
 * error from loading this file and falls back to the INTERACTIVE browser consent
 * flow: it prints a URL, waits forever, never answers initialize, and the only
 * symptom is this process timing out after 60s and crash-looping with nothing
 * that says why.
 *
 * A bare refresh token is accepted and wrapped. `1//...` is what you get from
 * copying the token out of token.json rather than copying the file, it is an
 * easy mistake to make through a vault UI, and there is no ambiguity about what
 * was meant.
 */
function normaliseToken(raw: string): string {
  const value = raw.trim();

  if (/^1\/\/[\w.-]+$/.test(value)) {
    console.warn(
      "[auth] GOOGLE_TOKEN_JSON held a bare refresh token, not a token.json document — wrapping it. Store the whole file to silence this.",
    );
    return `${JSON.stringify({ refresh_token: value })}\n`;
  }

  let parsed: { refresh_token?: unknown };
  try {
    parsed = JSON.parse(value) as { refresh_token?: unknown };
  } catch (error) {
    // Describe the SHAPE, never the content: this value is a credential.
    const why = error instanceof Error ? error.message : String(error);
    throw new Error(
      `FATAL: GOOGLE_TOKEN_JSON is not valid JSON (${why}). It is ${value.length} characters over ${value.split("\n").length} line(s), starts with ${JSON.stringify(value.slice(0, 2))} and ends with ${JSON.stringify(value.slice(-2))}. Expected either the whole token.json, or the bare refresh token starting "1//".`,
    );
  }

  if (typeof parsed.refresh_token !== "string" || !parsed.refresh_token) {
    throw new Error(
      `FATAL: GOOGLE_TOKEN_JSON parsed as JSON but has no refresh_token. Its keys are: ${Object.keys(parsed).join(", ") || "(none)"}. If those look like client_id/client_secret you have stored the OAuth CLIENT file; the token comes from completing consent.`,
    );
  }

  return value;
}

/**
 * Start the stdio child once and keep it. Google's OAuth refresh happens inside
 * that process, so restarting it per request would re-do the token dance every
 * time and lose its cache.
 */
async function connectUpstream(credentials: {
  clientId: string;
  clientSecret: string;
  tokenJson: string;
}): Promise<void> {
  // The upstream server reads its refresh token from this path. Writing it at
  // boot from the vault keeps the credential out of the image and out of git.
  const tokenPath = path.join(config.tokenDir, "token.json");

  // Validate before writing. loadSavedCredentialsIfExist swallows every error
  // and returns null, and authorize() then falls into the INTERACTIVE browser
  // OAuth flow: the child prints a consent URL, waits, never answers
  // initialize, and never rejects — so the only symptom is this process timing
  // out after 60s and crash-looping with nothing that says why.
  const tokenJson = normaliseToken(credentials.tokenJson);

  await fs.mkdir(config.tokenDir, { recursive: true, mode: 0o700 });
  await fs.writeFile(tokenPath, tokenJson, { mode: 0o600 });
  // writeFile's mode is ignored when the file already exists.
  await fs.chmod(tokenPath, 0o600);

  // The upstream is a pinned dependency, not `npx -y` at runtime: that would
  // fetch a package over the network on every boot, resolve a different version
  // than was tested, and put a live download in the startup path of a service
  // holding Google credentials.
  const transport = new StdioClientTransport({
    command: "node",
    args: [config.upstreamEntry],
    // Built explicitly, NOT spread from process.env. The child is ~155 files of
    // third-party code that also parses every byte of attacker-controlled
    // document text; a spread hands it SECRETS_MANAGER_TOKEN, which reads the
    // whole Root folder of the vault — VPS_SSH_KEY included. It also hands it
    // MCP_TRANSPORT and SERVICE_ACCOUNT_PATH, either of which silently changes
    // what the child does.
    env: {
      PATH: process.env.PATH ?? "/usr/local/bin:/usr/bin:/bin",
      NODE_ENV: "production",
      // Its own heap budget, not the parent's: both processes inheriting one
      // --max-old-space-size is how two 192 MB heaps end up in a 320 MB cgroup.
      NODE_OPTIONS: `--max-old-space-size=${config.childHeapMb}`,
      GOOGLE_CLIENT_ID: credentials.clientId,
      GOOGLE_CLIENT_SECRET: credentials.clientSecret,
      // The upstream resolves its token store relative to XDG config.
      XDG_CONFIG_HOME: config.configHome,
      HOME: config.configHome,
    },
    stderr: "pipe",
  });

  const client = new Client(
    { name: "gworkspace-bridge", version: "0.1.0" },
    { capabilities: {} },
  );

  // BEFORE connect, not after: the boot handshake is exactly the failure this
  // drain exists to explain, and the SDK builds the PassThrough in the transport
  // constructor, so a listener attached now catches the child's first line.
  drainStderr(transport.stderr);
  await client.connect(transport);

  // A dead child is otherwise invisible: the SDK clears its own transport and
  // never touches this module's reference, so /healthz stayed 200, the Docker
  // healthcheck stayed green, autoheal never fired, and every tool call
  // returned "Failed: Not connected" until someone restarted it by hand.
  client.onclose = () => {
    console.error("[upstream] the Workspace server exited — marking unhealthy");
    upstream = null;
  };

  const listed = await client.listTools();
  upstream = client;
  upstreamToolCount = listed.tools.length;
  exposedTools = selectTools(listed.tools as UpstreamTool[]);

  console.log(
    `[upstream] connected, ${upstreamToolCount} tools available, ${exposedTools.length} exposed`,
  );
}

/**
 * Decides once, at boot, which tools this relay exposes.
 *
 * Done here rather than inside createServer because createServer runs per
 * request: a throw there is a 500 on EVERY POST /mcp — including initialize, so
 * Open WebUI shows "failed to connect" and zero tools — while /healthz stays
 * green. registerTool throws on a duplicate name rather than warning, and the
 * upstream's own source registers `appendTableRows` twice (docs and sheets).
 *
 * Measured: tools/list returns 119 entries, all unique — FastMCP collapses that
 * collision before it reaches the wire, so the duplicate is not reachable today
 * and this guard has never fired. It is here because the cost of being wrong is
 * a total outage that every health signal reports as fine, and because the
 * collapse itself is silently lossy: the sheets implementation wins, so the
 * docs one (documentId, tableId, rows) is simply not reachable through this
 * relay. Keep it out of the allowlist.
 */
function selectTools(tools: UpstreamTool[]): UpstreamTool[] {
  const allowed = config.toolAllowlist;

  // Deny-all on an empty allowlist, deliberately. This account's grant covers
  // full Drive and gmail.modify; the upstream's unfiltered surface includes
  // sendEmail, deleteFile and setFilePermission. "Exposes everything by
  // default" is the wrong failure mode for a service reachable by a model
  // reading documents that other people can write.
  if (!allowed.length) {
    console.warn(
      "[tools] TOOL_ALLOWLIST is empty — exposing NO tools. Set it to a comma-separated list of upstream tool names.",
    );
    return [];
  }

  const byName = new Map<string, UpstreamTool>();
  for (const tool of tools) {
    if (!allowed.includes(tool.name)) continue;
    if (byName.has(tool.name)) {
      console.warn(
        `[tools] upstream registers "${tool.name}" more than once — keeping the first and dropping the rest`,
      );
      continue;
    }
    byName.set(tool.name, tool);
  }

  // A typo in the allowlist otherwise removes a tool with no error, no warning
  // and no health signal.
  const missing = allowed.filter((name) => !byName.has(name));
  if (missing.length) {
    console.warn(
      `[tools] not found upstream, ignored: ${missing.join(", ")}`,
    );
  }

  return [...byName.values()];
}

/**
 * Arguments the relay refuses to pass upstream, by tool.
 *
 * createEvent is an outbound channel: attendees[].email is an arbitrary address
 * and description is arbitrary text, so a document containing injected
 * instructions — one shared with this account by anyone — turns a read tool and
 * a calendar tool into mail-it-to-me. The event still gets created; it just
 * cannot address a stranger. updateEvent carries the same two fields.
 */
const ARGUMENT_GUARDS: Record<string, (args: Record<string, unknown>) => Record<string, unknown>> = {
  createEvent: stripAttendees,
  updateEvent: stripAttendees,
};

function stripAttendees(args: Record<string, unknown>): Record<string, unknown> {
  const { attendees, ...rest } = args;
  if (attendees !== undefined) {
    console.warn("[guard] dropped attendees from a calendar call");
  }
  return { ...rest, sendUpdates: "none" };
}

const fail = (why: string) => ({
  content: [{ type: "text" as const, text: `Failed: ${why}` }],
  isError: true,
});

/**
 * A server exposing the selected tools verbatim.
 *
 * Schemas are passed through with fromJsonSchema rather than re-declared: the
 * upstream owns them, and re-typing ~120 tool schemas by hand would be wrong
 * within a release.
 */
function createServer(): McpServer {
  const server = new McpServer({ name: "gworkspace", version: "0.1.0" });

  for (const tool of exposedTools) {
    server.registerTool(
      tool.name,
      {
        description: tool.description ?? `Google Workspace: ${tool.name}`,
        inputSchema: fromJsonSchema(tool.inputSchema),
      },
      async (args: unknown) => {
        const raw = (args ?? {}) as Record<string, unknown>;
        // Argument KEYS only. The values are document text, spreadsheet cells,
        // search queries and calendar descriptions — user content, sometimes
        // personal, going into a log file with no retention policy of its own.
        console.log(`[tool] ${tool.name} (${Object.keys(raw).join(", ")})`);
        if (!upstream) return fail("the Workspace server has exited.");
        const callArgs = ARGUMENT_GUARDS[tool.name]?.(raw) ?? raw;
        try {
          const result = await upstream.callTool({
            name: tool.name,
            arguments: callArgs,
          });
          return result as CallToolResult;
        } catch (error) {
          return fail(error instanceof Error ? error.message : String(error));
        }
      },
    );
  }

  return server;
}

const app = express();

// NOT app-level: mounted on the tool route only, and after the auth check, so
// anything else on the shared Docker network cannot make this 0.25-vCPU service
// parse megabytes of JSON per request with no credential.
const parseJson = express.json({ limit: "4mb" });

app.get("/healthz", (_req, res) => {
  const healthy = Boolean(upstream);
  // 503, not a false 200: this is what autoheal and the deploy's health wait
  // read, and a relay with a dead child serves every tool call as an error.
  res.status(healthy ? 200 : 503).json({
    status: healthy,
    tools: exposedTools.length,
    upstreamTools: upstreamToolCount,
  });
});

let bearerToken = "";

function tokenMatches(provided: string): boolean {
  if (!bearerToken) return false;
  // Digest both sides first: timingSafeEqual throws on a length mismatch, which
  // would itself be the leak.
  const a = crypto.createHash("sha256").update(provided).digest();
  const b = crypto.createHash("sha256").update(bearerToken).digest();
  return crypto.timingSafeEqual(a, b);
}

app.post("/mcp", (req, res, next) => {
  const provided = (req.headers.authorization ?? "").replace(/^Bearer\s+/i, "");
  if (!tokenMatches(provided)) {
    console.warn(`[auth] rejected request from ${req.ip ?? "unknown"}`);
    res.status(401).json({ error: "unauthorized" });
    return;
  }
  parseJson(req, res, next);
});

app.post("/mcp", async (req, res) => {
  const transport = new NodeStreamableHTTPServerTransport({
    sessionIdGenerator: undefined,
  });
  let server: McpServer;
  try {
    server = createServer();
  } catch (error) {
    // Degrade one request rather than every request. Registration is validated
    // at boot, so reaching here means a schema the SDK's Ajv would not compile.
    console.error(
      `[mcp] could not build the tool server: ${error instanceof Error ? error.message : String(error)}`,
    );
    res.status(503).json({ error: "tools unavailable" });
    return;
  }
  res.on("close", () => {
    void transport.close?.();
    void server.close?.();
  });
  await server.connect(transport);
  await transport.handleRequest(req, res, req.body);
});

// Registered after the POST routes, so POST still wins.
app.all("/mcp", (_req, res) => {
  res.status(405).set("Allow", "POST").json({
    jsonrpc: "2.0",
    error: {
      code: -32000,
      message: "Method not allowed. This server is stateless; use POST.",
    },
    id: null,
  });
});

async function main() {
  await new Promise<void>((resolve) => {
    app.listen(config.port, "0.0.0.0", () => {
      console.log(`gworkspace-mcp listening on :${config.port}`);
      resolve();
    });
  });

  // Concurrently: four sequential fetches against a slow vault is 4 x (4
  // attempts x 15s timeout + backoff), which outlasts both start_period and the
  // deploy's health wait.
  const [token, clientId, clientSecret, tokenJson] = await Promise.all([
    vaultFetch("GWORKSPACE_MCP_TOKEN"),
    vaultFetch("GOOGLE_CLIENT_ID"),
    vaultFetch("GOOGLE_CLIENT_SECRET"),
    vaultFetch("GOOGLE_TOKEN_JSON"),
  ]);
  bearerToken = token;

  // Nothing reads the vault after boot, and this process spawns a child that
  // parses untrusted document text. Drop the capability once it is spent.
  delete process.env.SECRETS_MANAGER_TOKEN;
  config.vault.token = "";

  await connectUpstream({ clientId, clientSecret, tokenJson });
  console.log(
    `exposing: ${exposedTools.map((t) => t.name).join(", ") || "(none)"}`,
  );

  // The upstream's authorize() makes no network call, so a revoked or expired
  // grant boots perfectly and then fails every single user action. One cheap
  // call turns that into a line in the log at the moment it becomes true.
  // Deliberately not fatal: a Google blip should not crash-loop the container.
  try {
    await upstream?.callTool({
      name: "listDocuments",
      arguments: { maxResults: 1 },
    });
    console.log("[upstream] credential verified");
  } catch (error) {
    console.error(
      `[upstream] CREDENTIAL CHECK FAILED - Workspace calls will fail until this is fixed: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
