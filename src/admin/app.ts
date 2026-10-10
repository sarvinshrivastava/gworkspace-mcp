/**
 * The admin dashboard on :3015 — a separate Express app, so no /mcp route
 * exists on this port at all.
 *
 * Reached through an SSH tunnel. The listener binds 0.0.0.0 inside the
 * container (so the published 127.0.0.1:3015 works), which also makes it
 * reachable from every container on the shared network: the source allowlist
 * runs before anything else, on the socket address.
 */

import express from "express";

import type { Audit } from "../audit.js";
import { FOLDER_MIME, type DriveClient } from "../drive/client.js";
import { errorMessage } from "../errors.js";
import { ID_RE } from "../enforce/guards.js";
import type { ScopeResolver } from "../enforce/resolver.js";
import type { SubtreeCache } from "../enforce/search.js";
import { enabledTools, isGrantKey } from "../policy/derive.js";
import { mintToken } from "../policy/tokens.js";
import {
  DEFAULT_AGENT,
  StoreError,
  type Area,
  type Grant,
  type Op,
  type PolicyStore,
} from "../policy/store.js";
import {
  LoginLimiter,
  SessionStore,
  isAllowed,
  isAllowedHost,
  safeEqual,
  usableHash,
  verifyPassword,
  type Session,
} from "./security.js";
import {
  agentPage,
  agentWarnings,
  AUDIT_DECISIONS,
  agentsPage,
  auditPage,
  browsePage,
  collisionWarnings,
  errorPage,
  loginPage,
} from "./views.js";

export const COOKIE = "gws_admin_sid";

export type AdminDeps = {
  store: () => PolicyStore | null;
  dbError: () => string | null;
  audit: () => Audit | null;
  drive: () => DriveClient | null;
  resolver: ScopeResolver;
  subtree: SubtreeCache;
  /** null until the vault has answered; "" when no hash is stored. */
  passwordHash: () => string | null;
  exposedOrder: () => string[];
  withheld: () => { scoped: ReadonlySet<string>; all: ReadonlySet<string> };
  allowed: () => Set<string>;
  /** Extra Host names, beyond loopback. */
  allowedHosts?: string[];
  sessions?: SessionStore;
  limiter?: LoginLimiter;
};

const STORE_MESSAGES: Record<StoreError["code"], string> = {
  invalid_slug: "Slugs are 2–32 characters: lowercase letters, digits and hyphens, not starting with a hyphen.",
  invalid_name: "The display name must be 1–80 characters.",
  exists: "That already exists. Archived agents keep their slug forever.",
  reserved: "The default agent cannot be archived or confined.",
  not_found: "Not found.",
  has_scopes: "Remove every folder first.",
};

const STORE_ACTION_MESSAGES: Partial<Record<string, string>> = {
  "scope.add:exists": "That folder is already in this agent's scope.",
};

function readCookie(header: string | undefined, name: string): string | undefined {
  for (const part of (header ?? "").split(";")) {
    const eq = part.indexOf("=");
    if (eq > 0 && part.slice(0, eq).trim() === name) return part.slice(eq + 1).trim();
  }
  return undefined;
}

/** A folder ID, or the ID inside a Drive folder URL. */
export function parseFolderInput(input: unknown): string | null {
  if (typeof input !== "string") return null;
  const value = input.trim();
  const fromUrl = /\/folders\/([A-Za-z0-9_-]+)/.exec(value)?.[1] ?? /[?&]id=([A-Za-z0-9_-]+)/.exec(value)?.[1];
  const id = fromUrl ?? value;
  return ID_RE.test(id) ? id : null;
}

