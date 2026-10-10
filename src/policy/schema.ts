/**
 * Forward-only migrations, applied in order and recorded in PRAGMA user_version.
 *
 * Additive only. The previous image must keep working against a database this
 * one has migrated (a rollback), so a migration may add tables, columns and
 * indexes but never drop or rename one the previous image reads.
 */
export const MIGRATIONS: string[] = [
  /* v1 */ `
CREATE TABLE agents (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  scoped INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  archived_at INTEGER
);
CREATE TABLE grants (
  agent TEXT NOT NULL REFERENCES agents(slug),
  area TEXT NOT NULL CHECK (area IN ('docs', 'sheets', 'drive', 'calendar')),
  op TEXT NOT NULL CHECK (op IN ('read', 'write', 'create')),
  PRIMARY KEY (agent, area, op)
);
CREATE TABLE tokens (
  id INTEGER PRIMARY KEY,
  agent TEXT NOT NULL REFERENCES agents(slug),
  token_hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE TABLE folder_scopes (
  agent TEXT NOT NULL REFERENCES agents(slug),
  folder_id TEXT NOT NULL,
  folder_name TEXT NOT NULL DEFAULT '',
  is_primary INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (agent, folder_id)
);
CREATE UNIQUE INDEX folder_scopes_one_primary ON folder_scopes(agent) WHERE is_primary = 1;
CREATE TABLE audit_log (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('tool', 'auth', 'admin')),
  agent TEXT,
  tool TEXT,
  decision TEXT NOT NULL CHECK (decision IN ('allow', 'deny', 'mutate', 'error')),
  reason TEXT NOT NULL DEFAULT '',
  resources TEXT NOT NULL DEFAULT '[]',
  source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_ts ON audit_log(ts);
CREATE INDEX audit_log_agent_ts ON audit_log(agent, ts);
`,
];
