import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import { DriveClient, DriveError, FOLDER_MIME, type DriveFile } from "../drive/client.js";
import { openPolicyStore, type PolicyStore } from "../policy/store.js";

export function tempDir(prefix = "gws-test-"): string {
  return mkdtempSync(path.join(tmpdir(), prefix));
}

export function tempStore(): PolicyStore {
  return openPolicyStore(path.join(tempDir(), "policy.sqlite"));
}

/** The 20 tools Open WebUI sees today, in the upstream's tools/list order. */
export const TODAYS_TOOLS = [
  "readDocument",
  "appendText",
  "insertText",
  "findAndReplace",
  "listDocuments",
  "searchDocuments",
  "listDriveFiles",
  "searchDriveFiles",
  "createFolder",
  "createDocument",
  "readSpreadsheet",
  "writeSpreadsheet",
  "appendRows",
  "createSpreadsheet",
  "listSpreadsheets",
  "appendMarkdown",
  "listEvents",
  "createEvent",
  "updateEvent",
  "quickAddEvent",
];

/** Silences console.warn/error for the duration of `fn`, returning what was written. */
export async function quietly<T>(fn: () => T | Promise<T>): Promise<{ result: T; logs: string[] }> {
  const logs: string[] = [];
  const { warn, error } = console;
  console.warn = (...args: unknown[]) => logs.push(args.map(String).join(" "));
  console.error = (...args: unknown[]) => logs.push(args.map(String).join(" "));
  try {
    return { result: await fn(), logs };
  } finally {
    console.warn = warn;
    console.error = error;
  }
}

// ---------------------------------------------------------------------------
// A Drive double. Files live in a map; errors can be injected per ID; every
// getFile/listFiles call is recorded. listFiles evaluates the small subset of
// the query language the gateway emits (see FakeDrive.matches).


export type FakeFile = DriveFile & { modifiedTime?: string; createdTime?: string };

export class FakeDrive extends DriveClient {
  files = new Map<string, FakeFile>();
  errors = new Map<string, DriveError>();
  getCalls: string[] = [];
  listCalls: Array<{ q: string; pageSize: number; orderBy?: string; pageToken?: string }> = [];
  /** When set, listFiles calls fail with this error. */
  listError: Error | null = null;
  /** Page size cap applied by the fake, to exercise pagination. */
  serverPageCap = Number.POSITIVE_INFINITY;
  /** Delay per listFiles call, to exercise budgets. */
  listDelayMs = 0;

  constructor() {
    super({ clientId: "x", clientSecret: "y", refreshToken: "z" });
  }

  add(id: string, parents: string[], extra: Partial<FakeFile> = {}): FakeFile {
    const f: FakeFile = {
      id,
      name: extra.name ?? id,
      mimeType: extra.mimeType ?? "application/vnd.google-apps.document",
      parents,
      trashed: extra.trashed ?? false,
      modifiedTime: extra.modifiedTime ?? "2026-01-01T00:00:00.000Z",
      createdTime: extra.createdTime ?? "2025-01-01T00:00:00.000Z",
    };
    this.files.set(id, f);
    return f;
  }

  folder(id: string, parents: string[], extra: Partial<FakeFile> = {}): FakeFile {
    return this.add(id, parents, { ...extra, mimeType: FOLDER_MIME });
  }

  override async getFile(id: string): Promise<DriveFile | null> {
    this.getCalls.push(id);
    const error = this.errors.get(id);
    if (error) throw error;
    const f = this.files.get(id);
    return f
      ? { id: f.id, name: f.name, mimeType: f.mimeType, parents: [...f.parents], trashed: f.trashed }
      : null;
  }

  override async listFiles(p: {
    q: string;
    fields: string;
    pageSize: number;
    orderBy?: string;
    pageToken?: string;
    signal?: AbortSignal;
  }): Promise<{ files: Record<string, unknown>[]; nextPageToken?: string }> {
    this.listCalls.push({ q: p.q, pageSize: p.pageSize, orderBy: p.orderBy, pageToken: p.pageToken });
    if (this.listDelayMs) {
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(resolve, this.listDelayMs);
        p.signal?.addEventListener("abort", () => {
          clearTimeout(timer);
          reject(p.signal?.reason ?? new Error("aborted"));
        });
      });
    }
    p.signal?.throwIfAborted();
    if (this.listError) throw this.listError;
    const matched = [...this.files.values()].filter((f) => FakeDrive.matches(f, p.q));
    const size = Math.min(p.pageSize, this.serverPageCap);
    const start = p.pageToken ? Number(p.pageToken) : 0;
    const page = matched.slice(start, start + size);
    const next = start + size < matched.length ? String(start + size) : undefined;
    return {
      files: page.map((f) => ({
        id: f.id,
        name: f.name,
        mimeType: f.mimeType,
        modifiedTime: f.modifiedTime,
        createdTime: f.createdTime,
        webViewLink: `https://drive.example/${f.id}`,
        owners: [{ displayName: "Owner" }],
        parents: f.parents,
      })),
      ...(next ? { nextPageToken: next } : {}),
    };
  }

  /**
   * Understands: trashed=false, mimeType='…', '<id>' in parents (OR-ed), and
   * name contains '…' / fullText contains '…' (both matched against the name).
   * Anything else in the query is ignored.
   */
  static matches(f: FakeFile, q: string): boolean {
    if (/trashed\s*=\s*false/.test(q) && f.trashed) return false;
    const mime = /mimeType='((?:[^'\\]|\\.)*)'/.exec(q);
    if (mime && f.mimeType !== mime[1]!.replace(/\\(.)/g, "$1")) return false;
    const parents = [...q.matchAll(/'((?:[^'\\]|\\.)*)' in parents/g)].map((m) => m[1]!);
    if (parents.length && !f.parents.some((p) => parents.includes(p))) return false;
    const term = /(?:name|fullText) contains '((?:[^'\\]|\\.)*)'/.exec(q);
    if (term && !f.name.includes(term[1]!.replace(/\\(.)/g, "$1"))) return false;
    return true;
  }
}

export const retryable = () => new DriveError(503, true, "Drive HTTP 503");
export const forbidden = () => new DriveError(403, false, "Drive HTTP 403");
