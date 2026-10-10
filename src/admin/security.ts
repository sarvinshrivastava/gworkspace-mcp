/**
 * The dashboard's gates: who may connect, the password, the lockout, sessions.
 *
 * Source addresses come from the socket, never from X-Forwarded-For: the
 * admin port is reachable from every container on the shared network, and a
 * header is something any of them can set.
 */

import crypto from "node:crypto";
import { readFileSync } from "node:fs";
import os from "node:os";

// --- who may connect -------------------------------------------------------

export type NetInfo = {
  interfaces: () => NodeJS.Dict<os.NetworkInterfaceInfo[]>;
  /** /proc/net/route, or null where there is none (macOS, tests). */
  routeText: () => string | null;
};

const defaultNet: NetInfo = {
  interfaces: () => os.networkInterfaces(),
  routeText: () => {
    try {
      return readFileSync("/proc/net/route", "utf8");
    } catch {
      return null;
    }
  },
};

/** Gateways from /proc/net/route: little-endian hex in the third column. */
export function gatewaysFromRoute(text: string): string[] {
  const out = new Set<string>();
  for (const line of text.split("\n").slice(1)) {
    const hex = line.trim().split(/\s+/)[2];
    if (!hex || !/^[0-9A-Fa-f]{8}$/.test(hex) || /^0+$/.test(hex)) continue;
    const bytes = (hex.match(/../g) ?? []).map((b) => parseInt(b, 16)).reverse();
    out.add(bytes.join("."));
  }
  return [...out];
}

const v4ToInt = (ip: string) =>
  ip.split(".").reduce((acc, octet) => ((acc << 8) | Number(octet)) >>> 0, 0);
