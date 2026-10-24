import { test } from "node:test";
import assert from "node:assert/strict";

import { GoogleHealth } from "../drive/health.js";
import { wireGoogle } from "../google.js";
import { creds, fakeFetch, json, quietly, tokenOk } from "./helpers.js";

const TOPIC = "wire-topic-1234";
const settle = () => new Promise((resolve) => setImmediate(resolve));

function wired() {
  const google = new GoogleHealth();
  const { impl, calls } = fakeFetch((url) => {
    if (url.includes("oauth2")) return tokenOk();
    if (url.startsWith("https://ntfy.test")) return new Response("{}", { status: 200 });
    return json(200, { id: "f", name: "f", mimeType: "x", parents: [], trashed: false });
  });
  const { drive, onExit } = wireGoogle(google, {
    creds,
    ntfyTopic: TOPIC,
    ntfyBase: "https://ntfy.test",
    fetchImpl: impl,
  });
  const pushes = () => calls.filter((c) => c.url === `https://ntfy.test/${TOPIC}`);
  return { google, drive, onExit, pushes };
}

test("a Drive answer on the shared client reaches the tracker", async () => {
  const { google, drive } = wired();
  await drive.getFile("a1234567890");
  assert.equal(google.snapshot().state, "ok");
});

test("a child exit sends an upstream_down push to base/topic", async () => {
  const { onExit, pushes } = wired();
  await quietly(() => onExit());
  await settle();
  assert.equal(pushes().length, 1);
  assert.equal((pushes()[0]!.init?.headers as Record<string, string>).Title, "gworkspace-mcp: Workspace server exited");
});

test("a failure goes out through the broadcaster built from the topic", async () => {
  const { google, pushes } = wired();
  await quietly(() => google.failure("rejected", "invalid_grant", "x"));
  await settle();
  assert.equal(pushes().length, 1);
  assert.match(String(pushes()[0]!.init?.body), /\(invalid_grant\)/);
});

test("no topic wires a log-only tracker", async () => {
  const google = new GoogleHealth();
  const { impl, calls } = fakeFetch(() => new Response("{}", { status: 200 }));
  await quietly(() => wireGoogle(google, { creds, ntfyTopic: "", ntfyBase: "https://ntfy.test", fetchImpl: impl }));
  await quietly(() => google.failure("rejected", "invalid_grant", "x"));
  await settle();
  assert.equal(calls.length, 0);
});
