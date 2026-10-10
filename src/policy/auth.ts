/**
 * Bearer → agent.
 *
 * The legacy bearer (vault GWORKSPACE_MCP_TOKEN, what Open WebUI already sends)
 * is compared in memory and is never a row in `tokens`. Rotating it is a vault
 * edit and a restart with no database step, and the dashboard cannot revoke
 * the break-glass path by accident.
 */

import crypto from "node:crypto";

import { DEFAULT_GRANTS } from "./derive.js";
import { hashToken } from "./tokens.js";
import { DEFAULT_AGENT, type Grant, type PolicyStore, type Scope } from "./store.js";

export type AgentContext = {
  slug: string;
  scoped: boolean;
  grants: Grant[];
  scopes: Scope[];
};

export type Authenticator = (bearer: string) => AgentContext | null;

function legacyMatches(provided: string, legacy: string): boolean {
  if (!legacy) return false;
  // Digest both sides first: timingSafeEqual throws on a length mismatch, which
  // would itself be the leak.
  const a = crypto.createHash("sha256").update(provided).digest();
  const b = crypto.createHash("sha256").update(legacy).digest();
  return crypto.timingSafeEqual(a, b);
}

export function loadContext(store: PolicyStore, slug: string): AgentContext | null {
  const agent = store.getAgent(slug);
  if (!agent || agent.archivedAt !== null) return null;
  return {
    slug,
    scoped: agent.scoped,
    grants: store.getGrants(slug),
    scopes: store.getScopes(slug),
  };
}

export function makeAuthenticator(
  store: PolicyStore,
  legacyToken: () => string,
): Authenticator {
  return (bearer) => {
    if (!bearer) return null;
    if (legacyMatches(bearer, legacyToken())) return loadContext(store, DEFAULT_AGENT);
    const slug = store.agentForTokenHash(hashToken(bearer));
    return slug ? loadContext(store, slug) : null;
  };
}

/**
 * Used only when the database never existed and cannot be created. Nothing can
 * have been restricted yet, so the legacy bearer gets exactly what a first
 * boot would have seeded; minted tokens cannot exist and are refused.
 */
export function makeDegradedAuthenticator(legacyToken: () => string): Authenticator {
  return (bearer) =>
    bearer && legacyMatches(bearer, legacyToken())
      ? { slug: DEFAULT_AGENT, scoped: false, grants: DEFAULT_GRANTS, scopes: [] }
      : null;
}
