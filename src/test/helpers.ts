import { mkdtempSync } from "node:fs";
import type { Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";

import { DriveClient, DriveError, FOLDER_MIME, type DriveFile } from "../drive/client.js";
import { openPolicyStore, type PolicyStore } from "../policy/store.js";
import { EXPECTED_KEYS, type UpstreamState, type UpstreamTool } from "../upstream.js";
import type express from "express";

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

// ---------------------------------------------------------------------------
// MCP over HTTP: a fake upstream child and a client call that reads the SSE.


export class FakeUpstream {
  calls: Array<{ name: string; arguments: Record<string, unknown> }> = [];
  /** Tool names whose next result is an isError. */
  failing = new Set<string>();

  async callTool(p: { name: string; arguments: Record<string, unknown> }): Promise<unknown> {
    this.calls.push(p);
    if (this.failing.has(p.name)) {
      return { content: [{ type: "text", text: "upstream said no" }], isError: true };
    }
    return { content: [{ type: "text", text: `ok:${p.name}` }] };
  }
}

/** Upstream tools with permissive schemas carrying the measured keys. */
export function upstreamTools(names: string[] = TODAYS_TOOLS): UpstreamTool[] {
  return names.map((name) => ({
    name,
    description: name,
    inputSchema: {
      type: "object",
      properties: Object.fromEntries(
        (EXPECTED_KEYS[name]?.keys ?? ["calendarId", "q", "timeMin", "timeMax", "maxResults", "singleEvents"]).map(
          (k) => [k, {}],
        ),
      ),
    },
  }));
}

export function fakeUpstreamState(client: FakeUpstream | null = new FakeUpstream()): UpstreamState {
  return {
    client,
    exposedTools: upstreamTools(),
    upstreamToolCount: 119,
    withheld: { scoped: new Set(), all: new Set() },
  };
}

export async function listen(app: express.Express): Promise<{ url: string; server: Server }> {
  const server = app.listen(0, "127.0.0.1");
  await new Promise<void>((resolve) => server.once("listening", () => resolve()));
  const { port } = server.address() as AddressInfo;
  return { url: `http://127.0.0.1:${port}`, server };
}

export type McpReply = { status: number; body: any };

/** One JSON-RPC request; parses either a JSON or an SSE response. */
export async function mcpCall(url: string, bearer: string | null, body: unknown): Promise<McpReply> {
  const res = await fetch(`${url}/mcp`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json, text/event-stream",
      ...(bearer ? { Authorization: `Bearer ${bearer}` } : {}),
    },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  const data = text
    .split("\n")
    .filter((line) => line.startsWith("data: "))
    .map((line) => line.slice(6));
  let parsed: unknown = null;
  try {
    parsed = JSON.parse(data.length ? data.join("") : text);
  } catch {
    parsed = text;
  }
  return { status: res.status, body: parsed };
}

export const listTools = (url: string, bearer: string) =>
  mcpCall(url, bearer, { jsonrpc: "2.0", id: 1, method: "tools/list", params: {} });

export const callTool = (url: string, bearer: string, name: string, args: Record<string, unknown>) =>
  mcpCall(url, bearer, { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name, arguments: args } });
