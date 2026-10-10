/**
 * The policy database: agents, their grants, folder scopes and tokens, and the
 * audit log. Every SQL statement in the service lives in this file.
 *
 * node:sqlite rather than better-sqlite3: it ships with Node, so there is no
 * native addon to build under `npm ci --ignore-scripts` on Alpine.
 */

import { mkdirSync, statSync } from "node:fs";
import path from "node:path";
import { DatabaseSync, backup } from "node:sqlite";

import { MIGRATIONS } from "./schema.js";

export type Area = "docs" | "sheets" | "drive" | "calendar";
export type Op = "read" | "write" | "create";
export type Grant = { area: Area; op: Op };
export type Scope = { folderId: string; folderName: string; isPrimary: boolean };
export type AgentRow = {
  slug: string;
  name: string;
  scoped: boolean;
  createdAt: number;
  archivedAt: number | null;
};
export type TokenRow = {
  id: number;
  agent: string;
  prefix: string;
  label: string;
  createdAt: number;
  revokedAt: number | null;
};
export type AuditRow = {
  id: number;
  ts: number;
  kind: "tool" | "auth" | "admin";
  agent: string | null;
  tool: string | null;
  decision: "allow" | "deny" | "mutate" | "error";
  reason: string;
  resources: string[];
  source: string;
};

export const SLUG_RE = /^[a-z0-9][a-z0-9-]{1,31}$/;
export const DEFAULT_AGENT = "default";
const MAX_RESOURCES = 50;

export class StoreError extends Error {
  constructor(
    public code:
      | "invalid_slug"
      | "invalid_name"
      | "exists"
      | "reserved"
      | "not_found"
      | "has_scopes",
  ) {
    super(code);
  }
}

export class OpenError extends Error {
  constructor(
    public existed: boolean,
    cause: unknown,
  ) {
    super(cause instanceof Error ? cause.message : String(cause));
  }
}

/**
 * Whether a database is already at `file`. Decides what a failed open means:
 * a database that existed may hold restrictions, so failing to read it must
 * stop the service rather than quietly run without them.
 *
 * Anything other than "no such file" counts as existing — a permissions error
 * on /state hides a real database just as well as it hides an absent one, and
 * the safe reading is the strict one. A 0-byte file with no WAL is what a
 * create that died on a full disk leaves behind; it holds nothing.
 */
export function databaseExists(file: string): boolean {
  const size = (p: string): number | null => {
    try {
      return statSync(p).size;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return null;
      return Number.POSITIVE_INFINITY;
    }
  };
  const main = size(file);
  const wal = size(`${file}-wal`);
  if (wal !== null && wal > 0) return true;
  return main !== null && main > 0;
}

export function openPolicyStore(file: string): PolicyStore {
  const existed = databaseExists(file);
  try {
    return new PolicyStore(new DatabaseSync(file));
  } catch (error) {
    throw new OpenError(existed, error);
  }
}

type Row = Record<string, unknown>;

export class PolicyStore {
  readonly schemaVersion: number;

  constructor(
    readonly db: DatabaseSync,
    private readonly migrations: string[] = MIGRATIONS,
    private readonly now: () => number = Date.now,
  ) {
    db.exec("PRAGMA journal_mode = WAL");
    db.exec("PRAGMA foreign_keys = ON");
    db.exec("PRAGMA busy_timeout = 5000");
    this.migrate();
    this.schemaVersion = this.userVersion();
  }

  private userVersion(): number {
    return Number((this.db.prepare("PRAGMA user_version").get() as Row).user_version);
  }

  /** Runs `fn` in one transaction; rolls back and rethrows on failure. */
  private tx<T>(fn: () => T): T {
    this.db.exec("BEGIN IMMEDIATE");
    try {
      const result = fn();
      this.db.exec("COMMIT");
      return result;
    } catch (error) {
      this.db.exec("ROLLBACK");
      throw error;
    }
  }

  migrate(): void {
    const current = this.userVersion();
    if (current > this.migrations.length) {
      // A newer image migrated this database and we are its rollback. Additive
      // migrations mean everything this version reads is still there.
      console.warn(
        `[db] schema version ${current} is newer than this build knows (${this.migrations.length}) — running without migrating`,
      );
      return;
    }
    for (let version = current; version < this.migrations.length; version++) {
      this.tx(() => {
        this.db.exec(this.migrations[version]!);
        // PRAGMA cannot take a bound parameter; version is our own integer.
        this.db.exec(`PRAGMA user_version = ${version + 1}`);
      });
    }
  }

  /**
   * Creates the `default` agent — the one the legacy bearer maps to — with the
   * given grants, only if it has never existed. Never re-seeds: restarts must
   * not undo what the dashboard changed.
   */
  ensureDefaultAgent(seed: Grant[]): boolean {
    return this.tx(() => {
      const inserted = this.db
        .prepare(
          "INSERT OR IGNORE INTO agents (slug, name, scoped, created_at) VALUES (?, ?, 0, ?)",
        )
        .run(DEFAULT_AGENT, "Default (legacy bearer)", this.now());
      if (Number(inserted.changes) === 0) return false;
      const grant = this.db.prepare("INSERT INTO grants (agent, area, op) VALUES (?, ?, ?)");
      for (const g of seed) grant.run(DEFAULT_AGENT, g.area, g.op);
      return true;
    });
  }

