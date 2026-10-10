import { test } from "node:test";
import assert from "node:assert/strict";

import { planCall, type Plan, type PipelineDeps } from "../enforce/pipeline.js";
import { quickAddGuard, stripAttendees } from "../enforce/guards.js";
import { ScopeResolver } from "../enforce/resolver.js";
import { SubtreeCache } from "../enforce/search.js";
import { DEFAULT_GRANTS } from "../policy/derive.js";
import type { AgentContext } from "../policy/auth.js";
import { FakeDrive, quietly, retryable } from "./helpers.js";

function setup() {
  const drive = new FakeDrive();
  drive.folder("root-folder1", []);
  drive.folder("scope-F-0001", ["root-folder1"]);
  drive.folder("sub-folder-1", ["scope-F-0001"]);
  drive.add("doc-in-F-001", ["scope-F-0001"], { name: "inside" });
  drive.add("sheet-in-F-1", ["sub-folder-1"], { mimeType: "application/vnd.google-apps.spreadsheet" });
  drive.folder("other-folder", ["root-folder1"]);
  drive.add("doc-outside1", ["other-folder"]);
  const subtree = new SubtreeCache(() => drive);
  const deps: PipelineDeps = {
    resolver: new ScopeResolver(() => drive),
    subtree,
    drive: () => drive,
  };
  return { drive, deps, subtree };
}

const confined = (overrides: Partial<AgentContext> = {}): AgentContext => ({
  slug: "agent-1",
  scoped: true,
  grants: DEFAULT_GRANTS,
  scopes: [{ folderId: "scope-F-0001", folderName: "F", isPrimary: true }],
  ...overrides,
});
const unconfined: AgentContext = { slug: "default", scoped: false, grants: DEFAULT_GRANTS, scopes: [] };

const reasonOf = (p: Plan) => (p.kind === "deny" ? p.reason : p.kind);
const argsOf = (p: Plan) => {
  assert.equal(p.kind, "relay");
  return (p as Extract<Plan, { kind: "relay" }>).args;
};

test("stripAttendees: mutated only when something changed", async () => {
  await quietly(() => {
    assert.equal((stripAttendees({ summary: "x" }) as { mutated: boolean }).mutated, false);
    assert.equal((stripAttendees({ summary: "x", sendUpdates: "none" }) as { mutated: boolean }).mutated, false);
    assert.equal((stripAttendees({ attendees: [] }) as { mutated: boolean }).mutated, true);
    assert.equal((stripAttendees({ sendUpdates: "all" }) as { mutated: boolean }).mutated, true);
  });
});

test("quickAddGuard: forces none; not mutated when omitted", () => {
  const result = quickAddGuard({ text: "Lunch tomorrow at noon" });
  assert.deepEqual(result, { args: { text: "Lunch tomorrow at noon", sendUpdates: "none" }, mutated: false });
});

test("quickAdd with a guest e-mail is denied for the unconfined default agent", async () => {
  const { deps } = setup();
  const plan = await planCall(unconfined, "quickAddEvent", { text: "lunch with a@b.co" }, deps);
  assert.equal(reasonOf(plan), "quickadd_guest");
});

test("unconfined agents relay untouched", async () => {
  const { deps, drive } = setup();
  const args = { documentId: "doc-outside1" };
  const plan = await planCall(unconfined, "readDocument", args, deps);
  assert.deepEqual(argsOf(plan), args);
  assert.equal(drive.getCalls.length, 0);
});

test("unconfined createEvent still loses attendees", async () => {
  const { deps } = setup();
  const plan = await quietly(() => planCall(unconfined, "createEvent", { summary: "s", attendees: [{ email: "x@y.z" }] }, deps));
  assert.deepEqual(argsOf(plan.result), { summary: "s", sendUpdates: "none" });
  assert.equal((plan.result as { mutated: boolean }).mutated, true);
});

test("a confined agent with no folder: Drive tools denied, calendar allowed", async () => {
  const { deps } = setup();
  const ctx = confined({ scopes: [] });
  assert.equal(reasonOf(await planCall(ctx, "readDocument", { documentId: "doc-in-F-001" }, deps)), "no_scope");
  assert.equal(reasonOf(await planCall(ctx, "listEvents", {}, deps)), "relay");
});

