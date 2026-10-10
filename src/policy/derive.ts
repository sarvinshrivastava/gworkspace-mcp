/**
 * From grants to tools. Static and exhaustive: an upstream tool that is not in
 * this map is never exposed to anyone, whatever the allowlist says, so a new
 * upstream release cannot widen an agent's surface on its own.
 *
 * Deliberately absent, forever: setFilePermission (sharing is an exfiltration
 * channel), appendTableRows (the upstream registers two different tools under
 * that name), and everything Gmail.
 */

import type { Area, Grant, Op } from "./store.js";

export const AREAS: readonly Area[] = ["docs", "sheets", "drive", "calendar"];
export const OPS: readonly Op[] = ["read", "write", "create"];

export const TOOL_MAP: Record<`${Area}:${Op}`, readonly string[]> = {
  "docs:read": ["readDocument", "listDocuments", "searchDocuments"],
  "docs:write": ["appendText", "insertText", "findAndReplace", "appendMarkdown"],
  "docs:create": ["createDocument"],
  "sheets:read": ["readSpreadsheet", "listSpreadsheets"],
  "sheets:write": ["writeSpreadsheet", "appendRows"],
  "sheets:create": ["createSpreadsheet"],
  "drive:read": ["searchDriveFiles", "listDriveFiles"],
  "drive:write": [],
  "drive:create": ["createFolder"],
  "calendar:read": ["listEvents"],
  "calendar:write": ["createEvent", "updateEvent", "quickAddEvent"],
  "calendar:create": [],
};

/** Every grant that maps to at least one tool: exactly today's 20. */
export const DEFAULT_GRANTS: Grant[] = AREAS.flatMap((area) =>
  OPS.filter((op) => TOOL_MAP[`${area}:${op}`].length > 0).map((op) => ({ area, op })),
);

/**
 * Upstream lists these from the whole Drive with no folder argument, so for a
 * folder-confined agent they can only ever return nothing useful. Confined
 * agents search with searchDriveFiles, which the gateway scopes.
 */
export const SCOPED_WITHHELD: ReadonlySet<string> = new Set([
  "listDocuments",
  "searchDocuments",
  "listSpreadsheets",
]);

export const CALENDAR_TOOLS: ReadonlySet<string> = new Set([
  ...TOOL_MAP["calendar:read"],
  ...TOOL_MAP["calendar:write"],
]);

export function isGrantKey(value: string): value is `${Area}:${Op}` {
  return Object.prototype.hasOwnProperty.call(TOOL_MAP, value);
}

export function deriveTools(grants: Grant[]): Set<string> {
  const tools = new Set<string>();
  for (const g of grants) {
    for (const name of TOOL_MAP[`${g.area}:${g.op}`] ?? []) tools.add(name);
  }
  return tools;
}

/**
 * The tools one agent sees, in the upstream's own order — so the legacy
 * connection's tools/list is identical to what it was before policies existed.
 */
export function enabledTools(
  grants: Grant[],
  scoped: boolean,
  exposedOrder: string[],
  withheld: { scoped: ReadonlySet<string>; all: ReadonlySet<string> },
): string[] {
  const derived = deriveTools(grants);
  return exposedOrder.filter(
    (name) =>
      derived.has(name) &&
      !withheld.all.has(name) &&
      !(scoped && (SCOPED_WITHHELD.has(name) || withheld.scoped.has(name))),
  );
}
