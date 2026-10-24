/**
 * What the gateway knows about Google, from its own Drive calls, the boot
 * probe and the Workspace child's exit. It feeds `/healthz` (a field, never the
 * status code), the `[drive]`/`[alert]` log lines, and the push notices.
 *
 * In-memory: a restart resets it to `unknown` and the boot probe re-seeds it.
 */

import { redactDetail } from "../errors.js";

export type GoogleState = "unknown" | "ok" | "failing" | "rejected";
export type BrokenKind = "rejected" | "failing" | "probe" | "upstream_down";
export type Notice = { title: string; message: string };
export type ProbeResult = { ok: true } | { ok: false; rejected: boolean; detail: string };

type Options = {
  now?: () => number;
  /** Consecutive transient failures before a `failing` alert. */
  threshold?: number;
  /** ... and how long after the first of them. */
  minSpanMs?: number;
  /** One `[drive]` line per reason and category in this window. */
  logWindowMs?: number;
};

const SAFE_CATEGORY = /^[A-Za-z0-9_ .-]{1,40}$/;

const safeCategory = (category: string) => (SAFE_CATEGORY.test(category) ? category : "unknown");

const redact = redactDetail;

export class GoogleHealth {
  private readonly now: () => number;
  private readonly threshold: number;
  private readonly minSpanMs: number;
  private readonly logWindowMs: number;

  private broadcast: (kind: BrokenKind, notice: Notice) => void = () => undefined;
  private state: GoogleState = "unknown";
  private since: number | null = null;
  private failures = 0;
  private firstFailureAt = 0;
  private rejectedAt = -Infinity;
  private rejectedCategory = "unknown";
  private lastTransientCategory = "unknown";
  private probeState: "unknown" | "ok" | "failed" = "unknown";
  private probeAt = 0;
  private downAt: number | null = null;
  private alerted = { failing: false, rejected: false, probe: false, down: false };
  private readonly lastLogged = new Map<string, number>();

  constructor(opts: Options = {}) {
    this.now = opts.now ?? Date.now;
    this.threshold = opts.threshold ?? 5;
    this.minSpanMs = opts.minSpanMs ?? 60_000;
    this.logWindowMs = opts.logWindowMs ?? 60_000;
  }

  setBroadcast(fn: (kind: BrokenKind, notice: Notice) => void): void {
    this.broadcast = fn;
  }

  /** A Drive answer that started at `startedAt`. A stale one never clears a rejection. */
  ok(startedAt: number): void {
    if (this.state === "rejected" && startedAt <= this.rejectedAt) return;
    this.failures = 0;
    if (this.state === "ok") return;
    this.state = "ok";
    this.since = this.now();
    if (this.alerted.failing || this.alerted.rejected) console.error("[recovered] Google Drive answers again");
    this.alerted.failing = false;
    this.alerted.rejected = false;
  }

  failure(kind: "rejected" | "transient", category: string, detail: string): void {
    const now = this.now();
    this.failures++;
    if (kind === "rejected") {
      if (this.state !== "rejected") {
        this.state = "rejected";
        this.since = now;
      }
      this.rejectedAt = now;
      this.rejectedCategory = category;
      if (!this.alerted.rejected) {
        this.alerted.rejected = true;
        console.error(`[alert] Google access rejected (${safeCategory(category)}): ${redact(detail)}`);
      }
      this.send("rejected", this.rejectedNotice());
      return;
    }
    this.lastTransientCategory = category;
    if (this.state === "rejected") return;
    if (this.state !== "failing") {
      this.state = "failing";
      this.since = now;
      this.failures = 1;
      this.firstFailureAt = now;
    }
    if (this.failures >= this.threshold && now - this.firstFailureAt >= this.minSpanMs) {
      if (!this.alerted.failing) {
        this.alerted.failing = true;
        console.error(
          `[alert] Google Drive failing: ${this.failures} consecutive failures (${safeCategory(category)}): ${redact(detail)}`,
        );
      }
      this.send("failing", this.failingNotice());
    }
  }

  /** The boot credential check. A probe that names `invalid_grant` is a rejection, not a probe notice. */
  probe(result: ProbeResult): void {
    this.probeAt = this.now();
    if (result.ok) {
      this.probeState = "ok";
      return;
    }
    this.probeState = "failed";
    if (result.rejected) {
      this.failure("rejected", "invalid_grant", result.detail);
      return;
    }
    if (!this.alerted.probe) {
      this.alerted.probe = true;
      console.error(`[alert] boot probe of the Google credential failed: ${redact(result.detail)}`);
    }
    this.send("probe", {
      title: "gworkspace-mcp: Google credential check failed at boot",
      message: `The boot credential check failed. Workspace tools may fail. Since ${this.iso(this.probeAt)}.`,
    });
  }

  /** The Workspace child exited, or a call found it gone. */
  upstreamDown(): void {
    this.downAt ??= this.now();
    if (!this.alerted.down) {
      this.alerted.down = true;
      console.error("[alert] the Workspace server process is gone; tool calls fail until the service restarts");
    }
    this.send("upstream_down", {
      title: "gworkspace-mcp: Workspace server exited",
      message: `The Google Workspace server process exited. Tool calls fail until the service restarts. Since ${this.iso(this.downAt)}.`,
    });
  }

  /** One line per reason and category per window; the denial itself is never suppressed. */
  logDrive(tool: string, reason: string, category: string, detail?: string): void {
    const now = this.now();
    const key = `${reason} ${category}`;
    const last = this.lastLogged.get(key);
    if (last !== undefined && now - last < this.logWindowMs) return;
    if (this.lastLogged.size > 100) this.lastLogged.clear();
    this.lastLogged.set(key, now);
    console.error(`[drive] ${tool} ${reason}: ${detail === undefined ? "-" : redact(detail)}`);
  }

  snapshot(): {
    state: GoogleState;
    since: string | null;
    consecutiveFailures: number;
    probe: "unknown" | "ok" | "failed";
  } {
    return {
      state: this.state,
      since: this.since === null ? null : this.iso(this.since),
      consecutiveFailures: this.failures,
      probe: this.probeState,
    };
  }

  private iso(ms: number): string {
    return new Date(ms).toISOString();
  }

  private rejectedNotice(): Notice {
    return {
      title: "gworkspace-mcp: Google access rejected",
      message: `Google access rejected (${safeCategory(this.rejectedCategory)}). Workspace tools will fail until the owner re-consents and replaces GOOGLE_TOKEN_JSON, or enables the API. Since ${this.iso(this.since ?? this.now())}.`,
    };
  }

  private failingNotice(): Notice {
    return {
      title: "gworkspace-mcp: Google Drive failing",
      message: `${this.failures} consecutive Drive failures since ${this.iso(this.since ?? this.now())}. Last: ${safeCategory(this.lastTransientCategory)}. Usually transient.`,
    };
  }

  /** A broken sender must never reach a tool call. */
  private send(kind: BrokenKind, notice: Notice): void {
    try {
      this.broadcast(kind, notice);
    } catch {
      // Swallowed on purpose.
    }
  }
}
