import { test } from "node:test";
import assert from "node:assert/strict";

import { makeBroadcaster, notifierFrom, type Notifier } from "../notify.js";
import { quietly } from "./helpers.js";

const notice = { title: "gworkspace-mcp: Google access rejected", message: "msg" };
const TOPIC = "s3cret-topic-123";
const settle = () => new Promise((resolve) => setImmediate(resolve));

type Seen = { url: string; init?: RequestInit };
const recording = (answer: () => Response | Promise<Response>) => {
  const seen: Seen[] = [];
  const impl = (async (input: string | URL | Request, init?: RequestInit) => {
    seen.push({ url: String(input), init });
    return answer();
  }) as typeof fetch;
  return { seen, impl };
};

test("an empty, invalid or insecure setup returns null, says why and never logs the topic", async () => {
  const cases: Array<[string, string, string]> = [
    ["", "https://ntfy.sh", "ntfy not configured"],
    ["   ", "https://ntfy.sh", "ntfy not configured"],
    ["short", "https://ntfy.sh", "invalid topic"],
    ["has spaces in it!", "https://ntfy.sh", "invalid topic"],
    [TOPIC, "not a url", "invalid base"],
    [TOPIC, "http://ntfy.sh", "invalid base"],
    [TOPIC, "ftp://ntfy.sh", "invalid base"],
  ];
  for (const [topic, base, why] of cases) {
    const { result, logs } = await quietly(() => notifierFrom(topic, base));
    assert.equal(result, null, `${topic} ${base}`);
    assert.ok(logs.join("\n").includes(why), why);
    assert.ok(!logs.join("\n").includes(TOPIC));
  }
  assert.ok(notifierFrom(TOPIC, "http://localhost:8080"));
  assert.ok(notifierFrom(TOPIC, "http://127.0.0.1:8080"));
});

test("posts the fixed notice to base/topic with the ntfy headers; a trailing newline in the topic is trimmed", async () => {
  const { seen, impl } = recording(() => new Response("{}", { status: 200 }));
  const send = notifierFrom(`${TOPIC}\n`, "https://ntfy.sh/", impl)!;
  assert.equal(await send(notice), true);
  assert.equal(seen[0]!.url, `https://ntfy.sh/${TOPIC}`);
  assert.equal(seen[0]!.init?.method, "POST");
  assert.equal(seen[0]!.init?.body, "msg");
  assert.deepEqual(seen[0]!.init?.headers, {
    Title: "gworkspace-mcp: Google access rejected",
    Priority: "high",
    Tags: "warning",
    "Content-Type": "text/plain; charset=utf-8",
  });
});

test("a non-2xx answer, a throw and a hang all resolve false and log neither the topic nor the URL", async () => {
  const bad = recording(() => new Response("no", { status: 500 }));
  const a = await quietly(() => notifierFrom(TOPIC, "https://ntfy.sh", bad.impl)!(notice));
  assert.equal(a.result, false);
  assert.deepEqual(a.logs, ["[alert] ntfy push failed: HTTP 500"]);

  const throwing = recording(() => {
    throw new TypeError(`fetch failed for https://ntfy.sh/${TOPIC}`);
  });
  const b = await quietly(() => notifierFrom(TOPIC, "https://ntfy.sh", throwing.impl)!(notice));
  assert.equal(b.result, false);
  assert.deepEqual(b.logs, ["[alert] ntfy push failed: TypeError"]);

  const hanging = (async (_url: unknown, init?: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(init.signal!.reason));
    })) as typeof fetch;
  const c = await quietly(() => notifierFrom(TOPIC, "https://ntfy.sh", hanging, 20)!(notice));
  assert.equal(c.result, false);
  assert.ok(c.logs[0]!.startsWith("[alert] ntfy push failed:"));
  assert.ok(!c.logs.join("\n").includes(TOPIC));
});

test("a response whose body.cancel rejects causes no unhandled rejection", async () => {
  const unhandled: unknown[] = [];
  const onUnhandled = (reason: unknown) => unhandled.push(reason);
  process.on("unhandledRejection", onUnhandled);
  try {
    const impl = (async () => ({
      ok: true,
      status: 200,
      body: { cancel: () => Promise.reject(new Error("cancel failed")) },
    })) as unknown as typeof fetch;
    assert.equal(await notifierFrom(TOPIC, "https://ntfy.sh", impl)!(notice), true);
    await settle();
    await settle();
  } finally {
    process.off("unhandledRejection", onUnhandled);
  }
  assert.deepEqual(unhandled, []);
});

function rig(deliver: (call: number) => Promise<boolean> | boolean | never) {
  let now = 0;
  let calls = 0;
  const notifier: Notifier = async () => deliver(++calls) as boolean;
  const broadcast = makeBroadcaster(notifier, { now: () => now });
  return { broadcast, calls: () => calls, at: (t: number) => (now = t) };
}

