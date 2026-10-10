/**
 * Server-rendered pages. Forms only, no JavaScript — the CSP forbids scripts
 * outright — so every value below goes through html`` and is escaped.
 */

import { AREAS, OPS, TOOL_MAP } from "../policy/derive.js";
import { DEFAULT_AGENT, type AgentRow, type AuditRow, type Grant, type Scope, type TokenRow } from "../policy/store.js";
import { html, layout, raw, type Safe } from "./html.js";

const when = (ms: number | null) => (ms === null ? "—" : `${new Date(ms).toISOString().replace("T", " ").slice(0, 19)} UTC`);

const csrfField = (csrf: string) => html`<input type="hidden" name="_csrf" value="${csrf}">`;

const notes = (items: string[], kind: "note" | "error") =>
  items.map((text) => html`<div class="${kind}">${text}</div>`);

/** The audit page's decision filter, in display order. */
export const AUDIT_DECISIONS: readonly string[] = ["allow", "mutate", "deny", "error"];

type LoginState = "starting" | "unconfigured" | "locked" | "form" | "misconfigured";

function loginBody(state: LoginState, error?: string): Safe {
  switch (state) {
    case "starting":
      return html`<div class="note">Starting — the service is still reading its configuration. Try again in a moment.</div>`;
    case "unconfigured":
      return html`<div class="note">Dashboard login is not configured. Store the output of <code>npm run hash-password</code> as <code>DASHBOARD_ADMIN_PASSWORD_HASH</code> in the vault and restart.</div>`;
    case "misconfigured":
      return html`<div class="error">The stored <code>DASHBOARD_ADMIN_PASSWORD_HASH</code> is not a usable scrypt hash, so no password can match it. Replace it with the exact output of <code>npm run hash-password</code> and restart.</div>`;
    case "locked":
      return html`<div class="error">Too many failed logins. Locked for 15 minutes. Every tunnel connection shares one source address, so this lock applies to everyone; restarting the container clears it.</div>`;
    case "form":
      return html`${error ? html`<div class="error">${error}</div>` : ""}
            <form method="post" action="/login" class="row">
              <input type="password" name="password" autocomplete="current-password" aria-label="Password" required autofocus>
              <button type="submit">Log in</button>
            </form>`;
  }
}

export function loginPage(state: LoginState, error?: string): string {
  return layout("Log in", html`<section><h1>gworkspace-mcp admin</h1>${loginBody(state, error)}</section>`);
}

export function errorPage(status: number, message: string): string {
  return layout(`Error ${status}`, html`<section><h1>Error ${status}</h1><div class="error">${message}</div><p><a href="/">Back</a></p></section>`);
}

export function scopeBadge(a: { scoped: boolean; scopes: Scope[] }): Safe {
  if (!a.scoped) return html`<span class="badge">unconfined</span>`;
  if (!a.scopes.length) return html`<span class="badge bad">no folder</span>`;
  if (!a.scopes.some((s) => s.isPrimary)) return html`<span class="badge bad">no primary</span>`;
  return html`<span class="badge ok">confined</span>`;
}

/**
 * Open WebUI merges every connection's tools into one registry, so the same
 * tool name on two connections routes ambiguously.
 */
export function collisionWarnings(
  agents: Array<{ slug: string; tools: string[]; credentialed: boolean }>,
): string[] {
  const live = agents.filter((a) => a.credentialed);
  const out: string[] = [];
  for (let i = 0; i < live.length; i++) {
    for (let j = i + 1; j < live.length; j++) {
      const shared = live[i]!.tools.filter((t) => live[j]!.tools.includes(t));
      if (shared.length) {
        out.push(
          `${live[i]!.slug} and ${live[j]!.slug} both expose ${shared.join(", ")}. Open WebUI merges tools from every connection by name, so calls to these may reach either agent. Give each connection a disjoint set, or retire one connection.`,
        );
      }
    }
  }
  if (live.length > 4) {
    out.push(
      `${live.length} agents have active credentials. Each Open WebUI connection re-sends its tool schemas on every chat turn; keep the count small.`,
    );
  }
  return out;
}

