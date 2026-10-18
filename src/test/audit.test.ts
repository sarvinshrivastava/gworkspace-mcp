import { test } from "node:test";
import assert from "node:assert/strict";

import { Audit } from "../audit.js";
import type { PolicyStore } from "../policy/store.js";
import { quietly, tempStore } from "./helpers.js";

test("auth failures are deduplicated per source and reason for a minute", () => {
  const store = tempStore();
  let now = 1_000_000;
  const audit = new Audit(store, () => now);
  audit.authFailure("10.0.0.1");
  audit.authFailure("10.0.0.1");
  audit.authFailure("10.0.0.1", "login");
  audit.authFailure("10.0.0.2");
  assert.equal(store.queryAudit({ limit: 100 }).length, 3);
  now += 61_000;
  audit.authFailure("10.0.0.1");
  assert.equal(store.queryAudit({ limit: 100 }).length, 4);
});

test("tool and admin rows carry their kind and decision", () => {
  const store = tempStore();
  const audit = new Audit(store);
  audit.tool({ slug: "a1" }, "readDocument", "deny", "outside", ["doc-id-123456"]);
  audit.admin("grants.set", "a1", ["a1"], "127.0.0.1");
  const [admin, tool] = store.queryAudit({ limit: 10 });
  assert.equal(tool!.kind, "tool");
  assert.deepEqual(tool!.resources, ["doc-id-123456"]);
  assert.equal(admin!.kind, "admin");
  assert.equal(admin!.decision, "mutate");
});

test("prune removes only rows past retention", () => {
  const store = tempStore();
  let now = 0;
  const audit = new Audit(store, () => now);
  audit.tool({ slug: "a" }, "t", "allow", "", []);
  now = 100 * 24 * 60 * 60 * 1000;
  audit.tool({ slug: "a" }, "t", "allow", "", []);
  assert.equal(audit.prune(90), 1);
  assert.equal(store.queryAudit({ limit: 10 }).length, 1);
  assert.equal(audit.prune(90), 0);
});

test("a failing store is counted, never thrown", async () => {
  const broken = {
    insertAudit() {
      throw new Error("disk full");
    },
    pruneAudit() {
      throw new Error("disk full");
    },
  } as unknown as PolicyStore;
  const audit = new Audit(broken);
  await quietly(() => {
    audit.tool({ slug: "a" }, "t", "allow", "", []);
    assert.equal(audit.prune(), 0);
  });
  assert.equal(audit.errors, 2);
});
