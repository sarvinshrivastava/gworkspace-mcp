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
import express from "express";
import { McpServer, fromJsonSchema } from "@modelcontextprotocol/server";
import type { CallToolResult } from "@modelcontextprotocol/server";
import { NodeStreamableHTTPServerTransport } from "@modelcontextprotocol/node";

import { config, vaultFetch } from "./config.js";
import { connectUpstream, describeResult, upstreamState } from "./upstream.js";

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

  for (const tool of upstreamState.exposedTools) {
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
        const upstream = upstreamState.client;
        if (!upstream) return fail("the Workspace server has exited.");
        const callArgs = ARGUMENT_GUARDS[tool.name]?.(raw) ?? raw;
        try {
          const result = await upstream.callTool({
            name: tool.name,
            arguments: callArgs,
          });
          // The upstream reports tool failures through FastMCP's log channel,
          // which is an MCP notification rather than stderr — so a failing call
          // left NOTHING in docker logs and the only account of it was whatever
          // the model chose to say. The relay sees every result, so it logs the
          // failures here.
          if ((result as { isError?: boolean }).isError) {
            console.error(`[tool] ${tool.name} FAILED: ${describeResult(result)}`);
          }
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
  const healthy = Boolean(upstreamState.client);
  // 503, not a false 200: this is what autoheal and the deploy's health wait
  // read, and a relay with a dead child serves every tool call as an error.
  res.status(healthy ? 200 : 503).json({
    status: healthy,
    tools: upstreamState.exposedTools.length,
    upstreamTools: upstreamState.upstreamToolCount,
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
    `exposing: ${upstreamState.exposedTools.map((t) => t.name).join(", ") || "(none)"}`,
  );

  // The upstream's authorize() makes no network call, so a revoked or expired
  // grant boots perfectly and then fails every single user action. One cheap
  // call turns that into a line in the log at the moment it becomes true.
  // Deliberately not fatal: a Google blip should not crash-loop the container.
  try {
    const probe = await upstreamState.client?.callTool({
      name: "listDocuments",
      arguments: { maxResults: 1 },
    });
    // callTool RESOLVES with isError: true for a tool failure — it only throws
    // on a transport failure. Checking the promise alone reported "verified"
    // for a call that had actually come back 403.
    if (probe && (probe as { isError?: boolean }).isError) {
      console.error(
        `[upstream] CREDENTIAL CHECK FAILED — every Workspace call will fail until this is fixed: ${describeResult(probe)}`,
      );
      console.error(
        "[upstream] a 403 here is usually one of two things, and the upstream discards which: the Drive/Docs/Sheets API is not enabled on the OAuth client's Google Cloud PROJECT, or consent did not grant that scope. Check the project first — `gcloud services list --enabled` is authoritative, a live probe is not.",
      );
    } else {
      console.log("[upstream] credential verified");
    }
  } catch (error) {
    console.error(
      `[upstream] CREDENTIAL CHECK could not run: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