export function agentWarnings(a: { scoped: boolean; scopes: Scope[]; grants: Grant[] }): string[] {
  const out: string[] = [];
  if (a.scoped && !a.scopes.length) {
    out.push("No folder assigned: this agent's Docs, Sheets and Drive calls are all denied until you add one.");
  }
  if (a.scoped && a.scopes.length && !a.scopes.some((s) => s.isPrimary)) {
    out.push("No primary folder: creates are denied. Mark one folder as primary.");
  }
  const has = (area: string, op: string) => a.grants.some((g) => g.area === area && g.op === op);
  if (a.scoped && (has("docs", "read") || has("sheets", "read")) && !has("drive", "read")) {
    out.push("No discovery tool: a confined agent finds files with searchDriveFiles. Grant drive:read, or it can only open IDs it is given.");
  }
  if (a.scoped) {
    out.push("Folder scope covers Docs, Sheets and Drive only. Calendar grants reach the whole calendar.");
  }
  return out;
}

export function agentsPage(p: {
  agents: Array<AgentRow & { tools: string[]; activeTokens: number; scopes: Scope[] }>;
  warnings: string[];
  flash: string[];
  csrf: string;
  /** Archived agents exist (they are not listed): the first-run note would be wrong. */
  anyArchived?: boolean;
}): string {
  const rows = p.agents.map(
    (a) => html`<tr>
      <td><a href="/agents/${a.slug}"><code>${a.slug}</code></a><br><span class="muted">${a.name}</span></td>
      <td>${a.tools.length}</td>
      <td>${a.slug === DEFAULT_AGENT ? `legacy bearer + ${a.activeTokens}` : a.activeTokens}</td>
      <td>${scopeBadge(a)}</td>
    </tr>`,
  );
  return layout(
    "Agents",
    html`${notes(p.flash, "note")}${notes(p.warnings, "error")}
    ${p.agents.length === 1 && !p.anyArchived
      ? html`<div class="note">Only the <code>default</code> agent exists. It is what the existing Open WebUI connection's bearer maps to. Create an agent per connection, mint it a token, and use that token as the connection's Bearer key.</div>`
      : ""}
    <section><h1>Agents</h1><div class="wrap"><table>
      <thead><tr><th>Agent</th><th>Tools</th><th>Active credentials</th><th>Folder scope</th></tr></thead>
      <tbody>${rows}</tbody></table></div></section>
    <section><h2>New agent</h2>
      <p class="muted">New agents start confined: no Docs, Sheets or Drive access until you assign a folder.</p>
      <form method="post" action="/agents" class="row">${csrfField(p.csrf)}
        <input type="text" name="slug" placeholder="slug (a-z, 0-9, -)" pattern="[a-z0-9][a-z0-9-]{1,31}" required aria-label="Slug">
        <input type="text" name="name" placeholder="Display name" maxlength="80" required aria-label="Name">
        <button type="submit">Create</button>
      </form></section>`,
    { nav: true, csrf: p.csrf },
  );
}

