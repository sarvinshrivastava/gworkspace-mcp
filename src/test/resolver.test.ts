import { test } from "node:test";
import assert from "node:assert/strict";

import { ScopeResolver } from "../enforce/resolver.js";
import { FakeDrive, forbidden, oauthRejected, retryable, serviceRejected } from "./helpers.js";

/** root ─ F (scope) ─ sub ─ doc ; root ─ other ─ outsideDoc */
function tree() {
  const drive = new FakeDrive();
  drive.folder("root-id-0001", []);
  drive.folder("scope-F-0001", ["root-id-0001"]);
  drive.folder("sub-folder-1", ["scope-F-0001"]);
  drive.add("doc-in-sub-1", ["sub-folder-1"]);
  drive.add("doc-in-F-001", ["scope-F-0001"]);
  drive.folder("other-folder", ["root-id-0001"]);
  drive.add("doc-outside1", ["other-folder"]);
  return drive;
}

const F = new Set(["scope-F-0001"]);

test("children and grandchildren are in scope", async () => {
  const r = new ScopeResolver(() => tree());
  assert.equal((await r.check("doc-in-F-001", F, false)).ok, true);
  assert.equal((await r.check("doc-in-sub-1", F, false)).ok, true);
});

test("a file elsewhere is outside", async () => {
  const r = new ScopeResolver(() => tree());
  assert.deepEqual(await r.check("doc-outside1", F, false), { ok: false, reason: "outside" });
});

test("the scope folder itself is in scope, unless trashed", async () => {
  const drive = tree();
  const r = new ScopeResolver(() => drive);
  assert.equal((await r.check("scope-F-0001", F, false)).ok, true);
  drive.files.get("scope-F-0001")!.trashed = true;
  assert.deepEqual(await r.check("scope-F-0001", F, true), { ok: false, reason: "trashed" });
  assert.deepEqual(await r.check("doc-in-sub-1", F, true), { ok: false, reason: "trashed" });
});

test("a trashed target is denied", async () => {
  const drive = tree();
  drive.files.get("doc-in-F-001")!.trashed = true;
  const r = new ScopeResolver(() => drive);
  assert.deepEqual(await r.check("doc-in-F-001", F, false), { ok: false, reason: "trashed" });
});

test("a shared-with-me file with no parents is denied", async () => {
  const drive = tree();
  drive.add("shared-doc-1", []);
  const r = new ScopeResolver(() => drive);
  assert.deepEqual(await r.check("shared-doc-1", F, false), { ok: false, reason: "no_parents" });
});

test("404 and non-rate-limit 403 are not_found", async () => {
  const drive = tree();
  drive.errors.set("doc-forbid-1", forbidden());
  const r = new ScopeResolver(() => drive);
  assert.deepEqual(await r.check("missing-doc1", F, false), { ok: false, reason: "not_found" });
  assert.deepEqual(await r.check("doc-forbid-1", F, false), { ok: false, reason: "not_found" });
});

test("a retryable error is drive_error and is not cached", async () => {
  const drive = tree();
  drive.errors.set("doc-in-F-001", retryable());
  const r = new ScopeResolver(() => drive);
  assert.equal((await r.check("doc-in-F-001", F, false) as { reason: string }).reason, "drive_error");
  drive.errors.delete("doc-in-F-001");
  assert.equal((await r.check("doc-in-F-001", F, false)).ok, true);
});

test("a retryable error on an ancestor is drive_error, not outside", async () => {
  const drive = tree();
  drive.errors.set("sub-folder-1", retryable());
  const r = new ScopeResolver(() => drive);
  assert.equal((await r.check("doc-in-sub-1", F, false) as { reason: string }).reason, "drive_error");
});

test("an ancestor we cannot see ends the path as outside", async () => {
  const drive = tree();
  drive.errors.set("sub-folder-1", forbidden());
  const r = new ScopeResolver(() => drive);
  assert.deepEqual(await r.check("doc-in-sub-1", F, false), { ok: false, reason: "outside" });
});

test("no Drive client yet is drive_error", async () => {
  const r = new ScopeResolver(() => null);
  assert.equal((await r.check("doc-in-F-001", F, false) as { reason: string }).reason, "drive_error");
});

test("a parent cycle terminates", async () => {
  const drive = new FakeDrive();
  drive.folder("cycle-a-0001", ["cycle-b-0001"]);
  drive.folder("cycle-b-0001", ["cycle-a-0001"]);
  drive.add("doc-cycle-01", ["cycle-a-0001"]);
  const r = new ScopeResolver(() => drive);
  assert.deepEqual(await r.check("doc-cycle-01", F, false), { ok: false, reason: "outside" });
});

