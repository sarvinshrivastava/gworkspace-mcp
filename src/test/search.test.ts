import { test } from "node:test";
import assert from "node:assert/strict";

import {
  ScopeTooLarge,
  SubtreeCache,
  buildConditions,
  escapeQ,
  scopedSearchDriveFiles,
  searchQuery,
} from "../enforce/search.js";
import { FakeDrive } from "./helpers.js";

const signal = () => AbortSignal.timeout(5_000);

function scopeWithDocs(count: number) {
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", ["root"]);
  drive.folder("sub-folder-1", ["scope-F-0001"]);
  drive.add("outside-doc1", ["elsewhere"], { name: "report outside" });
  for (let i = 0; i < count; i++) {
    const day = String(i + 1).padStart(2, "0");
    drive.add(`doc-${day}-xxxxx`, [i % 2 ? "sub-folder-1" : "scope-F-0001"], {
      name: `report ${day}`,
      modifiedTime: `2026-03-${day}T00:00:00.000Z`,
    });
  }
  return drive;
}

test("defaults are applied when only {query} is given", async () => {
  const drive = scopeWithDocs(25);
  const out = JSON.parse(
    await scopedSearchDriveFiles(drive, new SubtreeCache(() => drive), ["scope-F-0001"], { query: "report" }),
  );
  assert.equal(out.files.length, 10);
  assert.equal(out.hasMore, true);
  const times = out.files.map((f: { modifiedTime: string }) => f.modifiedTime);
  assert.deepEqual(times, [...times].sort().reverse());
  const search = drive.listCalls.at(-1)!;
  assert.equal(search.pageSize, 10);
  assert.equal(search.orderBy, "modifiedTime desc");
  assert.match(search.q, /\(name contains 'report' or fullText contains 'report'\)/);
});

test("only files under the scope are returned", async () => {
  const drive = scopeWithDocs(3);
  const out = JSON.parse(
    await scopedSearchDriveFiles(drive, new SubtreeCache(() => drive), ["scope-F-0001"], {
      query: "report",
      maxResults: 50,
    }),
  );
  assert.equal(out.files.length, 3);
  assert.ok(!out.files.some((f: { id: string }) => f.id === "outside-doc1"));
  assert.equal(out.hasMore, false);
  assert.ok(!("nextPageToken" in out));
});

test("output keys mirror the upstream's mapping", async () => {
  const drive = scopeWithDocs(1);
  const out = JSON.parse(
    await scopedSearchDriveFiles(drive, new SubtreeCache(() => drive), ["scope-F-0001"], { query: "report" }),
  );
  assert.deepEqual(Object.keys(out), ["files", "total", "hasMore"]);
  assert.deepEqual(Object.keys(out.files[0]), [
    "id", "name", "mimeType", "size", "modifiedTime", "createdTime", "owner", "url",
  ]);
});

test("escapeQ handles backslash then quote, like the upstream", () => {
  assert.equal(escapeQ("a\\' or name contains '"), "a\\\\\\' or name contains \\'");
});

test("an injection attempt stays inside its own condition", () => {
  const q = searchQuery(
    buildConditions({
      query: "a\\' or name contains '",
      searchIn: "name",
      mimeType: "x' or mimeType='y",
      orderBy: "name",
      sortDirection: "asc",
      maxResults: 10,
    }),
    ["f1"],
  );
  assert.equal(
    q,
    "(trashed=false) and (name contains 'a\\\\\\' or name contains \\'') and (mimeType='x\\' or mimeType=\\'y') and ('f1' in parents)",
  );
});

test("MIME shortcuts resolve as upstream does", () => {
  const conditions = buildConditions({
    query: "x", searchIn: "name", mimeType: "spreadsheet", orderBy: "name", sortDirection: "asc", maxResults: 1,
  });
  assert.ok(conditions.includes("mimeType='application/vnd.google-apps.spreadsheet'"));
});

test("an invalid modifiedAfter fails the way upstream does", async () => {
  const drive = scopeWithDocs(1);
  await assert.rejects(
    scopedSearchDriveFiles(drive, new SubtreeCache(() => drive), ["scope-F-0001"], {
      query: "x",
      modifiedAfter: "not a date",
    }),
    /Invalid time value/,
  );
});

test("the subtree includes the roots and follows pagination", async () => {
  const drive = new FakeDrive();
  drive.serverPageCap = 2;
  drive.folder("scope-F-0001", []);
  for (let i = 0; i < 5; i++) drive.folder(`child-${i}-xxxxx`, ["scope-F-0001"]);
  const folders = await new SubtreeCache(() => drive).folders(["scope-F-0001"], signal());
  assert.equal(folders.length, 6);
  assert.ok(folders.includes("scope-F-0001"));
  assert.ok(drive.listCalls.some((c) => c.pageToken));
});

