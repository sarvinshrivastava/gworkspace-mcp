/**
 * What to do with one tools/call: deny it, relay it (perhaps with rewritten
 * arguments), or serve it from the gateway.
 *
 * Order matters and is fixed: guards that apply to every agent run first, so
 * no early return can skip them; calendar tools are never folder-scoped;
 * unconfined agents are then a plain relay, exactly as before policies.
 */

import { DriveError, FOLDER_MIME, categoryOf, rejectionDetail, type DriveClient } from "../drive/client.js";
import { CALENDAR_TOOLS } from "../policy/derive.js";
import type { AgentContext } from "../policy/auth.js";
import { MAX_RESOURCES } from "../policy/store.js";
import { ALL_AGENT_GUARDS, EVENT_TIME_ONLY_KEYS, ID_RE } from "./guards.js";
import { errorMessage } from "../errors.js";
import type { DenyReason, DriveReason, ScopeResolver, Verdict } from "./resolver.js";
import { scopedSearchDriveFiles, type SubtreeCache } from "./search.js";

export type Plan =
  | { kind: "deny"; message: string; reason: DriveReason; category: string; detail?: string; resources: string[] }
  | { kind: "deny"; message: string; reason: Exclude<DenyReason, DriveReason>; detail?: string; resources: string[] }
  | {
      kind: "relay";
      args: Record<string, unknown>;
      mutated: boolean;
      resources: string[];
      /** Runs after a successful upstream call. */
      after?: () => void;
    }
  | { kind: "serve"; run: () => Promise<string>; resources: string[] };

export type PipelineDeps = {
  resolver: ScopeResolver;
  subtree: SubtreeCache;
  drive: () => DriveClient | null;
};

/** The argument holding the file a tool reads or writes. */
export const ID_ARG: Record<string, string> = {
  readDocument: "documentId",
  appendText: "documentId",
  insertText: "documentId",
  findAndReplace: "documentId",
  appendMarkdown: "documentId",
  readSpreadsheet: "spreadsheetId",
  writeSpreadsheet: "spreadsheetId",
  appendRows: "spreadsheetId",
};

export const WRITE_TOOLS: ReadonlySet<string> = new Set([
  "appendText",
  "insertText",
  "findAndReplace",
  "appendMarkdown",
  "writeSpreadsheet",
  "appendRows",
]);

export const CREATE_TOOLS: ReadonlySet<string> = new Set([
  "createDocument",
  "createSpreadsheet",
  "createFolder",
]);

export function denyMessage(reason: DenyReason, id?: string, detail?: string): string {
  switch (reason) {
    case "not_found":
    case "outside":
    case "trashed":
    case "no_parents":
    case "too_deep":
      return `Denied: \`${id ?? "that file"}\` was not found or is outside this agent's folder scope.`;
    case "drive_error":
      return "Could not verify the folder scope right now — try again.";
    case "credential_rejected":
      return `Google access rejected${detail ? ` (${detail})` : ""} — do not retry. The Workspace owner must re-consent and update GOOGLE_TOKEN_JSON, or enable the API.`;
    case "calendar_rejected":
      return `Google Calendar access rejected${detail ? ` (${detail})` : ""} — do not retry. The Workspace owner must enable the Calendar API or grant the Calendar scope.`;
    case "no_scope":
      return "This agent has no Drive folder assigned yet.";
    case "bad_id":
      return "Denied: that is not a valid Google file ID.";
    case "no_primary":
      return "Denied: this agent has no primary folder to create files in.";
    case "primary_unavailable":
      return "Denied: this agent's folder is missing or in the trash.";
    case "not_folder":
      return "Denied: the parent must be a folder inside this agent's scope.";
    case "pagination":
      return "Paging is not available here — narrow the query (name, mimeType, folderId or modifiedAfter) instead.";
    case "initial_data":
      return "Create the spreadsheet without data, then use writeSpreadsheet.";
    case "too_large":
      return "This agent's folder tree is too large to search (over 500 folders) — pass a folderId deeper in the tree.";
    case "quickadd_guest":
      return "quickAddEvent cannot add guests — remove the e-mail address.";
    case "calendar_not_primary":
      return "Denied: events can only be written to this account's own calendar (calendarId \"primary\").";
    case "event_unverifiable":
      return "Could not check this event's guests, so it was not changed. Try again; changing only its time does not need the check.";
    case "event_has_guests":
      return "Denied: that event has guests, so only its time (start, end) can be changed here.";
    case "not_enabled":
    case "unmapped":
      return "Denied: this tool is not enabled for this agent.";
  }
}

