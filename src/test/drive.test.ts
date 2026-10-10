import { test } from "node:test";
import assert from "node:assert/strict";

import { DriveClient, DriveError } from "../drive/client.js";

type Call = { url: string; init?: RequestInit };

/** A fetch that answers from a handler and records every call. */
function fakeFetch(handler: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const calls: Call[] = [];
  const impl = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    calls.push({ url, init });
    return handler(url, init);
  }) as typeof fetch;
  return { impl, calls };
}

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const creds = { clientId: "cid", clientSecret: "csecret", refreshToken: "1//rt" };
const tokenOk = () => json(200, { access_token: "at-1", expires_in: 3600 });
const file = { id: "file-id-12345", name: "F", mimeType: "x", parents: ["p1"], trashed: false };

test("concurrent calls share one token refresh, and the token is cached", async () => {
  const { impl, calls } = fakeFetch((url) => (url.includes("oauth2") ? tokenOk() : json(200, file)));
  const drive = new DriveClient(creds, impl);
  await Promise.all([drive.getFile("a1234567890"), drive.getFile("b1234567890")]);
  await drive.getFile("c1234567890");
  assert.equal(calls.filter((c) => c.url.includes("oauth2")).length, 1);
});

test("an expired token is refreshed", async () => {
  let now = 0;
  const { impl, calls } = fakeFetch((url) => (url.includes("oauth2") ? tokenOk() : json(200, file)));
  const drive = new DriveClient(creds, impl, 10_000, () => now);
  await drive.getFile("a1234567890");
  now += 3600 * 1000;
  await drive.getFile("a1234567890");
  assert.equal(calls.filter((c) => c.url.includes("oauth2")).length, 2);
});

test("a 401 refreshes and retries once", async () => {
  let driveCalls = 0;
  const { impl, calls } = fakeFetch((url) => {
    if (url.includes("oauth2")) return tokenOk();
    driveCalls++;
    return driveCalls === 1 ? json(401, {}) : json(200, file);
  });
  const got = await new DriveClient(creds, impl).getFile("a1234567890");
  assert.equal(got?.id, "file-id-12345");
  assert.equal(calls.filter((c) => c.url.includes("oauth2")).length, 2);
});

test("404 is null", async () => {
  const { impl } = fakeFetch((url) => (url.includes("oauth2") ? tokenOk() : json(404, {})));
  assert.equal(await new DriveClient(creds, impl).getFile("a1234567890"), null);
});

const errorFor = async (status: number, reason?: string) => {
  const { impl } = fakeFetch((url) =>
    url.includes("oauth2")
      ? tokenOk()
      : json(status, reason ? { error: { errors: [{ reason }] } } : {}),
  );
  try {
    await new DriveClient(creds, impl).getFile("a1234567890");
  } catch (error) {
    return error as DriveError;
  }
  throw new Error("expected a DriveError");
};

test("rate limits, 429 and 5xx are retryable; other 403 is not", async () => {
  assert.equal((await errorFor(403, "rateLimitExceeded")).retryable, true);
  assert.equal((await errorFor(403, "userRateLimitExceeded")).retryable, true);
  assert.equal((await errorFor(429)).retryable, true);
  assert.equal((await errorFor(503)).retryable, true);
  const forbidden = await errorFor(403, "insufficientFilePermissions");
  assert.equal(forbidden.retryable, false);
  assert.equal(forbidden.status, 403);
});

test("a network failure is retryable", async () => {
  const { impl } = fakeFetch((url) => {
    if (url.includes("oauth2")) return tokenOk();
    throw new TypeError("fetch failed");
  });
  await assert.rejects(new DriveClient(creds, impl).getFile("a1234567890"), (e: DriveError) => e.retryable);
});

test("listFiles asks for every drive and passes the query through", async () => {
  const { impl, calls } = fakeFetch((url) =>
    url.includes("oauth2") ? tokenOk() : json(200, { files: [{ id: "x" }], nextPageToken: "n" }),
  );
  const got = await new DriveClient(creds, impl).listFiles({
    q: "'abc' in parents",
    fields: "files(id)",
    pageSize: 10,
    orderBy: "name",
  });
  assert.deepEqual(got, { files: [{ id: "x" }], nextPageToken: "n" });
  const url = new URL(calls.at(-1)!.url);
  assert.equal(url.searchParams.get("q"), "'abc' in parents");
  assert.equal(url.searchParams.get("corpora"), "allDrives");
  assert.equal(url.searchParams.get("supportsAllDrives"), "true");
  assert.equal(url.searchParams.get("includeItemsFromAllDrives"), "true");
});

test("a failed refresh never repeats the response body", async () => {
  const { impl } = fakeFetch(() => json(400, { error: "invalid_grant", echo: "1//rt csecret" }));
  await assert.rejects(new DriveClient(creds, impl).getFile("a1234567890"), (e: Error) => {
    assert.doesNotMatch(e.message, /csecret|1\/\/rt|invalid_grant/);
    return true;
  });
});