test("deeper than 32 levels is too_deep", async () => {
  const drive = new FakeDrive();
  drive.folder("scope-F-0001", []);
  let parent = "scope-F-0001";
  for (let i = 0; i < 40; i++) {
    const id = `level-${String(i).padStart(6, "0")}`;
    drive.folder(id, [parent]);
    parent = id;
  }
  drive.add("deep-doc-001", [parent]);
  const r = new ScopeResolver(() => drive);
  assert.deepEqual(await r.check("deep-doc-001", F, false), { ok: false, reason: "too_deep" });
});

test("reads use the cache within the TTL; live refetches every level", async () => {
  let now = 0;
  const drive = tree();
  const r = new ScopeResolver(() => drive, { now: () => now });
  await r.check("doc-in-sub-1", F, false);
  const first = drive.getCalls.length;
  await r.check("doc-in-sub-1", F, false);
  assert.equal(drive.getCalls.length, first);
  await r.check("doc-in-sub-1", F, true);
  assert.equal(drive.getCalls.length, first * 2);
  now += 300_001;
  await r.check("doc-in-sub-1", F, false);
  assert.equal(drive.getCalls.length, first * 3);
});

test("a folder moved out: cached reads still pass, live writes are denied", async () => {
  const drive = tree();
  const r = new ScopeResolver(() => drive);
  assert.equal((await r.check("doc-in-sub-1", F, false)).ok, true);
  drive.files.get("sub-folder-1")!.parents = ["other-folder"];
  assert.equal((await r.check("doc-in-sub-1", F, false)).ok, true);
  assert.deepEqual(await r.check("doc-in-sub-1", F, true), { ok: false, reason: "outside" });
});

test("flush empties the cache", async () => {
  const drive = tree();
  const r = new ScopeResolver(() => drive);
  await r.check("doc-in-F-001", F, false);
  const before = drive.getCalls.length;
  r.flush();
  await r.check("doc-in-F-001", F, false);
  assert.ok(drive.getCalls.length > before);
});

test("the cache evicts least-recently-used entries", async () => {
  const drive = tree();
  const r = new ScopeResolver(() => drive, { max: 2 });
  await r.node("doc-in-F-001", false);
  await r.node("doc-in-sub-1", false);
  await r.node("doc-in-F-001", false); // refresh recency
  await r.node("doc-outside1", false); // evicts doc-in-sub-1
  const before = drive.getCalls.length;
  await r.node("doc-in-F-001", false);
  assert.equal(drive.getCalls.length, before);
  await r.node("doc-in-sub-1", false);
  assert.equal(drive.getCalls.length, before + 1);
});

test("E8: one cache serves agents with different scopes without leaking verdicts", async () => {
  const drive = tree();
  const r = new ScopeResolver(() => drive);
  const other = new Set(["other-folder"]);
  assert.equal((await r.check("doc-in-F-001", F, false)).ok, true);
  assert.deepEqual(await r.check("doc-in-F-001", other, false), { ok: false, reason: "outside" });
  assert.equal((await r.check("doc-outside1", other, false)).ok, true);
  assert.deepEqual(await r.check("doc-outside1", F, false), { ok: false, reason: "outside" });
});

test("a rejection on the target or on a parent is credential_rejected with its category, never not_found", async () => {
  for (const error of [oauthRejected(), oauthRejected("HTTP 403", 403), serviceRejected("drive")]) {
    for (const failing of ["doc-in-sub-1", "sub-folder-1"]) {
      const drive = tree();
      drive.errors.set(failing, error);
      const verdict = await new ScopeResolver(() => drive).check("doc-in-sub-1", F, true);
      assert.equal(verdict.ok, false);
      assert.equal((verdict as { reason: string }).reason, "credential_rejected", failing);
      const category = (verdict as { category: string }).category;
      assert.equal(category, error.source === "oauth" ? error.rejection : `drive ${error.rejection}`);
    }
  }
});

test("a transient failure keeps drive_error with its category; a plain 403 and a 404 stay not_found", async () => {
  const drive = tree();
  drive.errors.set("doc-in-F-001", retryable());
  assert.deepEqual(await new ScopeResolver(() => drive).check("doc-in-F-001", F, true), {
    ok: false,
    reason: "drive_error",
    category: "HTTP 503",
    detail: "Drive HTTP 503",
  });
  const plain = tree();
  plain.errors.set("doc-in-F-001", forbidden());
  assert.deepEqual(await new ScopeResolver(() => plain).check("doc-in-F-001", F, true), { ok: false, reason: "not_found" });
});

test("a calendar-source rejection is never a definite no and never a credential rejection", async () => {
  const drive = tree();
  drive.errors.set("doc-in-F-001", serviceRejected("calendar"));
  const verdict = await new ScopeResolver(() => drive).check("doc-in-F-001", F, true);
  assert.equal((verdict as { reason: string }).reason, "drive_error");
});

test("no Drive client gives drive_error with category unavailable", async () => {
  const verdict = await new ScopeResolver(() => null).check("doc-in-F-001", F, true);
  assert.deepEqual(verdict, { ok: false, reason: "drive_error", category: "unavailable", detail: "not ready" });
});