const deny = (
  reason: Exclude<DenyReason, DriveReason>,
  resources: string[],
  id?: string,
  detail?: string,
): Plan => ({
  kind: "deny",
  reason,
  message: denyMessage(reason, id, detail),
  ...(detail === undefined ? {} : { detail }),
  resources: resources.slice(0, MAX_RESOURCES),
});

/** A denial because Google could not answer or refused; the category says which, and cannot be left out. */
const denyDrive = (reason: DriveReason, resources: string[], category: string, detail?: string): Plan => ({
  kind: "deny",
  reason,
  category,
  message: denyMessage(reason, undefined, reason === "credential_rejected" ? detail : undefined),
  ...(detail === undefined ? {} : { detail }),
  resources: resources.slice(0, MAX_RESOURCES),
});

const validId = (value: unknown): value is string =>
  typeof value === "string" && ID_RE.test(value);

export async function planCall(
  ctx: AgentContext,
  tool: string,
  rawArgs: Record<string, unknown>,
  deps: PipelineDeps,
): Promise<Plan> {
  // 1. Guards for every agent, before any early return.
  let args = rawArgs;
  let mutated = false;
  const guard = ALL_AGENT_GUARDS[tool];
  if (guard) {
    const result = guard(rawArgs);
    if ("deny" in result) return deny(result.deny, []);
    args = result.args;
    mutated = result.mutated;
  }

  // 1b. updateEvent is a PATCH: guests already on the event stay, and new text
  // syncs to their calendars even with sendUpdates "none". So an event with
  // guests keeps its words; only its time may move.
  // No eventId: the upstream's schema rejects it; nothing to look up.
  if (
    tool === "updateEvent" &&
    typeof args.eventId === "string" &&
    args.eventId &&
    Object.keys(args).some((key) => args[key] !== undefined && !EVENT_TIME_ONLY_KEYS.has(key))
  ) {
    const drive = deps.drive();
    if (!drive) return deny("event_unverifiable", []);
    let guests: string[] | null;
    try {
      // null: no such event — the upstream reports that in its own words.
      guests = await drive.eventGuests("primary", args.eventId);
    } catch (error) {
      // Fail closed, transient or not: an unchecked event may have guests.
      // Logged, because a permanent cause (no Calendar scope) looks the same.
      console.error(`[guard] could not read an event's guests: ${errorMessage(error)}`);
      if (error instanceof DriveError && error.rejection) {
        return error.source === "calendar"
          ? deny("calendar_rejected", [], undefined, rejectionDetail(error))
          : denyDrive("credential_rejected", [], categoryOf(error), rejectionDetail(error));
      }
      return deny("event_unverifiable", []);
    }
    if (guests?.length) return deny("event_has_guests", []);
  }

  // 2. Calendar is never folder-scoped. 3. Unconfined agents relay as before —
  // still naming the file in the audit, and still making a new folder visible
  // to confined agents' searches at once.
  if (CALENDAR_TOOLS.has(tool) || !ctx.scoped) {
    const id = ID_ARG[tool] ? args[ID_ARG[tool]] : undefined;
    return {
      kind: "relay",
      args,
      mutated,
      resources: validId(id) ? [id] : [],
      ...(tool === "createFolder" ? { after: () => deps.subtree.flush() } : {}),
    };
  }

  // 4. Confined with no folder: no Docs, Sheets or Drive at all.
  if (!ctx.scopes.length) return deny("no_scope", []);

  const scopeIds = new Set(ctx.scopes.map((s) => s.folderId));
  const primary = ctx.scopes.find((s) => s.isPrimary)?.folderId;
  const r = deps.resolver;

  const verdictDeny = (v: Verdict & { ok: false }, id: string): Plan =>
    v.reason === "drive_error" || v.reason === "credential_rejected"
      ? denyDrive(v.reason, [id], v.category, v.detail)
      : deny(v.reason, [id], id);

  /** A folder argument the agent supplied: valid, in scope, and a folder. */
  const checkFolder = async (value: unknown, live: boolean): Promise<Plan | string> => {
    if (!validId(value)) return deny("bad_id", []);
    const v = await r.check(value, scopeIds, live);
    if (!v.ok) return verdictDeny(v, value);
    if (v.file.mimeType !== FOLDER_MIME) return deny("not_folder", [value], value);
    return value;
  };

  /** The primary, checked before anything is put in it. */
  const checkPrimary = async (live: boolean): Promise<Plan | string> => {
    if (!primary) return deny("no_primary", []);
    const v = await r.check(primary, scopeIds, live);
    // Drive not answering is "try again", never "your folder is gone".
    if (!v.ok && (v.reason === "drive_error" || v.reason === "credential_rejected")) {
      return denyDrive(v.reason, [primary], v.category, v.detail);
    }
    if (!v.ok || v.file.mimeType !== FOLDER_MIME) return deny("primary_unavailable", [primary]);
    return primary;
  };

  /**
   * The folder in `key` when the agent gave one, else the primary — and filling
   * in the primary rewrites the call. `in`, not a value check: "" is "given",
   * and the upstream treats it as Drive root.
   */
  const folderOrPrimary = async (key: string, live: boolean): Promise<Plan | string> => {
    if (key in args) return checkFolder(args[key], live);
    const fallback = await checkPrimary(live);
    if (typeof fallback === "string") mutated = true;
    return fallback;
  };

  // 6. Reads and writes of one file.
  const idArg = ID_ARG[tool];
  if (idArg) {
    const id = args[idArg];
    if (!validId(id)) return deny("bad_id", []);
    const v = await r.check(id, scopeIds, WRITE_TOOLS.has(tool));
    if (!v.ok) return verdictDeny(v, id);
    if (tool === "writeSpreadsheet" || tool === "appendRows") {
      // USER_ENTERED evaluates formulas, and =IMPORTRANGE("<any id>") reads a
      // spreadsheet outside the scope into one inside it.
      if (args.valueInputOption !== "RAW") {
        args = { ...args, valueInputOption: "RAW" };
        mutated = true;
      }
    }
    return { kind: "relay", args, mutated, resources: [id] };
  }

  // 7. Creates: inside the scope, or in the primary when no parent is given.
  if (CREATE_TOOLS.has(tool)) {
    // The upstream writes initialData USER_ENTERED, unconditionally.
    if (tool === "createSpreadsheet" && args.initialData !== undefined) {
      return deny("initial_data", []);
    }
    const parent = await folderOrPrimary("parentFolderId", true);
    if (typeof parent !== "string") return parent;
    return {
      kind: "relay",
      args: { ...args, parentFolderId: parent },
      mutated,
      resources: [parent],
      // A new folder must be searchable at once, not after the subtree TTL.
      ...(tool === "createFolder" ? { after: () => deps.subtree.flush() } : {}),
    };
  }

  // 8. Direct children of one folder.
  if (tool === "listDriveFiles") {
    const folder = await folderOrPrimary("folderId", false);
    if (typeof folder !== "string") return folder;
    return { kind: "relay", args: { ...args, folderId: folder }, mutated, resources: [folder] };
  }

  // 9. Search, served here.
  if (tool === "searchDriveFiles") {
    if (args.pageToken !== undefined) return deny("pagination", []);
    let roots: string[];
    if ("folderId" in args) {
      const folder = await checkFolder(args.folderId, false);
      if (typeof folder !== "string") return folder;
      roots = [folder];
    } else {
      roots = [];
      for (const id of scopeIds) {
        const v = await r.check(id, scopeIds, false);
        if (v.ok) roots.push(id);
        else if (v.reason === "drive_error" || v.reason === "credential_rejected") {
          return denyDrive(v.reason, [id], v.category, v.detail);
        }
      }
      if (!roots.length) return deny("primary_unavailable", [...scopeIds]);
    }
    const drive = deps.drive();
    if (!drive) return denyDrive("drive_error", [], "unavailable", "not ready");
    const { folderId: _ignored, ...searchArgs } = args;
    return {
      kind: "serve",
      resources: roots.slice(0, MAX_RESOURCES),
      run: () => scopedSearchDriveFiles(drive, deps.subtree, roots, searchArgs),
    };
  }

  // 10. Withheld from confined agents; unreachable through tools/list.
  return deny("unmapped", []);
}
