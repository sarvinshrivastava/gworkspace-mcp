/**
 * gworkspace-mcp — puts the Google Workspace MCP server on Streamable HTTP,
 * one policy-scoped identity per agent.
 *
 * The upstream server (@a-bonus/google-docs-mcp) speaks stdio. Open WebUI's MCP
 * client speaks Streamable HTTP and nothing else — its utils/mcp/client.py
 * imports streamablehttp_client alone — so the two cannot talk directly. This
 * process runs the stdio server as a child and relays between them, deciding
 * per bearer which tools exist and which files they may touch.
 *
 * Two SDK majors on purpose: v2 has no stdio client transport, v1 does, and the
 * two speak the same protocol over the wire.
 *
 * This file is boot wiring only:
 *   1. open the policy DB (before any listener, so no request sees it half-open)
 *   2. listen on :3014 (MCP) and :3015 (admin)
 *   3. read the vault, build the Drive client, start the child, mark ready
 */

import { createAdminApp } from "./admin/app.js";
import { makeAllowedSources } from "./admin/security.js";
import { Audit, DAY_MS } from "./audit.js";
import { config, vaultFetch, vaultFetchOptional } from "./config.js";
import { DriveClient } from "./drive/client.js";
import { ScopeResolver } from "./enforce/resolver.js";
import { SubtreeCache } from "./enforce/search.js";
import { errorMessage } from "./errors.js";
import { createMcpApp } from "./gateway.js";
import {
  makeAuthenticator,
  makeDegradedAuthenticator,
  type Authenticator,
} from "./policy/auth.js";
import { DEFAULT_GRANTS } from "./policy/derive.js";
import { OpenError, backupDaily, openPolicyStore, type PolicyStore } from "./policy/store.js";
import {
  connectUpstream,
  describeResult,
  normaliseToken,
  refreshTokenOf,
  upstreamState,
} from "./upstream.js";

let store: PolicyStore | null = null;
let dbError: string | null = null;
let audit: Audit | null = null;
let legacyToken = "";
let passwordHash: string | null = null;
let drive: DriveClient | null = null;
let ready = false;

/**
 * The policy DB, or degraded mode.
 *
 * A database that EXISTED may hold restrictions — `default` narrowed, tokens
 * revoked — so failing to read it must stop the service, visibly, rather than
 * serve without them. A database that never existed and cannot be created
 * (first boot on a missing or read-only volume) restricts nothing: the legacy
 * bearer gets exactly what a first boot would have seeded, and Open WebUI
 * keeps working while /healthz says `db: false`.
 */
function openStore(): void {
  try {
    store = openPolicyStore(config.dbPath);
  } catch (error) {
    if (error instanceof OpenError && !error.existed) {
      dbError = error.message;
      console.error(
        `[db] DEGRADED: cannot create the policy database at ${config.dbPath} (${errorMessage(error)}). Only the legacy bearer is served, with the first-boot tool set; minted tokens are refused; the dashboard is unavailable. Mount a writable volume at ${config.dbPath.replace(/\/[^/]+$/, "")}.`,
      );
      return;
    }
    console.error(
      `FATAL: policy DB exists but cannot be opened at ${config.dbPath}: ${errorMessage(error)}. Refusing to start without the restrictions it holds. Restore it from ${config.backupDir} (see docker-compose.yml) or fix its permissions.`,
    );
    process.exit(1);
  }

  if (store.ensureDefaultAgent(DEFAULT_GRANTS)) {
    console.log("[db] created the default agent for the legacy bearer, with every mapped tool");
  }
  audit = new Audit(store);

  const housekeeping = () => {
    audit?.prune();
    if (store) {
      backupDaily(store, config.backupDir).catch((error) =>
        console.error(`[db] backup failed: ${errorMessage(error)}`),
      );
    }
  };
  housekeeping();
  setInterval(housekeeping, DAY_MS).unref();
}

async function main() {
  openStore();

  const legacy = () => legacyToken;
  const authenticator: Authenticator = store
    ? makeAuthenticator(store, legacy)
    : makeDegradedAuthenticator(legacy);
  const resolver = new ScopeResolver(() => drive);
  const subtree = new SubtreeCache(() => drive);

  const mcpApp = createMcpApp({
    authenticate: () => authenticator,
    audit: () => audit,
    pipeline: { resolver, subtree, drive: () => drive },
    upstream: upstreamState,
    ready: () => ready,
    health: () => ({ db: store !== null, auditErrors: audit?.errors ?? 0 }),
  });

  const adminApp = createAdminApp({
    store: () => store,
    dbError: () => dbError,
    audit: () => audit,
    drive: () => drive,
    resolver,
    subtree,
    passwordHash: () => passwordHash,
    exposedOrder: () => upstreamState.exposedTools.map((t) => t.name),
    withheld: () => upstreamState.withheld,
    allowed: makeAllowedSources(config.adminAllowedSources),
    allowedHosts: config.adminAllowedHosts,
  });

  await Promise.all([
    new Promise<void>((resolve) => {
      mcpApp.listen(config.port, "0.0.0.0", () => {
        console.log(`gworkspace-mcp listening on :${config.port}`);
        resolve();
      });
    }),
    new Promise<void>((resolve) => {
      // 0.0.0.0 so the 127.0.0.1-only host publish can reach it; the app's
      // source allowlist is what keeps other containers out.
      adminApp.listen(config.adminPort, "0.0.0.0", () => {
        console.log(`admin dashboard listening on :${config.adminPort}`);
        resolve();
      });
    }),
  ]);

  // Concurrently: sequential fetches against a slow vault is n x (4 attempts x
  // 15s timeout + backoff), which outlasts both start_period and the deploy's
  // health wait.
  const [token, clientId, clientSecret, tokenJson, hash] = await Promise.all([
    vaultFetch("GWORKSPACE_MCP_TOKEN"),
    vaultFetch("GOOGLE_CLIENT_ID"),
    vaultFetch("GOOGLE_CLIENT_SECRET"),
    vaultFetch("GOOGLE_TOKEN_JSON"),
    vaultFetchOptional("DASHBOARD_ADMIN_PASSWORD_HASH"),
  ]);
  legacyToken = token;
  passwordHash = hash;
  if (!hash) console.warn("[admin] no DASHBOARD_ADMIN_PASSWORD_HASH in the vault — dashboard login is disabled");

  // Nothing reads the vault after boot. This removes the token from this
  // process's view and from anything it spawns later — NOT from
  // /proc/<pid>/environ, which keeps the original block and is readable by the
  // child (same uid). Bounding that needs the vault-folder migration (spec D8).
  delete process.env.SECRETS_MANAGER_TOKEN;
  config.vault.token = "";

  // The gateway's own Drive client, for scope lookups. Same credential as the
  // child; it never leaves this process.
  // Normalised once here; connectUpstream's own pass is then silent.
  const normalised = normaliseToken(tokenJson);
  drive = new DriveClient({ clientId, clientSecret, refreshToken: refreshTokenOf(normalised) });

  await connectUpstream({ clientId, clientSecret, tokenJson: normalised });
  ready = true;
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
      `[upstream] CREDENTIAL CHECK could not run: ${errorMessage(error)}`,
    );
  }
}

// Node as PID 1 under `init: true` gets SIGTERM on every deploy. Closing the
// database checkpoints the WAL, so a restore never meets a stale -wal file.
for (const signal of ["SIGTERM", "SIGINT"] as const) {
  process.on(signal, () => {
    try {
      store?.close();
    } finally {
      process.exit(0);
    }
  });
}

main().catch((error) => {
  console.error(errorMessage(error));
  process.exit(1);
});
