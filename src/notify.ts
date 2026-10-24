/**
 * Broken-only pushes to one shared ntfy topic. The topic is the credential on
 * public ntfy.sh: it is read from the vault, never logged, and every push body
 * is a fixed template built by `GoogleHealth`.
 */

import type { BrokenKind, Notice } from "./drive/health.js";

/** True when ntfy answered 2xx. Never rejects. */
export type Notifier = (notice: Notice) => Promise<boolean>;

const TOPIC = /^[A-Za-z0-9_-]{8,64}$/;
const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);

/** HTTP header values are Latin-1 at best; keep the title plain ASCII. */
const asciiTitle = (title: string) => title.replace(/[^\x20-\x7e]/g, "?");

export function notifierFrom(
  rawTopic: string,
  base: string,
  fetchImpl: typeof fetch = fetch,
  timeoutMs = 10_000,
): Notifier | null {
  const topic = rawTopic.trim();
  if (!topic) {
    console.warn("[alert] ntfy not configured — alerts are log-only");
    return null;
  }
  if (!TOPIC.test(topic)) {
    console.warn("[alert] ntfy disabled: invalid topic");
    return null;
  }
  const url = URL.canParse(base) ? new URL(base) : null;
  if (!url || (url.protocol !== "https:" && !(url.protocol === "http:" && LOOPBACK.has(url.hostname)))) {
    console.warn("[alert] ntfy disabled: invalid base");
    return null;
  }
  const target = `${base.replace(/\/+$/, "")}/${topic}`;
  return async (notice) => {
    try {
      const res = await fetchImpl(target, {
        method: "POST",
        headers: {
          Title: asciiTitle(notice.title),
          Priority: "high",
          Tags: "warning",
          "Content-Type": "text/plain; charset=utf-8",
        },
        body: notice.message,
        signal: AbortSignal.timeout(timeoutMs),
      });
      void res.body?.cancel().catch(() => undefined);
      if (!res.ok) {
        console.warn(`[alert] ntfy push failed: HTTP ${res.status}`);
        return false;
      }
      return true;
    } catch (error) {
      // The name only: the message can carry the URL, and the URL carries the topic.
      console.warn(`[alert] ntfy push failed: ${error instanceof Error ? error.name : "error"}`);
      return false;
    }
  };
}

/**
 * At most one delivered push per kind per `gapMs`, one send in flight per kind,
 * and never more than one attempt per kind per minute. An undelivered push does
 * not start the gap, so the next event of that kind retries it. No timers.
 */
export function makeBroadcaster(
  notifier: Notifier | null,
  opts: { now?: () => number; gapMs?: number } = {},
): (kind: BrokenKind, notice: Notice) => void {
  const now = opts.now ?? Date.now;
  const gapMs = opts.gapMs ?? 1_800_000;
  const lastDelivered = new Map<BrokenKind, number>();
  const lastAttempt = new Map<BrokenKind, number>();
  const inFlight = new Set<BrokenKind>();
  return (kind, notice) => {
    if (!notifier || inFlight.has(kind)) return;
    if (now() - (lastDelivered.get(kind) ?? -Infinity) < gapMs) return;
    if (now() - (lastAttempt.get(kind) ?? -Infinity) < 60_000) return;
    inFlight.add(kind);
    lastAttempt.set(kind, now());
    let sent: Promise<boolean>;
    try {
      sent = notifier(notice);
    } catch {
      sent = Promise.resolve(false);
    }
    void sent
      .catch(() => false)
      .then((delivered) => {
        inFlight.delete(kind);
        if (delivered) lastDelivered.set(kind, now());
      });
  };
}