test("reads inside are relayed, outside denied, bad IDs refused", async () => {
  const { deps } = setup();
  assert.equal(reasonOf(await planCall(confined(), "readDocument", { documentId: "doc-in-F-001" }, deps)), "relay");
  const out = await planCall(confined(), "readDocument", { documentId: "doc-outside1" }, deps);
  assert.equal(reasonOf(out), "outside");
  assert.match((out as { message: string }).message, /not found or is outside/);
  assert.equal(reasonOf(await planCall(confined(), "readDocument", { documentId: "../etc" }, deps)), "bad_id");
  assert.equal(reasonOf(await planCall(confined(), "readDocument", {}, deps)), "bad_id");
});

test("reads use the cache, writes resolve live", async () => {
  const { deps, drive } = setup();
  await planCall(confined(), "readDocument", { documentId: "doc-in-F-001" }, deps);
  const afterFirst = drive.getCalls.length;
  await planCall(confined(), "readDocument", { documentId: "doc-in-F-001" }, deps);
  assert.equal(drive.getCalls.length, afterFirst);
  await planCall(confined(), "appendText", { documentId: "doc-in-F-001", text: "x" }, deps);
  assert.ok(drive.getCalls.length > afterFirst);
});

test("sheet writes are forced to RAW", async () => {
  const { deps } = setup();
  const omitted = await planCall(confined(), "writeSpreadsheet", { spreadsheetId: "sheet-in-F-1", range: "A1", values: [["=IMPORTRANGE(1)"]] }, deps);
  assert.equal(argsOf(omitted).valueInputOption, "RAW");
  assert.equal((omitted as { mutated: boolean }).mutated, true);
  const explicit = await planCall(confined(), "appendRows", { spreadsheetId: "sheet-in-F-1", range: "A1", values: [], valueInputOption: "USER_ENTERED" }, deps);
  assert.equal(argsOf(explicit).valueInputOption, "RAW");
});

test("create without a parent lands in the primary", async () => {
  const { deps } = setup();
  const plan = await planCall(confined(), "createDocument", { title: "t" }, deps);
  assert.equal(argsOf(plan).parentFolderId, "scope-F-0001");
  assert.equal((plan as { mutated: boolean }).mutated, true);
});

test("create with an in-scope parent folder is honoured", async () => {
  const { deps } = setup();
  const plan = await planCall(confined(), "createFolder", { name: "n", parentFolderId: "sub-folder-1" }, deps);
  assert.equal(argsOf(plan).parentFolderId, "sub-folder-1");
});

test("create parents: outside, a file, empty, or no primary are denied", async () => {
  const { deps } = setup();
  assert.equal(reasonOf(await planCall(confined(), "createDocument", { title: "t", parentFolderId: "other-folder" }, deps)), "outside");
  assert.equal(reasonOf(await planCall(confined(), "createDocument", { title: "t", parentFolderId: "doc-in-F-001" }, deps)), "not_folder");
  assert.equal(reasonOf(await planCall(confined(), "createDocument", { title: "t", parentFolderId: "" }, deps)), "bad_id");
  const noPrimary = confined({ scopes: [{ folderId: "scope-F-0001", folderName: "F", isPrimary: false }] });
  assert.equal(reasonOf(await planCall(noPrimary, "createDocument", { title: "t" }, deps)), "no_primary");
});

test("a trashed primary blocks creates and default listings", async () => {
  const { deps, drive } = setup();
  drive.files.get("scope-F-0001")!.trashed = true;
  assert.equal(reasonOf(await planCall(confined(), "createDocument", { title: "t" }, deps)), "primary_unavailable");
  assert.equal(reasonOf(await planCall(confined(), "listDriveFiles", {}, deps)), "primary_unavailable");
});

test("createSpreadsheet with initialData is refused", async () => {
  const { deps } = setup();
  assert.equal(reasonOf(await planCall(confined(), "createSpreadsheet", { title: "t", initialData: [["1"]] }, deps)), "initial_data");
  assert.equal(reasonOf(await planCall(confined(), "createSpreadsheet", { title: "t" }, deps)), "relay");
});

test("createFolder relays with an after hook that flushes the subtree", async () => {
  const { deps, subtree, drive } = setup();
  await subtree.folders(["scope-F-0001"], AbortSignal.timeout(1000));
  const plan = await planCall(confined(), "createFolder", { name: "n" }, deps);
  assert.equal(plan.kind, "relay");
  const after = (plan as { after?: () => void }).after;
  assert.ok(after);
  const calls = drive.listCalls.length;
  after();
  await subtree.folders(["scope-F-0001"], AbortSignal.timeout(1000));
  assert.ok(drive.listCalls.length > calls);
});

