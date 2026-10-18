import { test, after } from "node:test";
import assert from "node:assert/strict";
import type { Server } from "node:http";
import express from "express";

import { internalGuard } from "../internal-auth.js";
import { listen, quietly } from "./helpers.js";

const servers: Server[] = [];
after(() => servers.forEach((s) => s.close()));

async function guarded(now: () => number) {
  const app = express();
  app.use("/internal", internalGuard(() => "t".repeat(32), { now }), (_req, res) => {
    res.json({ ok: true });
  });
  const { url, server } = await listen(app);
  servers.push(server);
  return (bearer: string) =>
    fetch(`${url}/internal/x`, { method: "POST", headers: { Authorization: `Bearer ${bearer}` } });
}

test("the limit resets after the window, and Retry-After says when", async () => {
  let now = 1_000_000;
  const call = await guarded(() => now);
  await quietly(async () => {
    for (let i = 0; i < 10; i++) assert.equal((await call("wrong")).status, 401);
  });
  now += 15_000;
  const limited = await call("t".repeat(32));
  assert.equal(limited.status, 429);
  assert.equal(limited.headers.get("retry-after"), "45");

  now += 45_000;
  assert.equal((await call("t".repeat(32))).status, 200);
});

test("the once-a-minute failure log comes back after the window", async () => {
  let now = 1_000_000;
  const call = await guarded(() => now);
  const { logs } = await quietly(async () => {
    await call("wrong");
    await call("wrong");
    now += 60_000;
    await call("wrong");
  });
  assert.equal(logs.filter((l) => l.includes("[internal] rejected POST /internal/x")).length, 2);
});