export function createAdminApp(deps: AdminDeps): express.Express {
  const sessions = deps.sessions ?? new SessionStore();
  const limiter = deps.limiter ?? new LoginLimiter();
  setInterval(() => sessions.sweep(), 5 * 60_000).unref();

  const app = express();
  app.disable("x-powered-by");

  // 1. Who may connect at all. The socket, never a header.
  app.use((req, res, next) => {
    if (!isAllowed(req.socket.remoteAddress, deps.allowed())) {
      res.status(403).type("text/plain").send("forbidden");
      return;
    }
    next();
  });

  // 1b. And only under a loopback name: see isAllowedHost.
  app.use((req, res, next) => {
    if (!isAllowedHost(req.headers.host, deps.allowedHosts ?? [])) {
      res.status(403).type("text/plain").send("forbidden host");
      return;
    }
    next();
  });

  // 2. Headers. No script may run on these pages, from anywhere.
  app.use((_req, res, next) => {
    res.set({
      "Content-Security-Policy":
        "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
      "X-Content-Type-Options": "nosniff",
      "Referrer-Policy": "no-referrer",
      "Cache-Control": "no-store",
    });
    next();
  });

  app.use(express.urlencoded({ extended: false, limit: "64kb" }));

  const source = (req: express.Request) => req.socket.remoteAddress ?? "";

  const page = (res: express.Response, status: number, body: string) =>
    res.status(status).type("html").send(body);

  const dbUnavailable = (res: express.Response) =>
    page(res, 503, errorPage(503, `The policy database is unavailable: ${deps.dbError() ?? "unknown error"}`));

  // --- login ---------------------------------------------------------------

  const loginState = (req: express.Request) => {
    const hash = deps.passwordHash();
    if (hash === null) return "starting" as const;
    if (!hash) return "unconfigured" as const;
    // Malformed, or parameters scrypt rejects: every login would fail and spend
    // the shared lockout. Say so instead.
    if (!usableHash(hash)) return "misconfigured" as const;
    if (limiter.locked(source(req))) return "locked" as const;
    return "form" as const;
  };

  app.get("/login", (req, res) => {
    if (!deps.store()) return dbUnavailable(res);
    page(res, 200, loginPage(loginState(req)));
  });

  app.post("/login", async (req, res) => {
    if (!deps.store()) return page(res, 503, errorPage(503, "The policy database is unavailable."));
    const state = loginState(req);
    if (state === "locked") return page(res, 429, loginPage("locked"));
    if (state !== "form") return page(res, 503, loginPage(state));
    if (!limiter.acquire()) return page(res, 429, loginPage("form", "Another login is being checked. Try again."));
    let ok = false;
    try {
      const password = typeof req.body?.password === "string" ? req.body.password : "";
      ok = password.length > 0 && password.length <= 1024 && (await verifyPassword(password, deps.passwordHash()!));
    } finally {
      limiter.release();
    }
    if (!ok) {
      deps.audit()?.adminDeny("login", source(req));
      if (limiter.fail(source(req))) deps.audit()?.adminDeny("login_lockout", source(req));
      return page(res, 401, loginPage(limiter.locked(source(req)) ? "locked" : "form", "Wrong password."));
    }
    limiter.success(source(req));
    const session = sessions.create();
    deps.audit()?.admin("login", null, [], source(req));
    res.setHeader(
      "Set-Cookie",
      `${COOKIE}=${session.id}; HttpOnly; SameSite=Strict; Path=/${req.secure ? "; Secure" : ""}`,
    );
    res.redirect(303, "/");
  });

  // --- everything else needs a session (and a store) -----------------------

  app.use((req, res, next) => {
    const store = deps.store();
    if (!store) return dbUnavailable(res);
    const session = sessions.get(readCookie(req.headers.cookie, COOKIE));
    if (!session) return res.redirect(303, "/login");
    if (req.method === "POST") {
      const token = req.body?._csrf;
      if (typeof token !== "string" || !safeEqual(token, session.csrf)) {
        deps.audit()?.adminDeny("csrf", source(req));
        return page(res, 403, errorPage(403, "This form has expired. Go back, reload and try again."));
      }
    }
    res.locals.session = session;
    res.locals.store = store;
    next();
  });

  const ctx = (res: express.Response) => ({
    session: res.locals.session as Session,
    store: res.locals.store as PolicyStore,
  });

  /** Audits a dashboard change against the agent it touched, then redirects. */
  const done = (
    req: express.Request,
    res: express.Response,
    action: string,
    resources: string[],
    to: string,
    agent: string | null = typeof req.params.slug === "string" ? req.params.slug : null,
  ) => {
    deps.audit()?.admin(action, agent, resources, source(req));
    res.redirect(303, to);
  };

  /** Runs a store mutation; a StoreError becomes a flash message. */
  const attempt = (session: Session, action: string, fn: () => void): boolean => {
    try {
      fn();
      return true;
    } catch (error) {
      if (error instanceof StoreError) {
        session.flash.push(STORE_ACTION_MESSAGES[`${action}:${error.code}`] ?? STORE_MESSAGES[error.code]);
        return false;
      }
      throw error;
    }
  };

  const toolsOf = (store: PolicyStore, slug: string, scoped: boolean) =>
    enabledTools(store.getGrants(slug), scoped, deps.exposedOrder(), deps.withheld());

  const requireAgent = (req: express.Request, res: express.Response) => {
    const agent = ctx(res).store.getAgent(String(req.params.slug));
    if (!agent) page(res, 404, errorPage(404, "No such agent."));
    return agent;
  };

  /** For changes: an archived agent is a record, not something to edit. */
  const requireLiveAgent = (req: express.Request, res: express.Response) => {
    const agent = requireAgent(req, res);
    if (agent && agent.archivedAt !== null) {
      page(res, 409, errorPage(409, "This agent is archived and can no longer be changed."));
      return null;
    }
    return agent;
  };

  app.post("/logout", (req, res) => {
    sessions.destroy(ctx(res).session.id);
    res.setHeader("Set-Cookie", `${COOKIE}=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0`);
    deps.audit()?.admin("logout", null, [], source(req));
    res.redirect(303, "/login");
  });

  app.get("/", (_req, res) => {
    const { session, store } = ctx(res);
    const agents = store.listAgents().map((a) => ({
      ...a,
      tools: toolsOf(store, a.slug, a.scoped),
      activeTokens: store.listTokens(a.slug).filter((t) => t.revokedAt === null).length,
      scopes: store.getScopes(a.slug),
    }));
    const warnings = collisionWarnings(
      agents.map((a) => ({ slug: a.slug, tools: a.tools, credentialed: a.slug === DEFAULT_AGENT || a.activeTokens > 0 })),
    );
    const flash = session.flash.splice(0);
    page(res, 200, agentsPage({ agents, warnings, flash, csrf: session.csrf }));
  });

  app.post("/agents", (req, res) => {
    const { session, store } = ctx(res);
    const slug = String(req.body?.slug ?? "").trim();
    const name = String(req.body?.name ?? "");
    if (attempt(session, "agent.create", () => store.createAgent(slug, name))) {
      return done(req, res, "agent.create", [slug], `/agents/${slug}`);
    }
    res.redirect(303, "/");
  });

  app.get("/agents/:slug", (req, res) => {
    const agent = requireAgent(req, res);
    if (!agent) return;
    const { session, store } = ctx(res);
    const grants = store.getGrants(agent.slug);
    const scopes = store.getScopes(agent.slug);
    const tokens = store.listTokens(agent.slug);
    const pending: Array<[number, string]> = [];
    for (const t of tokens) {
      const plaintext = session.pending.get(t.id);
      if (plaintext !== undefined) {
        pending.push([t.id, plaintext]);
        session.pending.delete(t.id);
      }
    }
    page(
      res,
      200,
      agentPage({
        agent,
        grants,
        tools: toolsOf(store, agent.slug, agent.scoped),
        scopes,
        tokens,
        pending,
        flash: session.flash.splice(0),
        csrf: session.csrf,
        warnings: agentWarnings({ scoped: agent.scoped, scopes, grants }),
      }),
    );
  });

  app.post("/agents/:slug/grants", (req, res) => {
    const agent = requireLiveAgent(req, res);
    if (!agent) return;
    const values = ([] as unknown[]).concat(req.body?.g ?? []);
    if (!values.every((v): v is string => typeof v === "string" && isGrantKey(v))) {
      return page(res, 400, errorPage(400, "Unknown grant."));
    }
    const grants: Grant[] = values.map((v) => {
      const [area, op] = v.split(":") as [Area, Op];
      return { area, op };
    });
    ctx(res).store.setGrants(agent.slug, grants);
    done(req, res, "grants.set", [agent.slug], `/agents/${agent.slug}`);
  });

  app.post("/agents/:slug/archive", (req, res) => {
    const agent = requireLiveAgent(req, res);
    if (!agent) return;
    const { session, store } = ctx(res);
    if (attempt(session, "agent.archive", () => store.archiveAgent(agent.slug))) {
      return done(req, res, "agent.archive", [agent.slug], "/");
    }
    res.redirect(303, `/agents/${agent.slug}`);
  });

  app.post("/agents/:slug/unconfine", (req, res) => {
    const agent = requireLiveAgent(req, res);
    if (!agent) return;
    const { session, store } = ctx(res);
    if (req.body?.confirm !== "yes") {
      session.flash.push("Tick the confirmation to remove confinement.");
      return res.redirect(303, `/agents/${agent.slug}`);
    }
    if (attempt(session, "agent.unconfine", () => store.setUnconfined(agent.slug))) {
      return done(req, res, "agent.unconfine", [agent.slug], `/agents/${agent.slug}`);
    }
    res.redirect(303, `/agents/${agent.slug}`);
  });

  app.post("/agents/:slug/scopes", async (req, res) => {
    const agent = requireLiveAgent(req, res);
    if (!agent) return;
    const { session, store } = ctx(res);
    const back = `/agents/${agent.slug}`;
    const fail = (message: string) => {
      session.flash.push(message);
      res.redirect(303, back);
    };
    if (agent.slug === DEFAULT_AGENT) return fail(STORE_MESSAGES.reserved);
    const id = parseFolderInput(req.body?.folder);
    if (!id) return fail("That is not a folder ID or a Drive folder URL.");
    if (!deps.drive()) return fail("Drive is not connected yet. Try again in a moment.");

    let file;
    let root;
    try {
      // Live: this decides what an agent may reach, so no cached answer.
      file = await deps.resolver.node(id, true);
      root = await deps.resolver.node("root", false);
    } catch (error) {
      return fail(`Could not check that folder with Drive: ${errorMessage(error)}`);
    }
    if (!file) return fail("Drive cannot find that folder, or this account cannot see it.");
    if (file.mimeType !== FOLDER_MIME) return fail("That is a file, not a folder.");
    if (file.trashed) return fail("That folder is in the trash.");
    // The whole Drive is not a scope. Shared folders without a visible parent are fine.
    if (root && file.id === root.id) return fail("You cannot scope an agent to the Drive root.");

    const primary = req.body?.primary === "1";
    if (attempt(session, "scope.add", () => store.addScope(agent.slug, file.id, file.name, primary))) {
      deps.subtree.flush();
      return done(req, res, "scope.add", [agent.slug, file.id], back);
    }
    res.redirect(303, back);
  });

  app.post("/agents/:slug/scopes/:folderId/remove", (req, res) => {
    const agent = requireLiveAgent(req, res);
    if (!agent) return;
    const { session, store } = ctx(res);
    const folderId = String(req.params.folderId);
    if (attempt(session, "scope.remove", () => store.removeScope(agent.slug, folderId))) {
      deps.subtree.flush();
      return done(req, res, "scope.remove", [agent.slug, folderId], `/agents/${agent.slug}`);
    }
    res.redirect(303, `/agents/${agent.slug}`);
  });

  app.post("/agents/:slug/scopes/:folderId/primary", (req, res) => {
    const agent = requireLiveAgent(req, res);
    if (!agent) return;
    const { session, store } = ctx(res);
    const folderId = String(req.params.folderId);
    if (attempt(session, "scope.primary", () => store.setPrimary(agent.slug, folderId))) {
      return done(req, res, "scope.primary", [agent.slug, folderId], `/agents/${agent.slug}`);
    }
    res.redirect(303, `/agents/${agent.slug}`);
  });

  app.get("/agents/:slug/browse", async (req, res) => {
    const agent = requireAgent(req, res);
    if (!agent) return;
    const parent = typeof req.query.parent === "string" ? req.query.parent : "root";
    // It goes into a Drive query; nothing but an ID shape gets there.
    if (parent !== "root" && !ID_RE.test(parent)) return page(res, 400, errorPage(400, "Not a folder ID."));
    const drive = deps.drive();
    if (!drive) return page(res, 503, errorPage(503, "Drive is not connected yet."));
    try {
      const listing = await drive.listFiles({
        q: `'${parent}' in parents and mimeType='${FOLDER_MIME}' and trashed=false`,
        fields: "files(id,name)",
        pageSize: 200,
        orderBy: "name",
      });
      const folders = listing.files.map((f) => ({ id: String(f.id), name: String(f.name ?? "") }));
      const more = Boolean(listing.nextPageToken);
      page(res, 200, browsePage({ slug: agent.slug, parent, folders, more, csrf: ctx(res).session.csrf }));
    } catch (error) {
      page(res, 502, errorPage(502, `Drive did not answer: ${errorMessage(error)}`));
    }
  });

  app.post("/agents/:slug/tokens", (req, res) => {
    const agent = requireLiveAgent(req, res);
    if (!agent) return;
    const { session, store } = ctx(res);
    const token = mintToken();
    const label = typeof req.body?.label === "string" ? req.body.label : "";
    // Stored before it is ever shown: a crash in between loses nothing usable.
    const id = store.insertToken(agent.slug, token.hash, token.prefix, label);
    session.pending.set(id, token.plaintext);
    done(req, res, "token.mint", [agent.slug, token.prefix], `/agents/${agent.slug}`);
  });

  app.post("/tokens/:id/revoke", (req, res) => {
    const { store } = ctx(res);
    const id = Number(req.params.id);
    const token = Number.isInteger(id) ? store.getToken(id) : null;
    if (!token) return page(res, 404, errorPage(404, "No such token."));
    store.revokeToken(id);
    done(req, res, "token.revoke", [token.agent, token.prefix], `/agents/${token.agent}`, token.agent);
  });

  app.get("/audit", (req, res) => {
    const { session, store } = ctx(res);
    const agent = typeof req.query.agent === "string" && req.query.agent ? req.query.agent : undefined;
    const decision =
      typeof req.query.decision === "string" && AUDIT_DECISIONS.includes(req.query.decision)
        ? req.query.decision
        : undefined;
    const rows = store.queryAudit({ agent, decision, limit: 200 });
    const agents = store.listAgents(true).map((a) => a.slug);
    page(res, 200, auditPage({ rows, agent, decision, agents, csrf: session.csrf }));
  });

  app.post("/cache/flush", (req, res) => {
    deps.resolver.flush();
    deps.subtree.flush();
    done(req, res, "cache.flush", [], "/");
  });

  app.use((_req, res) => {
    page(res, 404, errorPage(404, "Not found."));
  });

  // Anything a route did not handle — a SQLite error, a Drive failure outside
  // a try. Logged here, and answered with the dashboard's own escaped page
  // rather than Express's default, which would print a stack trace.
  app.use((error: unknown, _req: express.Request, res: express.Response, _next: express.NextFunction) => {
    console.error(`[admin] unhandled error: ${errorMessage(error)}`);
    if (res.headersSent) return;
    page(res, 500, errorPage(500, "Something went wrong. The error is in the service log."));
  });

  return app;
}
