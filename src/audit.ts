/**
 * The audit trail: one row per tool call, per refused credential and per
 * dashboard change. Resource IDs only — never argument values or result text,
 * which are document contents.
 *
 * Fail-open by design: a full disk must not turn into "every tool call fails".
 * A write that fails is logged and counted, and /healthz reports the count.
 */

import { errorMessage } from "./errors.js";
import type { AuditRow, PolicyStore } from "./policy/store.js";

export const DAY_MS = 24 * 60 * 60 * 1000;
const AUTH_DEDUPE_MS = 60_000;

export class Audit {
  errors = 0;
  private lastAuthFailure = new Map<string, number>();

  constructor(
    private readonly store: PolicyStore,
    private readonly now: () => number = Date.now,
  ) {}

  private write(row: Omit<AuditRow, "id" | "ts">): void {
    try {
      this.store.insertAudit({ ...row, ts: this.now() });
    } catch (error) {
      this.errors++;
      console.error(
        `[audit] could not write a row (${this.errors} so far): ${errorMessage(error)}`,
      );
    }
  }

  tool(
    ctx: { slug: string },
    tool: string,
    decision: AuditRow["decision"],
    reason: string,
    resources: string[],
  ): void {
    this.write({ kind: "tool", agent: ctx.slug, tool, decision, reason, resources, source: "" });
  }

  /**
   * At most one row per source and reason per minute: a misconfigured client
   * retrying a dead token would otherwise write a row per request forever.
   */
  authFailure(source: string, reason = "bad_bearer"): void {
    const key = `${source}\u0000${reason}`;
    const at = this.now();
    const last = this.lastAuthFailure.get(key);
    if (last !== undefined && at - last < AUTH_DEDUPE_MS) return;
    if (this.lastAuthFailure.size > 1000) this.lastAuthFailure.clear();
    this.lastAuthFailure.set(key, at);
    this.write({ kind: "auth", agent: null, tool: null, decision: "deny", reason, resources: [], source });
  }

  /** A dashboard change, filed under the agent it touched so the agent filter finds it. */
  admin(action: string, agent: string | null, resources: string[], source: string): void {
    this.write({ kind: "admin", agent, tool: null, decision: "mutate", reason: action, resources, source });
  }

  /**
   * A refused dashboard request: a failed login, a lockout, a missing CSRF
   * token. Never deduplicated — the lockout already caps how many there can
   * be, and a password-guessing burst is exactly what this log is for.
   */
  adminDeny(reason: string, source: string): void {
    this.write({ kind: "auth", agent: null, tool: null, decision: "deny", reason, resources: [], source });
  }

  /** The number of rows removed; 0 when the prune failed (counted in `errors`). */
  prune(retentionDays = 90): number {
    try {
      const removed = this.store.pruneAudit(this.now() - retentionDays * DAY_MS);
      if (removed) console.log(`[audit] pruned ${removed} rows older than ${retentionDays} days`);
      return removed;
    } catch (error) {
      this.errors++;
      console.error(
        `[audit] prune failed: ${errorMessage(error)}`,
      );
      return 0;
    }
  }
}
