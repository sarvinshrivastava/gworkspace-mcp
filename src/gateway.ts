/**
 * The MCP side: /healthz, POST /mcp and POST /internal/housekeeping on :3014.
 *
 * Stateless as before — one McpServer per request — so building it per AGENT
 * costs nothing extra. Each agent's tools/list holds only its enabled tools,
 * and a dashboard change applies on the agent's next request.
 */

import express from "express";
import { McpServer, fromJsonSchema } from "@modelcontextprotocol/server";
import type { CallToolResult } from "@modelcontextprotocol/server";
import { NodeStreamableHTTPServerTransport } from "@modelcontextprotocol/node";

import type { Audit } from "./audit.js";
import { DriveError } from "./drive/client.js";
import { denyMessage, planCall, type PipelineDeps } from "./enforce/pipeline.js";
import { ScopeTooLarge } from "./enforce/search.js";
import { errorMessage } from "./errors.js";
import type { Housekeeping } from "./housekeeping.js";
import { internalGuard } from "./internal-auth.js";
import type { AgentContext, Authenticator } from "./policy/auth.js";
import { enabledTools } from "./policy/derive.js";
import { describeResult, type UpstreamState } from "./upstream.js";

export type GatewayDeps = {
  /** A getter: degraded mode and normal mode use different authenticators. */
  authenticate: () => Authenticator;
  audit: () => Audit | null;
  pipeline: PipelineDeps;
  upstream: UpstreamState;
  /** False until the vault has answered and the child is up. */
  ready: () => boolean;
  health: () => Record<string, unknown>;
  /** A getter: null until the policy store has opened (and in degraded mode). */
  housekeeping: () => Housekeeping | null;
  /** pipeline-runner's bearer for /internal/*; "" when the vault has none. */
  plrToken: () => string;
};

const fail = (why: string): CallToolResult => ({
  content: [{ type: "text" as const, text: `Failed: ${why}` }],
  isError: true,
});

export function toolsFor(ctx: AgentContext, upstream: UpstreamState): string[] {
  return enabledTools(
    ctx.grants,
    ctx.scoped,
    upstream.exposedTools.map((t) => t.name),
    upstream.withheld,
  );
}

/**
 * A server exposing this agent's tools.
 *
 * Schemas are passed through with fromJsonSchema rather than re-declared: the
 * upstream owns them, and re-typing them by hand would be wrong within a
 * release. Every call then goes through the policy pipeline.
 */
export function createServer(ctx: AgentContext, deps: GatewayDeps): McpServer {
  const server = new McpServer({ name: "gworkspace", version: "0.2.0" });
  const enabled = new Set(toolsFor(ctx, deps.upstream));

  for (const tool of deps.upstream.exposedTools) {
    if (!enabled.has(tool.name)) continue;
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
        console.log(`[tool] ${ctx.slug} ${tool.name} (${Object.keys(raw).join(", ")})`);
        const audit = deps.audit();
        const upstream = deps.upstream.client;
        if (!upstream) {
          audit?.tool(ctx, tool.name, "error", "upstream_down", []);
          return fail("the Workspace server has exited.");
        }

        let resources: string[] = [];
        try {
          const plan = await planCall(ctx, tool.name, raw, deps.pipeline);
          resources = plan.resources;

          if (plan.kind === "deny") {
            audit?.tool(ctx, tool.name, "deny", plan.reason, plan.resources);
            return fail(plan.message);
          }

          if (plan.kind === "serve") {
            try {
              const text = await plan.run();
              audit?.tool(ctx, tool.name, "allow", "served", plan.resources);
              return { content: [{ type: "text" as const, text }] };
            } catch (error) {
              if (error instanceof ScopeTooLarge) {
                audit?.tool(ctx, tool.name, "deny", "too_large", plan.resources);
                return fail(denyMessage("too_large"));
              }
              // Drive not answering (rate limit, 5xx, timeout): "try again",
              // as everywhere else — never Drive's raw status text.
              if ((error instanceof DriveError && error.retryable) || isAbort(error)) {
                audit?.tool(ctx, tool.name, "deny", "drive_error", plan.resources);
                return fail(denyMessage("drive_error"));
              }
              throw error;
            }
          }

          const result = await upstream.callTool({ name: tool.name, arguments: plan.args });
          // The upstream reports tool failures through FastMCP's log channel,
          // which is an MCP notification rather than stderr — so a failing call
          // left NOTHING in docker logs and the only account of it was whatever
          // the model chose to say. The relay sees every result, so it logs the
          // failures here.
          if ((result as { isError?: boolean }).isError) {
            console.error(`[tool] ${tool.name} FAILED: ${describeResult(result)}`);
            audit?.tool(ctx, tool.name, "error", "upstream", plan.resources);
          } else {
            audit?.tool(ctx, tool.name, plan.mutated ? "mutate" : "allow", "", plan.resources);
            plan.after?.();
          }
          return result as CallToolResult;
        } catch (error) {
          const message = errorMessage(error);
          audit?.tool(ctx, tool.name, "error", "exception", resources);
          return fail(message);
        }
      },
    );
  }

  return server;
}

