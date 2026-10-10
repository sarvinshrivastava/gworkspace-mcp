/**
 * The upstream child: starting it, watching it, and deciding which of its tools
 * this relay may expose at all.
 *
 * Moved out of index.ts unchanged so the gateway can be tested against a fake
 * client. The one addition is the tripwire, which guards the policy engine
 * against an upstream release that quietly changes a tool's arguments.
 */

import { promises as fs } from "node:fs";
import path from "node:path";
import type { JsonSchemaType } from "@modelcontextprotocol/server";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

import { config } from "./config.js";

export type UpstreamTool = {
  name: string;
  description?: string;
  inputSchema: JsonSchemaType;
};

/** What the gateway needs from the child. A Client satisfies it. */
export type UpstreamClient = {
  callTool(params: {
    name: string;
    arguments: Record<string, unknown>;
  }): Promise<unknown>;
};

export type Withheld = { scoped: Set<string>; all: Set<string> };

export type UpstreamState = {
  client: UpstreamClient | null;
  /** The tools actually registered — deduplicated and allowlisted at boot. */
  exposedTools: UpstreamTool[];
  upstreamToolCount: number;
  /** Tools the tripwire flagged, per population. */
  withheld: Withheld;
};

export const upstreamState: UpstreamState = {
  client: null,
  exposedTools: [],
  upstreamToolCount: 0,
  withheld: { scoped: new Set(), all: new Set() },
};

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
export function normaliseToken(raw: string): string {
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

/** The refresh token inside an already-normalised token document. */
export function refreshTokenOf(normalisedJson: string): string {
  return (JSON.parse(normalisedJson) as { refresh_token: string }).refresh_token;
}

/**
 * Start the stdio child once and keep it. Google's OAuth refresh happens inside
 * that process, so restarting it per request would re-do the token dance every
 * time and lose its cache.
 */
export async function connectUpstream(credentials: {
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
    upstreamState.client = null;
  };

  const listed = await client.listTools();
  const tools = listed.tools as UpstreamTool[];
  upstreamState.upstreamToolCount = tools.length;
  upstreamState.exposedTools = selectTools(tools, config.toolAllowlist);
  upstreamState.withheld = tripwire(upstreamState.exposedTools);
  upstreamState.client = client;

  console.log(
    `[upstream] connected, ${upstreamState.upstreamToolCount} tools available, ${upstreamState.exposedTools.length} exposed`,
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
export function selectTools(
  tools: UpstreamTool[],
  allowed: string[],
): UpstreamTool[] {
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
    console.warn(`[tools] not found upstream, ignored: ${missing.join(", ")}`);
  }

  return [...byName.values()];
}

/**
 * The exact argument names of every tool the policy engine reads or rewrites,
 * as measured against @a-bonus/google-docs-mcp@1.11.3.
 *
 * Exact, not "contains": an upstream release that ADDS an argument is as
 * dangerous as one that renames one. A new `templateId` on createDocument, or a
 * raw `q` on listDriveFiles, is a way out of a folder scope that the gateway
 * would relay without ever looking at.
 */
export const EXPECTED_KEYS: Record<
  string,
  { keys: readonly string[]; population: "scoped" | "all" }
> = {
  readDocument: { population: "scoped", keys: ["documentId", "format", "maxLength", "tabId"] },
  appendText: { population: "scoped", keys: ["documentId", "text", "addNewlineIfNeeded", "tabId"] },
  insertText: { population: "scoped", keys: ["documentId", "text", "index", "tabId"] },
  findAndReplace: { population: "scoped", keys: ["documentId", "findText", "replaceText", "matchCase", "tabId"] },
  appendMarkdown: { population: "scoped", keys: ["documentId", "markdown", "addNewlineIfNeeded", "tabId", "firstHeadingAsTitle"] },
  createDocument: { population: "scoped", keys: ["title", "parentFolderId", "initialContent", "contentFormat"] },
  readSpreadsheet: { population: "scoped", keys: ["spreadsheetId", "range", "valueRenderOption"] },
  writeSpreadsheet: { population: "scoped", keys: ["spreadsheetId", "range", "values", "valueInputOption"] },
  appendRows: { population: "scoped", keys: ["spreadsheetId", "range", "values", "valueInputOption"] },
  createSpreadsheet: { population: "scoped", keys: ["title", "parentFolderId", "initialData"] },
  listDriveFiles: { population: "scoped", keys: ["maxResults", "mimeType", "folderId", "orderBy", "sortDirection", "ownedByMe", "sharedWithMe", "modifiedAfter"] },
  searchDriveFiles: { population: "scoped", keys: ["query", "searchIn", "mimeType", "folderId", "orderBy", "sortDirection", "maxResults", "modifiedAfter", "pageToken"] },
  createFolder: { population: "scoped", keys: ["name", "parentFolderId"] },
  createEvent: { population: "all", keys: ["calendarId", "summary", "description", "location", "start", "end", "attendees", "sendUpdates", "conferenceData"] },
  updateEvent: { population: "all", keys: ["calendarId", "eventId", "summary", "description", "location", "start", "end", "attendees", "sendUpdates"] },
  quickAddEvent: { population: "all", keys: ["calendarId", "text", "sendUpdates"] },
};

/**
 * Compares each policed tool's argument names with what the policy engine was
 * written against. A mismatch withholds the tool from the population whose
 * enforcement depends on those arguments, and says so loudly.
 *
 * Never a crash: a thrown boot is an Open WebUI outage, and the unaffected
 * tools are still safe to serve.
 */
export function tripwire(tools: UpstreamTool[]): Withheld {
  const withheld: Withheld = { scoped: new Set(), all: new Set() };
  for (const tool of tools) {
    const expected = EXPECTED_KEYS[tool.name];
    if (!expected) continue;
    const properties = (tool.inputSchema as { properties?: Record<string, unknown> })
      .properties;
    const got = Object.keys(properties ?? {}).sort();
    const want = [...expected.keys].sort();
    if (got.join(",") === want.join(",")) continue;
    console.error(
      `[tripwire] ${tool.name}: the upstream's arguments changed (expected ${want.join(", ")}; got ${got.join(", ")}) — withholding it from ${expected.population === "all" ? "every agent" : "folder-scoped agents"} until the policy engine is updated`,
    );
    withheld[expected.population].add(tool.name);
  }
  return withheld;
}

/** The text of a tool result, for logging. Truncated; never the arguments. */
export function describeResult(result: unknown): string {
  const content = (result as { content?: Array<{ type?: string; text?: string }> })
    .content;
  if (!Array.isArray(content)) return "(no content)";
  return content
    .map((part) => (part.type === "text" ? part.text : `<${part.type}>`))
    .join(" ")
    .slice(0, 300);
}
