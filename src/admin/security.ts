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
    const base = ["127.0.0.1", ...gatewaysFromRoute(net.routeText() ?? ""), ...firstHosts(net.interfaces()), ...extra];
    const set = new Set<string>(["::1"]);
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

// --- the password ----------------------------------------------------------

const SCRYPT = { N: 16384, r: 8, p: 1, keylen: 32 } as const;

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
  // Refuse parameters that would make one login attempt a memory bomb.
  if (N > 1 << 17 || r > 16 || p > 4) return false;
  const salt = Buffer.from(m[4]!, "base64");
  const expected = Buffer.from(m[5]!, "base64");
  if (!expected.length) return false;
  const key = await new Promise<Buffer>((resolve, reject) =>
    crypto.scrypt(password, salt, expected.length, { N, r, p, maxmem: 64 * 1024 * 1024 }, (err, k) =>
      err ? reject(err) : resolve(k),
    ),
  );
  return crypto.timingSafeEqual(key, expected);
}

export function safeEqual(a: string, b: string): boolean {
  const x = crypto.createHash("sha256").update(a).digest();
  const y = crypto.createHash("sha256").update(b).digest();
  return crypto.timingSafeEqual(x, y);
}

// --- the lockout -----------------------------------------------------------

/**
 * Five failures lock a source for fifteen minutes, checked before scrypt runs.
 * Over the SSH tunnel every request arrives from the same bridge gateway, so
 * in practice this is one lock for everyone on the host; a restart clears it.
 */
export class LoginLimiter {
  private failures = new Map<string, { count: number; lockedUntil: number }>();
  private inFlight = false;

  constructor(private readonly now: () => number = Date.now) {}

  locked(source: string): boolean {
    const entry = this.failures.get(source);
    return Boolean(entry && entry.lockedUntil > this.now());
  }

  fail(source: string): void {
    const entry = this.failures.get(source) ?? { count: 0, lockedUntil: 0 };
    if (entry.lockedUntil && entry.lockedUntil <= this.now()) entry.count = 0;
    entry.count++;
    if (entry.count >= 5) entry.lockedUntil = this.now() + 15 * 60_000;
    this.failures.set(source, entry);
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