  private agentFrom(row: Row): AgentRow {
    return {
      slug: String(row.slug),
      name: String(row.name),
      scoped: Number(row.scoped) === 1,
      createdAt: Number(row.created_at),
      archivedAt: row.archived_at === null ? null : Number(row.archived_at),
    };
  }

  listAgents(includeArchived = false): AgentRow[] {
    const sql = includeArchived
      ? "SELECT * FROM agents ORDER BY created_at, slug"
      : "SELECT * FROM agents WHERE archived_at IS NULL ORDER BY created_at, slug";
    return (this.db.prepare(sql).all() as Row[]).map((r) => this.agentFrom(r));
  }

  getAgent(slug: string): AgentRow | null {
    const row = this.db.prepare("SELECT * FROM agents WHERE slug = ?").get(slug) as
      | Row
      | undefined;
    return row ? this.agentFrom(row) : null;
  }

  private requireAgent(slug: string): AgentRow {
    const agent = this.getAgent(slug);
    if (!agent) throw new StoreError("not_found");
    return agent;
  }

  /** New agents are confined: no Drive access until a folder is assigned. */
  createAgent(slug: string, name: string): void {
    if (!SLUG_RE.test(slug)) throw new StoreError("invalid_slug");
    const trimmed = name.trim();
    if (!trimmed || trimmed.length > 80) throw new StoreError("invalid_name");
    // Archived slugs stay reserved: a new agent must never inherit an old
    // one's tokens, grants or scopes through a reused key.
    if (this.getAgent(slug)) throw new StoreError("exists");
    this.db
      .prepare("INSERT INTO agents (slug, name, scoped, created_at) VALUES (?, ?, 1, ?)")
      .run(slug, trimmed, this.now());
  }

  archiveAgent(slug: string): void {
    if (slug === DEFAULT_AGENT) throw new StoreError("reserved");
    this.requireAgent(slug);
    const at = this.now();
    this.tx(() => {
      this.db.prepare("UPDATE agents SET archived_at = ? WHERE slug = ?").run(at, slug);
      this.db
        .prepare("UPDATE tokens SET revoked_at = ? WHERE agent = ? AND revoked_at IS NULL")
        .run(at, slug);
    });
  }

  /** Lifts confinement. Only with no folders left, so it is never accidental. */
  setUnconfined(slug: string): void {
    this.requireAgent(slug);
    if (this.getScopes(slug).length) throw new StoreError("has_scopes");
    this.db.prepare("UPDATE agents SET scoped = 0 WHERE slug = ?").run(slug);
  }

  getGrants(slug: string): Grant[] {
    return (
      this.db
        .prepare("SELECT area, op FROM grants WHERE agent = ? ORDER BY area, op")
        .all(slug) as Row[]
    ).map((r) => ({ area: r.area as Area, op: r.op as Op }));
  }

  setGrants(slug: string, grants: Grant[]): void {
    this.requireAgent(slug);
    this.tx(() => {
      this.db.prepare("DELETE FROM grants WHERE agent = ?").run(slug);
      const insert = this.db.prepare(
        "INSERT OR IGNORE INTO grants (agent, area, op) VALUES (?, ?, ?)",
      );
      for (const g of grants) insert.run(slug, g.area, g.op);
    });
  }

  getScopes(slug: string): Scope[] {
    return (
      this.db
        .prepare(
          "SELECT folder_id, folder_name, is_primary FROM folder_scopes WHERE agent = ? ORDER BY is_primary DESC, created_at, folder_id",
        )
        .all(slug) as Row[]
    ).map((r) => ({
      folderId: String(r.folder_id),
      folderName: String(r.folder_name),
      isPrimary: Number(r.is_primary) === 1,
    }));
  }

  /**
   * Adds a folder to an agent's scope and confines the agent. The first folder
   * is the primary — where creates land — whatever was asked.
   */
  addScope(slug: string, folderId: string, folderName: string, primary: boolean): void {
    // `default` is the legacy connection's identity; confining it would change
    // what Open WebUI sees on a connection nobody reconfigured.
    if (slug === DEFAULT_AGENT) throw new StoreError("reserved");
    this.requireAgent(slug);
    this.tx(() => {
      const exists = this.db
        .prepare("SELECT 1 FROM folder_scopes WHERE agent = ? AND folder_id = ?")
        .get(slug, folderId);
      if (exists) throw new StoreError("exists");
      const first = this.getScopes(slug).length === 0;
      const makePrimary = primary || first;
      if (makePrimary) {
        this.db.prepare("UPDATE folder_scopes SET is_primary = 0 WHERE agent = ?").run(slug);
      }
      this.db
        .prepare(
          "INSERT INTO folder_scopes (agent, folder_id, folder_name, is_primary, created_at) VALUES (?, ?, ?, ?, ?)",
        )
        .run(slug, folderId, folderName, makePrimary ? 1 : 0, this.now());
      this.db.prepare("UPDATE agents SET scoped = 1 WHERE slug = ?").run(slug);
    });
  }

