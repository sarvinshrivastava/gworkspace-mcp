/**
 * Is this Drive ID inside one of these folders?
 *
 * Walks the file's parents upward until it meets a scope folder. The cache
 * holds Drive NODES (id → parents, type, trashed), never verdicts: scopes
 * differ per agent and change in the dashboard, so the answer is recomputed
 * every time from nodes that are at most five minutes old.
 *
 * Writes pass `live`, which refetches every level. A folder moved out of scope
 * can therefore still be READ for up to the cache TTL, but never written.
 */

import { DriveError, type DriveClient, type DriveFile } from "../drive/client.js";
import { errorMessage } from "../errors.js";

export type DenyReason =
  | "not_found"
  | "outside"
  | "trashed"
  | "no_parents"
  | "too_deep"
  | "drive_error"
  | "bad_id"
  | "no_scope"
  | "no_primary"
  | "primary_unavailable"
  | "not_folder"
  | "pagination"
  | "initial_data"
  | "too_large"
  | "quickadd_guest"
  | "calendar_not_primary"
  | "event_has_guests"
  | "not_enabled"
  | "unmapped";

export type Verdict =
  | { ok: true; file: DriveFile }
  | { ok: false; reason: DenyReason; detail?: string };

const MAX_DEPTH = 32;

/** A Drive answer that ends a path: gone, or not ours to see. */
const isDefiniteNo = (error: unknown) =>
  error instanceof DriveError && !error.retryable && (error.status === 403 || error.status === 404);

export class ScopeResolver {
  private cache = new Map<string, { file: DriveFile | null; at: number }>();
  private readonly ttlMs: number;
  private readonly max: number;
  private readonly maxFetches: number;
  private readonly now: () => number;

  constructor(
    private readonly drive: () => DriveClient | null,
    opts: { ttlMs?: number; max?: number; maxFetches?: number; now?: () => number } = {},
  ) {
    this.ttlMs = opts.ttlMs ?? 300_000;
    this.max = opts.max ?? 2000;
    this.maxFetches = opts.maxFetches ?? 64;
    this.now = opts.now ?? Date.now;
  }

  flush(): void {
    this.cache.clear();
  }

  /** One node, from the cache unless `live`. Errors propagate and are never cached. */
  async node(id: string, live: boolean): Promise<DriveFile | null> {
    const drive = this.drive();
    if (!drive) throw new DriveError(0, true, "Drive client not ready");
    const hit = this.cache.get(id);
    if (!live && hit && this.now() - hit.at < this.ttlMs) {
      this.cache.delete(id);
      this.cache.set(id, hit);
      return hit.file;
    }
    const file = await drive.getFile(id);
    this.cache.delete(id);
    this.cache.set(id, { file, at: this.now() });
    while (this.cache.size > this.max) {
      this.cache.delete(this.cache.keys().next().value as string);
    }
    return file;
  }

  async check(id: string, scopeIds: ReadonlySet<string>, live: boolean): Promise<Verdict> {
    if (!this.drive()) return { ok: false, reason: "drive_error", detail: "not ready" };
    let fetches = 0;
    const fetchNode = async (nodeId: string): Promise<DriveFile | null> => {
      fetches++;
      return this.node(nodeId, live);
    };

    let target: DriveFile | null;
    try {
      target = await fetchNode(id);
    } catch (error) {
      if (isDefiniteNo(error)) return { ok: false, reason: "not_found" };
      return { ok: false, reason: "drive_error", detail: errorMessage(error) };
    }
    if (!target) return { ok: false, reason: "not_found" };
    if (target.trashed) return { ok: false, reason: "trashed" };
    if (scopeIds.has(target.id) || scopeIds.has(id)) return { ok: true, file: target };
    if (!target.parents.length) return { ok: false, reason: "no_parents" };

    let tooDeep = false;
    let sawTrashed = false;
    const visited = new Set<string>([target.id]);
    const stack: Array<{ id: string; depth: number }> = target.parents.map((p) => ({
      id: p,
      depth: 1,
    }));

    while (stack.length) {
      const { id: nodeId, depth } = stack.pop()!;
      if (visited.has(nodeId)) continue;
      visited.add(nodeId);
      if (depth > MAX_DEPTH || fetches >= this.maxFetches) {
        tooDeep = true;
        continue;
      }
      let node: DriveFile | null;
      try {
        node = await fetchNode(nodeId);
      } catch (error) {
        // A parent we may not see ends this path; Drive failing to answer ends
        // the whole check, because "try again" is the only honest result.
        if (isDefiniteNo(error)) continue;
        return { ok: false, reason: "drive_error", detail: errorMessage(error) };
      }
      if (!node) continue;
      if (node.trashed) {
        sawTrashed = true;
        continue;
      }
      if (scopeIds.has(nodeId)) return { ok: true, file: target };
      for (const parent of node.parents) stack.push({ id: parent, depth: depth + 1 });
    }

    if (tooDeep) return { ok: false, reason: "too_deep" };
    if (sawTrashed) return { ok: false, reason: "trashed" };
    return { ok: false, reason: "outside" };
  }
}