export function createMcpApp(deps: GatewayDeps): express.Express {
  const app = express();

  // NOT app-level: mounted on the tool route only, and after the auth check, so
  // anything else on the shared Docker network cannot make this 0.25-vCPU service
  // parse megabytes of JSON per request with no credential.
  const parseJson = express.json({ limit: "4mb" });

  app.get("/healthz", (_req, res) => {
    const healthy = Boolean(deps.upstream.client);
    // 503, not a false 200: this is what autoheal and the deploy's health wait
    // read, and a relay with a dead child serves every tool call as an error.
    res.status(healthy ? 200 : 503).json({
      status: healthy,
      tools: deps.upstream.exposedTools.length,
      upstreamTools: deps.upstream.upstreamToolCount,
      ...deps.health(),
    });
  });

  // pipeline-runner's daily call. Gated on the store only, not on ready():
  // ready waits for Google, and a prune and a backup are DB-only, so a Google
  // outage must not stop them. No body is read.
  // Its own router, case-sensitive and strict, so /internal/HOUSEKEEPING and
  // /internal/housekeeping/ are not this route either (the guard already
  // refuses a case variant of the prefix).
  const internal = express.Router({ caseSensitive: true, strict: true });
  internal.post("/housekeeping", async (_req, res) => {
    const housekeeping = deps.housekeeping();
    if (!housekeeping) {
      res.status(503).json({ error: "no_store" });
      return;
    }
    try {
      res.json(await housekeeping());
    } catch (error) {
      // The detail stays in this log: the caller only needs the status to
      // retry and page, and it keeps run output that may travel further.
      console.error(`[db] housekeeping failed: ${errorMessage(error)}`);
      res.status(500).json({ error: "housekeeping_failed" });
    }
  });
  app.use(
    "/internal",
    internalGuard(deps.plrToken, {
      onReject: (source) => deps.audit()?.authFailure(source, "internal_bad_bearer"),
    }),
    internal,
  );

  app.post("/mcp", (req, res, next) => {
    // Before boot finishes there is no legacy bearer and no tool list; an
    // answer now would be a 401 or an empty tools/list that Open WebUI caches.
    if (!deps.ready()) {
      res.status(503).json({ error: "starting" });
      return;
    }
    const provided = (req.headers.authorization ?? "").replace(/^Bearer\s+/i, "");
    let ctx: AgentContext | null;
    try {
      ctx = deps.authenticate()(provided);
    } catch (error) {
      // The policy DB could not answer (busy past its timeout, I/O). Neither
      // "unauthorized" nor a stack trace: say what happened.
      console.error(`[auth] policy lookup failed: ${errorMessage(error)}`);
      res.status(503).json({ error: "policy unavailable" });
      return;
    }
    if (!ctx) {
      const source = req.socket.remoteAddress ?? "";
      console.warn(`[auth] rejected request from ${source || "unknown"}`);
      deps.audit()?.authFailure(source);
      res.status(401).json({ error: "unauthorized" });
      return;
    }
    res.locals.agent = ctx;
    parseJson(req, res, next);
  });

  app.post("/mcp", async (req, res) => {
    const ctx = res.locals.agent as AgentContext;

    // Batching was removed from the protocol in 2025-06-18 and Open WebUI
    // never sends it; refusing it keeps one request = one decision.
    if (Array.isArray(req.body)) {
      res.status(400).json({ error: "batch requests are not supported" });
      return;
    }

    // The SDK answers a disabled tool itself (-32602, "not found"); this only
    // makes sure the refusal is on record.
    const body = req.body as { method?: unknown; params?: { name?: unknown } } | undefined;
    if (body?.method === "tools/call" && typeof body.params?.name === "string") {
      const name = body.params.name;
      if (!toolsFor(ctx, deps.upstream).includes(name)) {
        deps.audit()?.tool(ctx, name.slice(0, 100), "deny", "not_enabled", []);
      }
    }

    const transport = new NodeStreamableHTTPServerTransport({
      sessionIdGenerator: undefined,
    });
    let server: McpServer;
    try {
      server = createServer(ctx, deps);
    } catch (error) {
      // Degrade one request rather than every request. Registration is validated
      // at boot, so reaching here means a schema the SDK's Ajv would not compile.
      console.error(
        `[mcp] could not build the tool server: ${errorMessage(error)}`,
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

  // Whatever a handler did not catch — the SDK transport failing, say.
  app.use((error: unknown, _req: express.Request, res: express.Response, next: express.NextFunction) => {
    if (res.headersSent) return next(error);
    // A body the JSON parser refused is the client's error, in JSON-RPC terms.
    const status = (error as { status?: unknown }).status;
    if (typeof status === "number" && status >= 400 && status < 500) {
      res.status(status).json({
        jsonrpc: "2.0",
        error: { code: status === 400 ? -32700 : -32600, message: status === 400 ? "Parse error" : "Invalid request" },
        id: null,
      });
      return;
    }
    console.error(`[mcp] unhandled error: ${errorMessage(error)}`);
    res.status(500).json({
      jsonrpc: "2.0",
      error: { code: -32603, message: "Internal error" },
      id: null,
    });
  });

  return app;
}

const isAbort = (error: unknown) =>
  error instanceof Error && (error.name === "AbortError" || error.name === "TimeoutError");