export function agentPage(p: {
  agent: AgentRow;
  grants: Grant[];
  tools: string[];
  scopes: Scope[];
  tokens: TokenRow[];
  pending: Array<[number, string]>;
  flash: string[];
  csrf: string;
  warnings: string[];
}): string {
  const a = p.agent;
  const isDefault = a.slug === DEFAULT_AGENT;
  const granted = (area: string, op: string) => p.grants.some((g) => g.area === area && g.op === op);

  const grid = html`<table><thead><tr><th>Area</th>${OPS.map((op) => html`<th>${op}</th>`)}</tr></thead><tbody>
    ${AREAS.map(
      (area) => html`<tr><td>${area}</td>${OPS.map((op) => {
        const tools = TOOL_MAP[`${area}:${op}`];
        return tools.length
          ? html`<td><label><input type="checkbox" name="g" value="${`${area}:${op}`}" ${granted(area, op) ? raw("checked") : ""}> <span class="muted">${tools.join(", ")}</span></label></td>`
          : html`<td class="muted">—</td>`;
      })}</tr>`,
    )}</tbody></table>`;

  const pending = p.pending.map(
    ([id, plaintext]) => html`<div class="secret"><strong>New token #${id} — copy it now, it is shown once:</strong><br><code>${plaintext}</code>
      <p class="muted">Use it as the Bearer key of one Open WebUI tool-server connection. A token you did not copy is useless; revoke it.</p></div>`,
  );

  const scopes = p.scopes.map(
    (s) => html`<tr><td>${s.folderName || "(unnamed)"}<br><code>${s.folderId}</code></td>
      <td>${s.isPrimary ? html`<span class="badge ok">primary</span>` : html`<form class="inline" method="post" action="/agents/${a.slug}/scopes/${s.folderId}/primary">${csrfField(p.csrf)}<button class="quiet" type="submit">Make primary</button></form>`}</td>
      <td><form class="inline" method="post" action="/agents/${a.slug}/scopes/${s.folderId}/remove">${csrfField(p.csrf)}<button class="danger" type="submit">Remove</button></form></td></tr>`,
  );

  const tokens = p.tokens.map(
    (t) => html`<tr><td><code>${t.prefix}…</code></td><td>${t.label}</td><td>${when(t.createdAt)}</td>
      <td>${t.revokedAt === null ? html`<form class="inline" method="post" action="/tokens/${t.id}/revoke">${csrfField(p.csrf)}<button class="danger" type="submit">Revoke</button></form>` : html`revoked ${when(t.revokedAt)}`}</td></tr>`,
  );

  const scopeSection = isDefault
    ? html`<section><h2>Folder scope</h2><p class="muted"><code>default</code> is the legacy Open WebUI connection and stays unconfined, so that connection keeps the tools it always had. Create a new agent for confined access.</p></section>`
    : html`<section><h2>Folder scope ${scopeBadge({ scoped: a.scoped, scopes: p.scopes })}</h2>
      ${a.scoped
        ? html`<div class="wrap"><table><tbody>${scopes}</tbody></table></div>
          <form method="post" action="/agents/${a.slug}/scopes" class="row spaced">${csrfField(p.csrf)}
            <input type="text" name="folder" placeholder="Folder ID or Drive folder URL" required aria-label="Folder" size="48">
            <label><input type="checkbox" name="primary" value="1"> primary (creates land here)</label>
            <button type="submit">Add folder</button>
          </form>
          <p><a href="/agents/${a.slug}/browse?parent=root">Browse Drive folders…</a></p>
          ${p.scopes.length ? "" : html`<form method="post" action="/agents/${a.slug}/unconfine" class="row">${csrfField(p.csrf)}
            <label><input type="checkbox" name="confirm" value="yes" required> I understand this gives the agent the whole Drive</label>
            <button class="danger" type="submit">Remove confinement</button></form>`}`
        : html`<p class="note">Unconfined: this agent reaches the whole Drive. Adding a folder confines it again.</p>
          <form method="post" action="/agents/${a.slug}/scopes" class="row">${csrfField(p.csrf)}
            <input type="text" name="folder" placeholder="Folder ID or Drive folder URL" required aria-label="Folder" size="48">
            <input type="hidden" name="primary" value="1"><button type="submit">Confine to folder</button></form>`}
    </section>`;

  return layout(
    a.slug,
    html`${notes(p.flash, "note")}${pending}${notes(p.warnings, "error")}
    <section><h1><code>${a.slug}</code> <span class="muted">${a.name}</span></h1>
      ${a.archivedAt !== null ? html`<div class="error">Archived ${when(a.archivedAt)}.</div>` : ""}
      <p class="muted">Enabled tools (${p.tools.length}): ${p.tools.length ? p.tools.join(", ") : "none"}</p></section>
    <section><h2>Grants</h2>
      <form method="post" action="/agents/${a.slug}/grants">${csrfField(p.csrf)}<div class="wrap">${grid}</div>
      <p><button type="submit">Save grants</button></p></form></section>
    ${scopeSection}
    <section><h2>Tokens</h2>
      ${isDefault ? html`<p class="muted">The legacy bearer (vault <code>GWORKSPACE_MCP_TOKEN</code>) always maps here and is rotated in the vault, not here.</p>` : ""}
      <div class="wrap"><table><thead><tr><th>Prefix</th><th>Label</th><th>Created</th><th></th></tr></thead><tbody>${tokens}</tbody></table></div>
      <form method="post" action="/agents/${a.slug}/tokens" class="row spaced">${csrfField(p.csrf)}
        <input type="text" name="label" placeholder="Label (e.g. Open WebUI chat-main)" maxlength="80" aria-label="Label">
        <button type="submit">Mint token</button></form></section>
    ${isDefault || a.archivedAt !== null ? "" : html`<section><h2>Archive</h2>
      <form method="post" action="/agents/${a.slug}/archive" class="row">${csrfField(p.csrf)}
        <span class="muted">Revokes every token. The slug stays reserved.</span>
        <button class="danger" type="submit">Archive agent</button></form></section>`}`,
    { nav: true, csrf: p.csrf },
  );
}