  /** Removing the last folder leaves the agent confined, with no Drive access. */
  removeScope(slug: string, folderId: string): void {
    this.db
      .prepare("DELETE FROM folder_scopes WHERE agent = ? AND folder_id = ?")
      .run(slug, folderId);
  }

  setPrimary(slug: string, folderId: string): void {
    this.tx(() => {
      const exists = this.db
        .prepare("SELECT 1 FROM folder_scopes WHERE agent = ? AND folder_id = ?")
        .get(slug, folderId);
      if (!exists) throw new StoreError("not_found");
      this.db.prepare("UPDATE folder_scopes SET is_primary = 0 WHERE agent = ?").run(slug);
      this.db
        .prepare("UPDATE folder_scopes SET is_primary = 1 WHERE agent = ? AND folder_id = ?")
        .run(slug, folderId);
    });
  }

  insertToken(slug: string, hash: string, prefix: string, label: string): number {
    this.requireAgent(slug);
    const result = this.db
      .prepare(
        "INSERT INTO tokens (agent, token_hash, prefix, label, created_at) VALUES (?, ?, ?, ?, ?)",
      )
      .run(slug, hash, prefix, label.trim().slice(0, 80), this.now());
    return Number(result.lastInsertRowid);
  }

  revokeToken(id: number): void {
    this.db
      .prepare("UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL")
      .run(this.now(), id);
  }

  getToken(id: number): TokenRow | null {
    const row = this.db.prepare("SELECT * FROM tokens WHERE id = ?").get(id) as
      | Row
      | undefined;
    return row ? this.tokenFrom(row) : null;
  }

  private tokenFrom(r: Row): TokenRow {
    return {
      id: Number(r.id),
      agent: String(r.agent),
      prefix: String(r.prefix),
      label: String(r.label),
      createdAt: Number(r.created_at),
      revokedAt: r.revoked_at === null ? null : Number(r.revoked_at),
    };
  }

  listTokens(slug: string): TokenRow[] {
    return (
      this.db
        .prepare("SELECT * FROM tokens WHERE agent = ? ORDER BY created_at DESC, id DESC")
        .all(slug) as Row[]
    ).map((r) => this.tokenFrom(r));
  }

  agentForTokenHash(hash: string): string | null {
    const row = this.db
      .prepare(
        "SELECT t.agent FROM tokens t JOIN agents a ON a.slug = t.agent WHERE t.token_hash = ? AND t.revoked_at IS NULL AND a.archived_at IS NULL",
      )
      .get(hash) as Row | undefined;
    return row ? String(row.agent) : null;
  }

  insertAudit(row: Omit<AuditRow, "id">): void {
    this.db
      .prepare(
        "INSERT INTO audit_log (ts, kind, agent, tool, decision, reason, resources, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
      )
      .run(
        row.ts,
        row.kind,
        row.agent,
        row.tool,
        row.decision,
        row.reason,
        JSON.stringify(row.resources.slice(0, MAX_RESOURCES)),
        row.source,
      );
  }

  queryAudit(f: { agent?: string; decision?: string; limit: number }): AuditRow[] {
    const where: string[] = [];
    const params: string[] = [];
    if (f.agent) {
      where.push("agent = ?");
      params.push(f.agent);
    }
    if (f.decision) {
      where.push("decision = ?");
      params.push(f.decision);
    }
    const sql = `SELECT * FROM audit_log ${where.length ? `WHERE ${where.join(" AND ")}` : ""} ORDER BY ts DESC, id DESC LIMIT ${Math.max(1, Math.min(1000, Math.floor(f.limit)))}`;
    return (this.db.prepare(sql).all(...params) as Row[]).map((r) => ({
      id: Number(r.id),
      ts: Number(r.ts),
      kind: r.kind as AuditRow["kind"],
      agent: r.agent === null ? null : String(r.agent),
      tool: r.tool === null ? null : String(r.tool),
      decision: r.decision as AuditRow["decision"],
      reason: String(r.reason),
      resources: parseResources(String(r.resources)),
      source: String(r.source),
    }));
  }

  pruneAudit(beforeTs: number): number {
    return Number(this.db.prepare("DELETE FROM audit_log WHERE ts < ?").run(beforeTs).changes);
  }

  close(): void {
    this.db.close();
  }
}

function parseResources(text: string): string[] {
  try {
    const value = JSON.parse(text) as unknown;
    return Array.isArray(value) ? value.map(String) : [];
  } catch {
    return [];
  }
}

/**
 * One backup per weekday, overwritten a week later. On the same volume as the
 * database: it protects against corruption and bad edits, not against losing
 * the volume.
 */
export async function backupDaily(
  store: PolicyStore,
  dir: string,
  now: Date = new Date(),
): Promise<string> {
  mkdirSync(dir, { recursive: true });
  const target = path.join(dir, `policy-${now.getUTCDay()}.sqlite`);
  await backup(store.db, target);
  return target;
}