const intToV4 = (n: number) =>
  [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join(".");

/**
 * The first host of each attached IPv4 network — Docker's default bridge
 * gateway, which is where host traffic (docker-proxy, the SSH tunnel) arrives
 * from. /proc/net/route only names the default route's gateway; a container
 * on two networks has two.
 */
export function firstHosts(ifaces: NodeJS.Dict<os.NetworkInterfaceInfo[]>): string[] {
  const out = new Set<string>();
  for (const list of Object.values(ifaces)) {
    for (const info of list ?? []) {
      if (info.family !== "IPv4" || info.internal) continue;
      const network = v4ToInt(info.address) & v4ToInt(info.netmask);
      out.add(intToV4((network + 1) >>> 0));
    }
  }
  return [...out];
}

/** Recomputed every 60 s, so a network attached after boot is picked up. */
export function makeAllowedSources(
  extra: string[],
  net: NetInfo = defaultNet,
  now: () => number = Date.now,
): () => Set<string> {
  let cached: { at: number; set: Set<string> } | null = null;
  return () => {
    if (cached && now() - cached.at < 60_000) return cached.set;
    // Not loopback: inside the container that is the upstream child, which
    // parses attacker-written documents and has no business on this port. Host
    // traffic (the tunnel, docker-proxy) arrives from a bridge gateway.
    const base = [...gatewaysFromRoute(net.routeText() ?? ""), ...firstHosts(net.interfaces()), ...extra];
    const set = new Set<string>();
    for (const ip of base) {
      set.add(ip);
      if (/^\d+\.\d+\.\d+\.\d+$/.test(ip)) set.add(`::ffff:${ip}`);
    }
    cached = { at: now(), set };
    return set;
  };
}

export function isAllowed(address: string | undefined, allowed: Set<string>): boolean {
  return Boolean(address) && allowed.has(address!);
}

/**
 * The Host header must name loopback (or an explicitly configured name).
 *
 * The socket allowlist cannot tell the admin's browser from a DNS-rebinding
 * page open in that same browser while the tunnel is up — both arrive through
 * the tunnel. The session cookie stops such a page acting as the admin, but
 * not from spending the shared login lockout. Its Host header is the attacker's
 * domain, so checking it closes that.
 */
export function isAllowedHost(hostHeader: string | undefined, extra: string[] = []): boolean {
  if (!hostHeader) return false;
  const host = hostHeader.toLowerCase().replace(/:\d+$/, "");
  return ["localhost", "127.0.0.1", "[::1]", ...extra.map((h) => h.toLowerCase())].includes(host);
}

// --- the password ----------------------------------------------------------

const SCRYPT = { N: 16384, r: 8, p: 1, keylen: 32 } as const;
const SCRYPT_MAXMEM = 64 * 1024 * 1024;

/** scrypt$N$r$p$<salt b64>$<key b64> — what `npm run hash-password` prints. */
export function hashPassword(password: string): string {
  const salt = crypto.randomBytes(16);
  const key = crypto.scryptSync(password, salt, SCRYPT.keylen, { N: SCRYPT.N, r: SCRYPT.r, p: SCRYPT.p });
  return `scrypt$${SCRYPT.N}$${SCRYPT.r}$${SCRYPT.p}$${salt.toString("base64")}$${key.toString("base64")}`;
}

export const HASH_RE = /^scrypt\$(\d+)\$(\d+)\$(\d+)\$([A-Za-z0-9+/=]+)\$([A-Za-z0-9+/=]+)$/;

export async function verifyPassword(password: string, encoded: string): Promise<boolean> {
  const m = HASH_RE.exec(encoded);
  if (!m) return false;
  const [N, r, p] = [Number(m[1]), Number(m[2]), Number(m[3])];
  // Refuse parameters scrypt would reject, or that would make one login
  // attempt a memory bomb: N a power of two, and 128·N·r within maxmem.
  if (N < 2 || (N & (N - 1)) !== 0 || r < 1 || p < 1 || p > 4) return false;
  if (128 * N * r > SCRYPT_MAXMEM / 2) return false;
  const salt = Buffer.from(m[4]!, "base64");
  const expected = Buffer.from(m[5]!, "base64");
  if (!expected.length) return false;
  try {
    const key = await new Promise<Buffer>((resolve, reject) =>
      crypto.scrypt(password, salt, expected.length, { N, r, p, maxmem: SCRYPT_MAXMEM }, (err, k) =>
        err ? reject(err) : resolve(k),
      ),
    );
    return crypto.timingSafeEqual(key, expected);
  } catch {
    return false;
  }
}


/** Whether a stored hash can ever verify: the login page says so if not. */
export function usableHash(encoded: string): boolean {
  const m = HASH_RE.exec(encoded);
  if (!m) return false;
  const [N, r] = [Number(m[1]), Number(m[2])];
  return N >= 2 && (N & (N - 1)) === 0 && r >= 1 && 128 * N * r <= SCRYPT_MAXMEM / 2;
}

export { safeEqual } from "../policy/tokens.js";

// --- the lockout -----------------------------------------------------------

const WINDOW_MS = 15 * 60_000;

/**
 * Five failures lock a source for fifteen minutes, checked before scrypt runs.
 * Over the SSH tunnel every request arrives from the same bridge gateway, so
 * in practice this is one lock for everyone on the host; a restart clears it.
 */
export class LoginLimiter {
  private failures = new Map<string, { count: number; first: number; lockedUntil: number }>();
  private inFlight = false;

  constructor(private readonly now: () => number = Date.now) {}

  locked(source: string): boolean {
    const entry = this.failures.get(source);
    return Boolean(entry && entry.lockedUntil > this.now());
  }

  /** Returns true when this failure starts a lock. */
  fail(source: string): boolean {
    const at = this.now();
    const entry = this.failures.get(source) ?? { count: 0, first: at, lockedUntil: 0 };
    // Five failures within one window, not five in the lifetime of the
    // process: a typo today and four last month are not an attack.
    if ((entry.lockedUntil && entry.lockedUntil <= at) || at - entry.first > WINDOW_MS) {
      entry.count = 0;
      entry.first = at;
      entry.lockedUntil = 0;
    }
    entry.count++;
    const locks = entry.count >= 5 && !entry.lockedUntil;
    if (entry.count >= 5) entry.lockedUntil = at + WINDOW_MS;
    this.failures.set(source, entry);
    return locks;
  }

  success(source: string): void {
    this.failures.delete(source);
  }

  /** One scrypt at a time: a login flood must not starve the MCP relay's quarter-CPU. */
  acquire(): boolean {
    if (this.inFlight) return false;
    this.inFlight = true;
    return true;
  }

  release(): void {
    this.inFlight = false;
  }
}

// --- sessions --------------------------------------------------------------

export type Session = {
  id: string;
  csrf: string;
  created: number;
  seen: number;
  flash: string[];
  /** Freshly minted token plaintexts, by token id, shown once. */
  pending: Map<number, string>;
};

const IDLE_MS = 30 * 60_000;
const ABSOLUTE_MS = 8 * 60 * 60_000;

/**
 * In memory: logout really ends a session, and no signing key has to live in
 * the vault. A restart logs everyone out.
 */
export class SessionStore {
  private sessions = new Map<string, Session>();

  constructor(private readonly now: () => number = Date.now) {}

  create(): Session {
    const at = this.now();
    const session: Session = {
      id: crypto.randomBytes(32).toString("base64url"),
      csrf: crypto.randomBytes(32).toString("base64url"),
      created: at,
      seen: at,
      flash: [],
      pending: new Map(),
    };
    this.sessions.set(session.id, session);
    return session;
  }

  private expired(s: Session): boolean {
    const at = this.now();
    return at - s.seen > IDLE_MS || at - s.created > ABSOLUTE_MS;
  }

  get(id: string | undefined): Session | null {
    if (!id) return null;
    const session = this.sessions.get(id);
    if (!session) return null;
    if (this.expired(session)) {
      this.sessions.delete(id);
      return null;
    }
    session.seen = this.now();
    return session;
  }

  destroy(id: string): void {
    this.sessions.delete(id);
  }

  sweep(): void {
    for (const [id, session] of this.sessions) {
      if (this.expired(session)) this.sessions.delete(id);
    }
  }

  get size(): number {
    return this.sessions.size;
  }
}