test("listDriveFiles: omitted → primary; root, outside and empty denied", async () => {
  const { deps } = setup();
  assert.equal(argsOf(await planCall(confined(), "listDriveFiles", {}, deps)).folderId, "scope-F-0001");
  assert.equal(argsOf(await planCall(confined(), "listDriveFiles", { folderId: "sub-folder-1" }, deps)).folderId, "sub-folder-1");
  assert.equal(reasonOf(await planCall(confined(), "listDriveFiles", { folderId: "root" }, deps)), "bad_id");
  assert.equal(reasonOf(await planCall(confined(), "listDriveFiles", { folderId: "" }, deps)), "bad_id");
  assert.equal(reasonOf(await planCall(confined(), "listDriveFiles", { folderId: "other-folder" }, deps)), "outside");
});

test("searchDriveFiles: served, pageToken refused, outside folder denied", async () => {
  const { deps } = setup();
  const served = await planCall(confined(), "searchDriveFiles", { query: "inside" }, deps);
  assert.equal(served.kind, "serve");
  const out = JSON.parse(await (served as { run: () => Promise<string> }).run());
  assert.deepEqual(out.files.map((f: { id: string }) => f.id), ["doc-in-F-001"]);
  assert.equal(reasonOf(await planCall(confined(), "searchDriveFiles", { query: "x", pageToken: "t" }, deps)), "pagination");
  assert.equal(reasonOf(await planCall(confined(), "searchDriveFiles", { query: "x", folderId: "other-folder" }, deps)), "outside");
});

test("searchDriveFiles skips trashed roots; all trashed is denied", async () => {
  const { deps, drive } = setup();
  drive.folder("scope-G-0001", ["root-folder1"], { trashed: true });
  const two = confined({
    scopes: [
      { folderId: "scope-F-0001", folderName: "F", isPrimary: true },
      { folderId: "scope-G-0001", folderName: "G", isPrimary: false },
    ],
  });
  const plan = await planCall(two, "searchDriveFiles", { query: "x" }, deps);
  assert.deepEqual((plan as { resources: string[] }).resources, ["scope-F-0001"]);
  drive.files.get("scope-F-0001")!.trashed = true;
  deps.resolver.flush();
  assert.equal(reasonOf(await planCall(two, "searchDriveFiles", { query: "x" }, deps)), "primary_unavailable");
});

test("withheld list tools are refused if they ever reach the pipeline", async () => {
  const { deps } = setup();
  assert.equal(reasonOf(await planCall(confined(), "listDocuments", {}, deps)), "unmapped");
});

test("a Drive outage on one scope root fails the search as drive_error, not partial", async () => {
  const { deps, drive } = setup();
  drive.folder("scope-G-0001", ["root-folder1"]);
  drive.errors.set("scope-G-0001", retryable());
  const two = confined({
    scopes: [
      { folderId: "scope-F-0001", folderName: "F", isPrimary: true },
      { folderId: "scope-G-0001", folderName: "G", isPrimary: false },
    ],
  });
  assert.equal(reasonOf(await planCall(two, "searchDriveFiles", { query: "x" }, deps)), "drive_error");
});

test("a Drive outage on the primary is drive_error, not 'folder missing'", async () => {
  const { deps, drive } = setup();
  drive.errors.set("scope-F-0001", retryable());
  assert.equal(reasonOf(await planCall(confined(), "createDocument", { title: "t" }, deps)), "drive_error");
  assert.equal(reasonOf(await planCall(confined(), "listDriveFiles", {}, deps)), "drive_error");
});

test("the primary is checked live for creates", async () => {
  const { deps, drive } = setup();
  await planCall(confined(), "listDriveFiles", {}, deps); // warms the cache
  drive.files.get("scope-F-0001")!.trashed = true;
  assert.equal(reasonOf(await planCall(confined(), "createDocument", { title: "t" }, deps)), "primary_unavailable");
});

test("unconfined: createFolder flushes the subtree; reads name their file", async () => {
  const { deps } = setup();
  const folder = await planCall(unconfined, "createFolder", { name: "n" }, deps);
  assert.ok((folder as { after?: () => void }).after);
  const read = await planCall(unconfined, "readDocument", { documentId: "doc-outside1" }, deps);
  assert.deepEqual((read as { resources: string[] }).resources, ["doc-outside1"]);
});
