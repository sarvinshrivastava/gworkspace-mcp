/**
 * searchDriveFiles for a folder-confined agent, served by the gateway.
 *
 * The upstream cannot do this: it scopes with `'<id>' in ancestors`, which is
 * not a Drive search term, so any folderId fails with "Invalid Value"
 * (learnings/drive-search-folder-scoping.md). Drive only knows direct
 * parents, so the gateway expands the scope into its folder set first and
 * then searches `… and ('f1' in parents or 'f2' in parents …)` in chunks.
 *
 * Arguments and output mirror the upstream tool, so the model sees the same
 * tool either way. The SDK applies none of the schema's defaults (its Ajv has
 * no useDefaults), so they are applied here.
 */

import { FOLDER_MIME, type DriveClient } from "../drive/client.js";

export const SEARCH_DEFAULTS = {
  searchIn: "both",
  orderBy: "modifiedTime",
  sortDirection: "desc",
  maxResults: 10,
} as const;

/** The upstream's own shortcuts (searchDriveFiles.js), verbatim. */
const MIME_TYPE_SHORTCUTS: Record<string, string> = {
  document: "application/vnd.google-apps.document",
  spreadsheet: "application/vnd.google-apps.spreadsheet",
  presentation: "application/vnd.google-apps.presentation",
  folder: FOLDER_MIME,
  form: "application/vnd.google-apps.form",
  pdf: "application/pdf",
  zip: "application/zip",
};

const CHUNK = 40;
const RESULT_FIELDS =
  "nextPageToken,incompleteSearch,files(id,name,mimeType,size,modifiedTime,createdTime,webViewLink,owners(displayName,emailAddress),parents)";

export class ScopeTooLarge extends Error {}

/** Same escaping as the upstream's escapeDriveQuery: backslash first, then quote. */
export function escapeQ(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/'/g, "\\'");
}

const chunks = <T>(items: T[], size: number): T[][] => {
  const out: T[][] = [];
  for (let i = 0; i < items.length; i += size) out.push(items.slice(i, i + size));
  return out;
};

const anyParent = (ids: string[]) => ids.map((id) => `'${escapeQ(id)}' in parents`).join(" or ");

/**
 * Each scope's folder tree, five minutes at a time. Expanded level by level:
 * one query per 40 folders of the frontier, so a tree costs depth × width/40
 * calls rather than one call per folder.
 */
export class SubtreeCache {
  /** `folders: null` remembers ScopeTooLarge, so an oversized scope is not re-walked on every call. */
  private cache = new Map<string, { folders: string[] | null; at: number }>();
  private static readonly MAX_ENTRIES = 200;
  private readonly ttlMs: number;
  private readonly cap: number;
  private readonly now: () => number;

  constructor(
    private readonly drive: () => DriveClient | null,
    opts: { ttlMs?: number; cap?: number; now?: () => number } = {},
  ) {
    this.ttlMs = opts.ttlMs ?? 300_000;
    this.cap = opts.cap ?? 500;
    this.now = opts.now ?? Date.now;
  }

  flush(): void {
    this.cache.clear();
  }

  async folders(roots: string[], signal: AbortSignal): Promise<string[]> {
    const key = [...roots].sort().join(",");
    const hit = this.cache.get(key);
    if (hit && this.now() - hit.at < this.ttlMs) {
      if (!hit.folders) throw new ScopeTooLarge(`more than ${this.cap} folders in scope`);
      return hit.folders;
    }
    this.cache.delete(key);

    const drive = this.drive();
    if (!drive) throw new Error("Drive client not ready");

    const seen = new Set(roots);
    let frontier = [...roots];
    while (frontier.length) {
      const next: string[] = [];
      for (const group of chunks(frontier, CHUNK)) {
        let pageToken: string | undefined;
        do {
          const page = await drive.listFiles({
            q: `(${anyParent(group)}) and mimeType='${FOLDER_MIME}' and trashed=false`,
            fields: "nextPageToken,files(id)",
            pageSize: 1000,
            pageToken,
            signal,
          });
          for (const file of page.files) {
            const id = String(file.id);
            if (seen.has(id)) continue;
            seen.add(id);
            next.push(id);
            if (seen.size > this.cap) {
              this.remember(key, null);
              throw new ScopeTooLarge(`more than ${this.cap} folders in scope`);
            }
          }
          pageToken = page.nextPageToken;
        } while (pageToken);
      }
      frontier = next;
    }

    const folders = [...seen];
    this.remember(key, folders);
    return folders;
  }

