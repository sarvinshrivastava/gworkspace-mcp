/**
 * Configuration, and the vault fetch that supplies this service's bearer.
 *
 * Same shape as bridge-mcp deliberately: one pattern for every service on this
 * box, so there is one thing to learn and one place each failure mode lives.
 */

const env = (key: string, fallback?: string): string => {
  const value = process.env[key];
  if (value === undefined || value === "") {
    if (fallback !== undefined) return fallback;
    throw new Error(`FATAL: ${key} is not set`);
  }
  return value;
};

const envList = (key: string): string[] =>
  (process.env[key] ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);

const oneOf = <T extends string>(key: string, allowed: readonly T[], fallback: T): T => {
  const value = env(key, fallback);
  if (!(allowed as readonly string[]).includes(value))
    throw new Error(`FATAL: ${key} must be one of ${allowed.join(", ")}, got "${value}"`);
  return value as T;
};

export const config = {
  port: Number(env("PORT", "3014")),

  /** The admin dashboard's own listener. Never routed /mcp. */
  adminPort: Number(env("ADMIN_PORT", "3015")),

  /** The policy database, on a named volume. Holds hashes only, no secrets. */
  dbPath: env("DB_PATH", "/state/gworkspace-v2.sqlite"),
  backupDir: env("BACKUP_DIR", "/state/backups"),

  /**
   * Who schedules the daily audit prune and DB backup after the boot run:
   * `internal` (a 24 h interval here) or `external` (pipeline-runner calls
   * POST /internal/housekeeping).
   */
  housekeepingScheduler: oneOf("HOUSEKEEPING_SCHEDULER", ["internal", "external"] as const, "internal"),

  /** Extra exact source addresses allowed on the admin port, comma-separated. */
  adminAllowedSources: envList("ADMIN_ALLOWED_SOURCES"),

  /** Extra Host header names the admin port answers to (default: loopback only). */
  adminAllowedHosts: envList("ADMIN_ALLOWED_HOSTS"),

  /** The pinned upstream server's entry point, inside this image. */
  upstreamEntry: env(
    "UPSTREAM_ENTRY",
    "/app/node_modules/@a-bonus/google-docs-mcp/dist/index.js",
  ),

  /**
   * Where the child keeps its OAuth token store. A tmpfs, NOT a volume: the
   * vault is rewritten over this path on every boot and the upstream only ever
   * persists a token in its interactive consent path, so a volume held a second
   * permanent plaintext copy of the refresh token on host disk and bought
   * nothing.
   */
  configHome: env("CONFIG_HOME", "/data/config"),
  tokenDir: env("TOKEN_DIR", "/data/config/google-docs-mcp"),

  /** Where broken-only pushes go. The topic itself comes from the vault. */
  ntfyBase: env("NTFY_BASE", "https://ntfy.sh"),

  /** The child's own V8 heap budget. Two processes share one cgroup. */
  childHeapMb: Number(env("CHILD_HEAP_MB", "192")),

  /**
   * Expose only these upstream tools. The upstream ships ~120 registrations and
   * that is a lot of schema for every chat turn to carry — Open WebUI re-lists
   * tools per message, and the full set measured 126 KB, ~31k tokens, resent on
   * every LLM call of every turn. Empty exposes NOTHING; see selectTools.
   */
  toolAllowlist: envList("TOOL_ALLOWLIST"),

  vault: {
    url: env("SECRETS_MANAGER_URL", "http://secret-manager:8000"),
    folder: env("SECRETS_MANAGER_FOLDER", "Root"),
    token: process.env.SECRETS_MANAGER_TOKEN ?? "",
  },

  limits: {
    // Per vault request, not per boot: four keys are fetched concurrently.
    httpTimeoutMs: 15_000,
  },
};

/** Extracts the value from a vault response. */
export function parseVaultBody(text: string, label: string): string {
  // Deliberately a plain parse. The bash sibling slurps concatenated documents
  // because curl cannot rewind a pipe on --retry and glues a failed body in
  // front of the good one. `fetch` has no such failure mode: every attempt is a
  // fresh response and text() reads exactly one body. The hand-rolled scanner
  // that used to live here also swallowed malformed JSON, which is worse than
  // failing on it.
  let parsed: { value?: unknown };
  try {
    parsed = JSON.parse(text) as { value?: unknown };
  } catch {
    throw new Error(`FATAL: vault returned unparseable JSON for ${label}`);
  }

  // An empty string is a failure, not a value: it would configure this service
  // with blank credentials that only fail later, at call time.
  if (typeof parsed.value !== "string" || parsed.value === "") {
    throw new Error(`FATAL: vault returned no usable value for ${label}`);
  }
  return parsed.value;
}

/**
 * Like vaultFetch, but a key that is not in the vault (404) is "" rather than
 * fatal. For settings the service can run without, like the dashboard login.
 */
export function vaultFetchOptional(key: string): Promise<string> {
  return vaultFetch(key, { optional: true });
}

export async function vaultFetch(
  key: string,
  opts: { optional?: boolean } = {},
): Promise<string> {
  if (!config.vault.token)
    throw new Error("FATAL: SECRETS_MANAGER_TOKEN is empty");

  const label = `${config.vault.folder}/${key}`;
  const attempts = 4;

  for (let attempt = 1; attempt <= attempts; attempt++) {
    let res: Response;
    try {
      res = await fetch(
        `${config.vault.url}/api/secrets/${config.vault.folder}/${key}`,
        {
          headers: { Authorization: `Bearer ${config.vault.token}` },
          signal: AbortSignal.timeout(config.limits.httpTimeoutMs),
          redirect: "error",
        },
      );
    } catch (error) {
      const cause =
        error instanceof Error && error.cause instanceof Error
          ? `: ${error.cause.message}`
          : "";
      if (attempt === attempts)
        throw new Error(`FATAL: vault unreachable for ${label}${cause}`);
      await new Promise((r) => setTimeout(r, attempt * 2000));
      continue;
    }

    if (res.status === 404 && opts.optional) {
      console.warn(`[vault] ${label} is not set`);
      return "";
    }
    if (res.status === 401 || res.status === 403 || res.status === 404) {
      throw new Error(`FATAL: vault returned HTTP ${res.status} for ${label}`);
    }
    if (!res.ok) {
      if (attempt === attempts)
        throw new Error(
          `FATAL: vault returned HTTP ${res.status} for ${label}`,
        );
      await new Promise((r) => setTimeout(r, attempt * 2000));
      continue;
    }

    return parseVaultBody(await res.text(), label);
  }

  throw new Error(`FATAL: vault unreachable for ${label}`);
}
