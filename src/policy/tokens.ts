import crypto from "node:crypto";

export function hashToken(plaintext: string): string {
  return crypto.createHash("sha256").update(plaintext).digest("hex");
}

/** Constant-time string comparison: the legacy bearer, the dashboard's CSRF token. */
export function safeEqual(a: string, b: string): boolean {
  // Digest both sides first: timingSafeEqual throws on a length mismatch, which
  // would itself be the leak.
  const x = crypto.createHash("sha256").update(a).digest();
  const y = crypto.createHash("sha256").update(b).digest();
  return crypto.timingSafeEqual(x, y);
}

/**
 * A bearer for one agent. 256 random bits, so a plain sha256 is the right
 * storage: there is nothing to brute-force that a slow hash would protect.
 * Only the hash and a short prefix (to tell tokens apart) are kept.
 */
export function mintToken(): { plaintext: string; hash: string; prefix: string } {
  const plaintext = `gws_${crypto.randomBytes(32).toString("base64url")}`;
  return { plaintext, hash: hashToken(plaintext), prefix: plaintext.slice(0, 12) };
}