export function browsePage(p: {
  slug: string;
  parent: string;
  folders: Array<{ id: string; name: string }>;
  more?: boolean;
  csrf: string;
}): string {
  const rows = p.folders.map(
    (f) => html`<tr><td><a href="/agents/${p.slug}/browse?parent=${encodeURIComponent(f.id)}">${f.name}</a><br><code>${f.id}</code></td>
      <td><form class="inline" method="post" action="/agents/${p.slug}/scopes">${csrfField(p.csrf)}<input type="hidden" name="folder" value="${f.id}"><button class="quiet" type="submit">Add as scope</button></form></td></tr>`,
  );
  return layout(
    "Browse",
    html`<section><h1>Folders in <code>${p.parent}</code></h1>
      <p><a href="/agents/${p.slug}">← back to ${p.slug}</a> · <a href="/agents/${p.slug}/browse?parent=root">My Drive</a></p>
      ${p.folders.length ? html`<div class="wrap"><table><tbody>${rows}</tbody></table></div>` : html`<p class="muted">No folders here.</p>`}
      ${p.more ? html`<p class="note">Only the first 200 folders are shown. Paste the folder's ID or URL on the agent page to add one not listed.</p>` : ""}
      <p class="muted">Shared drives and folders shared with this account are not browsable here; paste their URL on the agent page.</p></section>`,
    { nav: true, csrf: p.csrf },
  );
}

export function auditPage(p: {
  rows: AuditRow[];
  agent?: string;
  decision?: string;
  agents: string[];
  csrf: string;
}): string {
  const option = (value: string, current: string | undefined, label = value) =>
    html`<option value="${value}" ${value === (current ?? "") ? raw("selected") : ""}>${label}</option>`;
  const badgeClass = (decision: AuditRow["decision"]) => {
    if (decision === "deny" || decision === "error") return "bad";
    if (decision === "mutate") return "warn";
    return "ok";
  };
  const rows = p.rows.map(
    (r) => html`<tr><td>${when(r.ts)}</td><td>${r.kind}</td><td>${r.agent ?? "—"}</td><td>${r.tool ?? "—"}</td>
      <td><span class="badge ${badgeClass(r.decision)}">${r.decision}</span></td>
      <td>${r.reason}</td><td>${r.resources.map((id) => html`<code>${id}</code> `)}</td><td>${r.source}</td></tr>`,
  );
  return layout(
    "Audit",
    html`<section><h1>Audit log</h1>
      <form method="get" action="/audit" class="row">
        <select name="agent" aria-label="Agent">${option("", p.agent, "all agents")}${p.agents.map((a) => option(a, p.agent))}</select>
        <select name="decision" aria-label="Decision">${option("", p.decision, "all decisions")}${AUDIT_DECISIONS.map((d) => option(d, p.decision))}</select>
        <button type="submit">Filter</button></form>
      <p class="muted">Newest 200. Resource IDs only; argument values and results are never recorded. Kept 90 days.</p>
      <div class="wrap"><table><thead><tr><th>Time</th><th>Kind</th><th>Agent</th><th>Tool</th><th>Decision</th><th>Reason</th><th>Resources</th><th>Source</th></tr></thead>
      <tbody>${rows}</tbody></table></div></section>`,
    { nav: true, csrf: p.csrf },
  );
}