test("120 folders over 3 levels takes at most 6 listing calls", async () => {
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", []);
  const level1 = Array.from({ length: 20 }, (_, i) => `l1-${i}-xxxxxxx`);
  for (const id of level1) drive.folder(id, ["scope-F-0001"]);
  level1.forEach((parent, i) => {
    for (let j = 0; j < 5; j++) drive.folder(`l2-${i}-${j}-xxxxx`, [parent]);
  });
  const folders = await new SubtreeCache(() => drive).folders(["scope-F-0001"], signal());
  assert.equal(folders.length, 121);
  assert.ok(drive.listCalls.length <= 6, `made ${drive.listCalls.length} calls`);
});

test("more folders than the cap is ScopeTooLarge", async () => {
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", []);
  for (let i = 0; i < 12; i++) drive.folder(`child-${i}-xxxxx`, ["scope-F-0001"]);
  await assert.rejects(
    new SubtreeCache(() => drive, { cap: 10 }).folders(["scope-F-0001"], signal()),
    ScopeTooLarge,
  );
});

test("41 folders are searched in 2 chunks", async () => {
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", []);
  for (let i = 0; i < 40; i++) drive.folder(`child-${String(i).padStart(2, "0")}-xxxx`, ["scope-F-0001"]);
  const subtree = new SubtreeCache(() => drive);
  await subtree.folders(["scope-F-0001"], signal());
  const before = drive.listCalls.length;
  await scopedSearchDriveFiles(drive, subtree, ["scope-F-0001"], { query: "x" });
  assert.equal(drive.listCalls.length - before, 2);
});

test("one failing chunk fails the whole search", async () => {
  const drive = scopeWithDocs(2);
  const subtree = new SubtreeCache(() => drive);
  await subtree.folders(["scope-F-0001"], signal());
  drive.listError = new Error("Drive HTTP 503");
  await assert.rejects(scopedSearchDriveFiles(drive, subtree, ["scope-F-0001"], { query: "x" }), /503/);
});

test("the time budget aborts a slow search", async () => {
  const drive = scopeWithDocs(2);
  drive.listDelayMs = 200;
  await assert.rejects(
    scopedSearchDriveFiles(drive, new SubtreeCache(() => drive), ["scope-F-0001"], { query: "x" }, 50),
  );
});

test("results merge, dedupe and sort across chunks", async () => {
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", []);
  drive.add("b-doc-xxxxxx", ["scope-F-0001"], { name: "beta" });
  drive.add("a-doc-xxxxxx", ["scope-F-0001"], { name: "alpha" });
  const out = JSON.parse(
    await scopedSearchDriveFiles(drive, new SubtreeCache(() => drive), ["scope-F-0001"], {
      query: "a",
      orderBy: "name",
      sortDirection: "asc",
    }),
  );
  assert.deepEqual(out.files.map((f: { name: string }) => f.name), ["alpha", "beta"]);
});

test("the subtree is cached and flushable", async () => {
  const drive = scopeWithDocs(1);
  const subtree = new SubtreeCache(() => drive);
  await subtree.folders(["scope-F-0001"], signal());
  const calls = drive.listCalls.length;
  await subtree.folders(["scope-F-0001"], signal());
  assert.equal(drive.listCalls.length, calls);
  subtree.flush();
  await subtree.folders(["scope-F-0001"], signal());
  assert.ok(drive.listCalls.length > calls);
});

test("an empty modifiedAfter is no filter, as upstream", () => {
  const conditions = buildConditions({
    query: "x", searchIn: "name", modifiedAfter: "", orderBy: "name", sortDirection: "asc", maxResults: 1,
  });
  assert.ok(!conditions.some((c) => c.startsWith("modifiedTime")));
});

test("an oversized scope is remembered, not re-walked", async () => {
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", []);
  for (let i = 0; i < 12; i++) drive.folder(`child-${i}-xxxxx`, ["scope-F-0001"]);
  const subtree = new SubtreeCache(() => drive, { cap: 10 });
  await assert.rejects(subtree.folders(["scope-F-0001"], signal()), ScopeTooLarge);
  const calls = drive.listCalls.length;
  await assert.rejects(subtree.folders(["scope-F-0001"], signal()), ScopeTooLarge);
  assert.equal(drive.listCalls.length, calls);
});

test("hasMore follows nextPageToken and incompleteSearch; duplicates collapse", async () => {
  const drive = scopeWithDocs(3);
  const subtree = new SubtreeCache(() => drive);
  const original = drive.listFiles.bind(drive);
  let mode: "token" | "incomplete" | "dup" = "token";
  drive.listFiles = async (p) => {
    const page = await original(p);
    if (!p.q.includes("contains")) return page;
    if (mode === "token") return { ...page, nextPageToken: "more" };
    if (mode === "incomplete") return { ...page, incompleteSearch: true } as typeof page;
    return { files: [...page.files, ...page.files] };
  };
  const run = async () =>
    JSON.parse(await scopedSearchDriveFiles(drive, subtree, ["scope-F-0001"], { query: "report", maxResults: 50 }));
  assert.equal((await run()).hasMore, true);
  mode = "incomplete";
  assert.equal((await run()).hasMore, true);
  mode = "dup";
  const deduped = await run();
  assert.equal(deduped.hasMore, false);
  assert.equal(deduped.files.length, 3);
});
