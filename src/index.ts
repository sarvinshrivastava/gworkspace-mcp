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

import express from "express";
import { McpServer } from "@modelcontextprotocol/server";
import { NodeStreamableHTTPServerTransport } from "@modelcontextprotocol/node";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
import { fromJsonSchema } from "@modelcontextprotocol/server";

import { config, vaultFetch } from "./config.js";

type UpstreamTool = {
  name: string;
  description?: string;
  inputSchema: Record<string, unknown>;
};

let upstream: Client | null = null;
let upstreamTools: UpstreamTool[] = [];

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
  // boot from the vault keeps the credential out of the image and off the disk
  // image, though it does land in the container's own filesystem.
  const { promises: fs } = await import("node:fs");
  const path = await import("node:path");
  const tokenPath = path.join(config.tokenDir, "token.json");
  await fs.mkdir(config.tokenDir, { recursive: true });
  await fs.writeFile(tokenPath, credentials.tokenJson, { mode: 0o600 });

  // The upstream is a pinned dependency, not `npx -y` at runtime: that would
  // fetch a package over the network on every boot, resolve a different version
  // than was tested, and put a live download in the startup path of a service
  // holding Google credentials.
  const transport = new StdioClientTransport({
    command: "node",
    args: [config.upstreamEntry],
    env: {
      ...process.env,
      GOOGLE_CLIENT_ID: credentials.clientId,
      GOOGLE_CLIENT_SECRET: credentials.clientSecret,
      // The upstream resolves its token store relative to XDG config.
      XDG_CONFIG_HOME: config.configHome,
      HOME: config.configHome,
    } as Record<string, string>,
    stderr: "pipe",
  });

  const client = new Client(
    { name: "gworkspace-bridge", version: "0.1.0" },
    { capabilities: {} },
  );

  await client.connect(transport);
  const listed = await client.listTools();
  upstream = client;
  upstreamTools = listed.tools as UpstreamTool[];

  console.log(`[upstream] connected, ${upstreamTools.length} tools available`);
}

/**
 * A server exposing the upstream's tools verbatim.
 *
 * Schemas are passed through with fromJsonSchema rather than re-declared: the
 * upstream owns them, and re-typing 118 tool schemas by hand would be wrong
 * within a release.
 */
function createServer(): McpServer {
  const server = new McpServer({ name: "gworkspace", version: "0.1.0" });

  for (const tool of upstreamTools) {
    if (
      config.toolAllowlist.length &&
      !config.toolAllowlist.includes(tool.name)
    ) {
      continue;
    }

    server.registerTool(
      tool.name,
      {
        description: tool.description ?? `Google Workspace: ${tool.name}`,
        inputSchema: fromJsonSchema(tool.inputSchema as never),
      },
      async (args: unknown) => {
        const callArgs = (args ?? {}) as Record<string, unknown>;
        console.log(
          `[tool] ${tool.name} ${JSON.stringify(callArgs).slice(0, 160)}`,
        );
        if (!upstream) {
          return {
            content: [
              {
                type: "text" as const,
                text: "Failed: the Workspace server is not connected.",
              },
            ],
            isError: true,
          };
        }
        try {
          const result = await upstream.callTool({
            name: tool.name,
            arguments: callArgs,
          });
          return result as never;
        } catch (error) {
          const message =
            error instanceof Error ? error.message : String(error);
          return {
            content: [{ type: "text" as const, text: `Failed: ${message}` }],
            isError: true,
          };
        }
      },
    );
  }

  return server;
}

const app = express();
app.use(express.json({ limit: "4mb" }));

app.get("/healthz", (_req, res) => {
  res.json({ status: Boolean(upstream), tools: upstreamTools.length });
});

let bearerToken = "";

app.post("/mcp", async (req, res) => {
  const provided = (req.headers.authorization ?? "").replace(/^Bearer\s+/i, "");
  if (!bearerToken || provided !== bearerToken) {
    console.warn(`[auth] rejected request from ${req.ip ?? "unknown"}`);
    res.status(401).json({ error: "unauthorized" });
    return;
  }

  const transport = new NodeStreamableHTTPServerTransport({
    sessionIdGenerator: undefined,
  });
  const server = createServer();
  res.on("close", () => {
    void transport.close?.();
    void server.close?.();
  });
  await server.connect(transport);
  await transport.handleRequest(req, res, req.body);
});

async function main() {
  bearerToken = await vaultFetch("GWORKSPACE_MCP_TOKEN");

  await connectUpstream({
    clientId: await vaultFetch("GOOGLE_CLIENT_ID"),
    clientSecret: await vaultFetch("GOOGLE_CLIENT_SECRET"),
    tokenJson: await vaultFetch("GOOGLE_TOKEN_JSON"),
  });

  app.listen(config.port, "0.0.0.0", () => {
    console.log(`gworkspace-mcp listening on :${config.port}`);
    if (config.toolAllowlist.length) {
      console.log(`tool allowlist: ${config.toolAllowlist.join(", ")}`);
    }
  });
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
