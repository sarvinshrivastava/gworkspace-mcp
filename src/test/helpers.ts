import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

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