test("the first push at clock 0 is sent; calls inside the gap after a delivered push are dropped", async () => {
  const r = rig(() => true);
  for (let i = 0; i < 10; i++) r.broadcast("rejected", notice);
  await settle();
  assert.equal(r.calls(), 1);
  r.at(1_799_999);
  r.broadcast("rejected", notice);
  await settle();
  assert.equal(r.calls(), 1);
  r.at(1_800_000);
  r.broadcast("rejected", notice);
  await settle();
  assert.equal(r.calls(), 2);
});

test("a call while a send is in flight does not double-send", async () => {
  let release!: (v: boolean) => void;
  const r = rig(() => new Promise<boolean>((resolve) => (release = resolve)));
  r.broadcast("failing", notice);
  r.at(120_000);
  r.broadcast("failing", notice);
  await settle();
  assert.equal(r.calls(), 1);
  release(true);
  await settle();
  r.at(121_000);
  r.broadcast("failing", notice);
  await settle();
  assert.equal(r.calls(), 1, "delivered, so the gap now holds");
});

test("an undelivered push is retried on the next call after 60s, never inside it", async () => {
  const r = rig((call) => call >= 3);
  r.broadcast("rejected", notice);
  await settle();
  for (const t of [1, 30_000, 59_999]) {
    r.at(t);
    r.broadcast("rejected", notice);
  }
  await settle();
  assert.equal(r.calls(), 1, "the 60s floor holds");
  r.at(60_000);
  r.broadcast("rejected", notice);
  await settle();
  assert.equal(r.calls(), 2);
  r.at(120_000);
  r.broadcast("rejected", notice);
  await settle();
  assert.equal(r.calls(), 3, "still undelivered, so retried");
});

test("the gap starts at the settle-time clock of a delivered push", async () => {
  let now = 0;
  let calls = 0;
  let resolveSend!: (v: boolean) => void;
  const notifier: Notifier = () => {
    calls++;
    return new Promise<boolean>((resolve) => (resolveSend = resolve));
  };
  const broadcast = makeBroadcaster(notifier, { now: () => now });
  broadcast("rejected", notice);
  now = 500;
  resolveSend(true);
  await settle();
  now = 1_800_499;
  broadcast("rejected", notice);
  assert.equal(calls, 1);
  now = 1_800_500;
  broadcast("rejected", notice);
  assert.equal(calls, 2);
});

test("attempt at 0 fails, 59,999 is blocked, 60,000 attempts and delivers at 60,500, blocked until 1,860,500", async () => {
  let now = 0;
  let calls = 0;
  const outcomes = [false, true, true];
  const notifier: Notifier = async () => {
    const delivered = outcomes[calls++]!;
    now += calls === 2 ? 500 : 0;
    return delivered;
  };
  const broadcast = makeBroadcaster(notifier, { now: () => now });
  broadcast("rejected", notice);
  await settle();
  now = 59_999;
  broadcast("rejected", notice);
  assert.equal(calls, 1);
  now = 60_000;
  broadcast("rejected", notice);
  await settle();
  assert.equal(calls, 2);
  assert.equal(now, 60_500);
  now = 1_860_499;
  broadcast("rejected", notice);
  assert.equal(calls, 2);
  now = 1_860_500;
  broadcast("rejected", notice);
  await settle();
  assert.equal(calls, 3);
});

test("a fast-failing notifier called ten times inside a minute makes one attempt", async () => {
  const r = rig(() => false);
  for (let i = 0; i < 10; i++) {
    r.at(i * 1000);
    r.broadcast("rejected", notice);
    await settle();
  }
  assert.equal(r.calls(), 1);
});

test("kinds are independent", async () => {
  const r = rig(() => true);
  r.broadcast("rejected", notice);
  r.broadcast("failing", notice);
  r.broadcast("probe", notice);
  r.broadcast("upstream_down", notice);
  await settle();
  assert.equal(r.calls(), 4);
});

test("a notifier that throws or rejects never throws or leaves an unhandled rejection, and the next call retries", async () => {
  const unhandled: unknown[] = [];
  const onUnhandled = (reason: unknown) => unhandled.push(reason);
  process.on("unhandledRejection", onUnhandled);
  try {
    let now = 0;
    let calls = 0;
    const notifier = (() => {
      calls++;
      if (calls === 1) throw new Error("sync");
      if (calls === 2) return Promise.reject(new Error("async"));
      return Promise.resolve(true);
    }) as Notifier;
    const broadcast = makeBroadcaster(notifier, { now: () => now });
    broadcast("rejected", notice);
    await settle();
    now = 60_000;
    broadcast("rejected", notice);
    await settle();
    now = 120_000;
    broadcast("rejected", notice);
    await settle();
    await settle();
    assert.equal(calls, 3);
  } finally {
    process.off("unhandledRejection", onUnhandled);
  }
  assert.deepEqual(unhandled, []);
});

test("no notifier means nothing happens", () => {
  makeBroadcaster(null)("rejected", notice);
});