  /** Bounded: every distinct folderId an agent searches is its own key. */
  private remember(key: string, folders: string[] | null): void {
    this.cache.delete(key);
    this.cache.set(key, { folders, at: this.now() });
    while (this.cache.size > SubtreeCache.MAX_ENTRIES) {
      this.cache.delete(this.cache.keys().next().value as string);
    }
  }
}

type SearchArgs = {
  query: string;
  searchIn: string;
  mimeType?: string;
  orderBy: string;
  sortDirection: string;
  maxResults: number;
  modifiedAfter?: string;
};

/** The upstream's conditions, each parenthesised so none can absorb another. */
export function buildConditions(args: SearchArgs): string[] {
  const term = escapeQ(args.query);
  const conditions = ["trashed=false"];
  if (args.searchIn === "name") conditions.push(`name contains '${term}'`);
  else if (args.searchIn === "content") conditions.push(`fullText contains '${term}'`);
  else conditions.push(`name contains '${term}' or fullText contains '${term}'`);
  if (args.mimeType) {
    const resolved = MIME_TYPE_SHORTCUTS[args.mimeType] ?? args.mimeType;
    conditions.push(`mimeType='${escapeQ(resolved)}'`);
  }
  // Truthiness, as the upstream: "" means no filter, not an invalid date.
  if (args.modifiedAfter) {
    // Throws RangeError("Invalid time value") exactly where the upstream would.
    const cutoff = new Date(args.modifiedAfter).toISOString();
    conditions.push(`modifiedTime > '${escapeQ(cutoff)}'`);
  }
  return conditions;
}

export function searchQuery(conditions: string[], parents: string[]): string {
  return `${conditions.map((c) => `(${c})`).join(" and ")} and (${anyParent(parents)})`;
}

export async function scopedSearchDriveFiles(
  drive: DriveClient,
  subtree: SubtreeCache,
  roots: string[],
  rawArgs: Record<string, unknown>,
  budgetMs = 25_000,
): Promise<string> {
  const args = { ...SEARCH_DEFAULTS, ...rawArgs } as SearchArgs;
  let conditions: string[];
  try {
    conditions = buildConditions(args);
  } catch {
    throw new Error("Invalid time value");
  }

  const signal = AbortSignal.timeout(budgetMs);
  const folders = await subtree.folders(roots, signal);
  const orderBy =
    args.sortDirection === "desc" ? `${args.orderBy} desc` : args.orderBy;

  const byId = new Map<string, Record<string, unknown>>();
  let more = false;
  for (const group of chunks(folders, CHUNK)) {
    const page = await drive.listFiles({
      q: searchQuery(conditions, group),
      fields: RESULT_FIELDS,
      pageSize: args.maxResults,
      orderBy,
      signal,
    });
    if (page.nextPageToken || page.incompleteSearch) more = true;
    for (const file of page.files) byId.set(String(file.id), file);
  }

  // Drive sorted each chunk; the merge has to sort again. Names compare with
  // localeCompare, which can differ from Drive's collation at the margins.
  const key = args.orderBy;
  const direction = args.sortDirection === "asc" ? 1 : -1;
  const merged = [...byId.values()].sort((a, b) => {
    const x = String(a[key] ?? "");
    const y = String(b[key] ?? "");
    const order = key === "name" ? x.localeCompare(y) : x < y ? -1 : x > y ? 1 : 0;
    return order * direction;
  });
  if (merged.length > args.maxResults) more = true;

  const files = merged.slice(0, args.maxResults).map((file) => ({
    id: file.id,
    name: file.name,
    mimeType: file.mimeType,
    size: file.size != null ? Number(file.size) : null,
    modifiedTime: file.modifiedTime,
    createdTime: file.createdTime,
    owner: (file.owners as Array<{ displayName?: string }> | undefined)?.[0]?.displayName || null,
    url: file.webViewLink,
  }));
  return JSON.stringify({ files, total: files.length, hasMore: more }, null, 2);
}
