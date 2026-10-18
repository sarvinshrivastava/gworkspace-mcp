/**
 * The guard for /internal/* — routes that only pipeline-runner calls, over the
 * `shared` Docker network, with its own bearer (GWORKSPACE_PLR_TOKEN).
 *
 * :3014 is also published for the nginx vhost, so these routes are one proxy
 * hop from the internet. nginx answers /internal with 404 itself; this is the
 * second layer, in this order:
 *   1. the exact lowercase /internal/ prefix, else 404 — Express matches paths
 *      case-insensitively, and nginx's case-insensitive rule is the only thing
 *      that would otherwise stand between /INTERNAL/... and this handler
 *   2. a proxy header (X-Forwarded-For / X-Real-IP), else 404: plr talks to
 *      the container directly, so a proxied request is never plr
 *   3. 10 requests per source per minute, else 429 — before the bearer, so
 *      guessing costs a window per ten tries
 *   4. no token configured → 503; never a match against ""
 *   5. a constant-time bearer compare, else 401 (logged once per source per minute)
 */

import { createHash, timingSafeEqual } from "node:crypto";
import type express from "express";

const WINDOW_MS = 60_000;
const MAX_PER_WINDOW = 10;

const digest = (value: string) => createHash("sha256").update(value).digest();

export function internalGuard(
  token: () => string,
  opts: {
    now?: () => number;
    /** Each rejected bearer, for the audit trail (which dedupes per source itself). */
    onReject?: (source: string) => void;
  } = {},
): express.RequestHandler {
  const now = opts.now ?? Date.now;
  // Fixed window per source, as secrets-manager's rate-limit.js. Swept on use
  // rather than on a timer: this route sees a few calls a day.
  const buckets = new Map<string, { start: number; count: number }>();
  const lastLogged = new Map<string, number>();

  const sweep = (at: number) => {
    for (const [source, bucket] of buckets) if (at - bucket.start >= WINDOW_MS) buckets.delete(source);
    for (const [source, logged] of lastLogged) if (at - logged >= WINDOW_MS) lastLogged.delete(source);
  };

  return (req, res, next) => {
    const proxied = req.headers["x-forwarded-for"] !== undefined || req.headers["x-real-ip"] !== undefined;
    if (!req.originalUrl.startsWith("/internal/") || proxied) {
      res.status(404).end();
      return;
    }

    const source = req.socket.remoteAddress ?? "";
    const at = now();
    sweep(at);
    const bucket = buckets.get(source);
    if (!bucket) {
      buckets.set(source, { start: at, count: 1 });
    } else if (bucket.count >= MAX_PER_WINDOW) {
      res.set("Retry-After", String(Math.ceil((bucket.start + WINDOW_MS - at) / 1000)));
      res.status(429).json({ error: "rate_limited" });
      return;
    } else {
      bucket.count++;
    }

    const expected = token();
    if (!expected) {
      res.status(503).json({ error: "not_configured" });
      return;
    }
    const provided = (req.headers.authorization ?? "").replace(/^Bearer\s+/i, "");
    if (!timingSafeEqual(digest(provided), digest(expected))) {
      if (!lastLogged.has(source)) {
        lastLogged.set(source, at);
        console.warn(`[internal] rejected ${req.method} ${req.baseUrl}${req.path} from ${source || "unknown"}`);
      }
      opts.onReject?.(source);
      res.status(401).json({ error: "unauthorized" });
      return;
    }
    next();
  };
}
