import { test } from "node:test";
import assert from "node:assert/strict";

import { DriveClient, DriveError, categoryOf, rejectionDetail } from "../drive/client.js";
import { creds, fakeFetch, json, tokenOk } from "./helpers.js";

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

test("eventGuests leaves out this account and is null for a missing event", async () => {
  const { impl, calls } = fakeFetch((url) => {
    if (url.includes("oauth2")) return tokenOk();
    if (url.includes("/events/missing")) return json(404, {});
    return json(200, { attendees: [{ email: "me@x.com", self: true }, { email: "guest@y.com" }] });
  });
  const drive = new DriveClient(creds, impl);
  assert.deepEqual(await drive.eventGuests("primary", "evt1"), ["guest@y.com"]);
  assert.equal(await drive.eventGuests("primary", "missing"), null);
  assert.match(calls.at(-2)!.url, /calendar\/v3\/calendars\/primary\/events\/evt1/);
});

// --- credential rejection -------------------------------------------------

const refreshError = async (status: number, body?: unknown, raw?: string) => {
  const { impl, calls } = fakeFetch(() =>
    raw !== undefined ? new Response(raw, { status }) : json(status, body ?? {}),
  );
  try {
    await new DriveClient(creds, impl).getFile("a1234567890");
  } catch (error) {
    return { error: error as DriveError, calls };
  }
  throw new Error("expected a DriveError");
};

test("a refresh answered 400 invalid_grant is a rejection and never echoes the description", async () => {
  const { error } = await refreshError(400, { error: "invalid_grant", error_description: "echo-secret" });
  assert.equal(error.rejection, "invalid_grant");
  assert.equal(error.retryable, false);
  assert.equal(error.source, "oauth");
  assert.doesNotMatch(error.message, /echo-secret|invalid_grant/);
  assert.doesNotMatch(String(error), /echo-secret/);
  assert.equal(categoryOf(error), "invalid_grant");
  assert.equal(rejectionDetail(error), "invalid_grant");
});

test("any other 4xx refresh answer is a rejection too; the category is sanitised", async () => {
  assert.equal((await refreshError(401, { error: "deleted_client" })).error.rejection, "deleted_client");
  assert.equal((await refreshError(400, undefined, "<html>no</html>")).error.rejection, "HTTP 400");
  assert.equal((await refreshError(403, undefined, "<html>no</html>")).error.rejection, "HTTP 403");
  assert.equal((await refreshError(400, { error: "Has Spaces!" })).error.rejection, "HTTP 400");
});

test("408, 429 and 5xx refresh answers stay retryable, with no rejection and the body unread", async () => {
  for (const status of [408, 429, 500, 503]) {
    const { error } = await refreshError(status, { error: "invalid_grant" });
    assert.equal(error.retryable, true, String(status));
    assert.equal(error.rejection, undefined, String(status));
    assert.equal(categoryOf(error), `HTTP ${status}`);
  }
});

// --- service rejections on Drive and Calendar ---------------------------------

const envelope = (reason: string | undefined, detailReason?: string, message = "denied") => ({
  error: {
    code: 403,
    message,
    ...(reason ? { errors: [{ reason }] } : {}),
    status: "PERMISSION_DENIED",
    ...(detailReason
      ? { details: [{ "@type": "type.googleapis.com/google.rpc.ErrorInfo", reason: detailReason }] }
      : {}),
  },
});

const driveFailure = async (body: unknown, status = 403) => {
  let oks = 0;
  const { impl } = fakeFetch((url) => (url.includes("oauth2") ? tokenOk() : json(status, body)));
  const drive = new DriveClient(creds, impl, 10_000, Date.now, { onOk: () => oks++ });
  try {
    await drive.getFile("a1234567890");
  } catch (error) {
    return { error: error as DriveError, oks };
  }
  throw new Error("expected a DriveError");
};

test("a disabled API or a missing scope on Drive is a service rejection and no sign of health", async () => {
  for (const body of [
    envelope("accessNotConfigured"),
    envelope(undefined, "SERVICE_DISABLED"),
    envelope("insufficientPermissions", "ACCESS_TOKEN_SCOPE_INSUFFICIENT"),
    envelope(undefined, "ACCESS_TOKEN_SCOPE_INSUFFICIENT"),
    envelope("insufficientPermissions", undefined, "Request had insufficient authentication scopes."),
  ]) {
    const { error, oks } = await driveFailure(body);
    assert.ok(error.rejection, JSON.stringify(body));
    assert.equal(error.retryable, false);
    assert.equal(error.source, "drive");
    assert.equal(oks, 0);
  }
  assert.equal((await driveFailure(envelope("accessNotConfigured"))).error.rejection, "accessNotConfigured");
  assert.equal(categoryOf((await driveFailure(envelope("accessNotConfigured"))).error), "drive accessNotConfigured");
});

test("a per-file 403 is not a rejection and shows Drive answered", async () => {
  for (const reason of ["insufficientPermissions", "insufficientFilePermissions", "forbidden"]) {
    const { error, oks } = await driveFailure(envelope(reason));
    assert.equal(error.rejection, undefined, reason);
    assert.equal(error.retryable, false);
    assert.equal(oks, 1, reason);
  }
});

test("a rate-limit 403 stays retryable and shows nothing", async () => {
  const { error, oks } = await driveFailure(envelope("rateLimitExceeded"));
  assert.equal(error.retryable, true);
  assert.equal(oks, 0);
});

test("a Calendar service rejection is tagged calendar and never calls onOk", async () => {
  let oks = 0;
  let calls = 0;
  const { impl } = fakeFetch((url) => {
    if (url.includes("oauth2")) return tokenOk();
    calls++;
    return calls === 1 ? json(401, {}) : json(403, envelope("accessNotConfigured"));
  });
  const drive = new DriveClient(creds, impl, 10_000, Date.now, { onOk: () => oks++ });
  await assert.rejects(drive.eventGuests("primary", "evt1"), (e: DriveError) => {
    assert.equal(e.source, "calendar");
    assert.equal(e.rejection, "accessNotConfigured");
    assert.equal(rejectionDetail(e), "calendar accessNotConfigured");
    return true;
  });
  assert.equal(oks, 0);
});

test("onOk gets the time the request started, on 200 and 404 only", async () => {
  const seen: number[] = [];
  let now = 100;
  let status = 200;
  const { impl } = fakeFetch((url) => {
    if (url.includes("oauth2")) return tokenOk();
    now += 50;
    return json(status, file);
  });
  const drive = new DriveClient(creds, impl, 10_000, () => now, { onOk: (t) => seen.push(t) });
  await drive.getFile("a1234567890");
  status = 404;
  await drive.getFile("a1234567890");
  assert.deepEqual(seen, [100, 150]);
  for (const bad of [503, 429]) {
    status = bad;
    await assert.rejects(drive.getFile("a1234567890"));
  }
  assert.equal(seen.length, 2);
});

test("onOk is not called for a network error or a failed 401 retry", async () => {
  let oks = 0;
  const net = fakeFetch((url) => {
    if (url.includes("oauth2")) return tokenOk();
    throw new TypeError("fetch failed");
  });
  await assert.rejects(
    new DriveClient(creds, net.impl, 10_000, Date.now, { onOk: () => oks++ }).getFile("a1234567890"),
  );
  const unauthorised = fakeFetch((url) => (url.includes("oauth2") ? tokenOk() : json(401, {})));
  await assert.rejects(
    new DriveClient(creds, unauthorised.impl, 10_000, Date.now, { onOk: () => oks++ }).getFile("a1234567890"),
  );
  assert.equal(oks, 0);
});

test("categoryOf never returns a message, and reads timeouts and not-ready clients", () => {
  assert.equal(categoryOf(new DriveError(401, true, "Drive rejected a freshly refreshed token")), "HTTP 401");
  assert.equal(categoryOf(new DriveError(0, true, "Drive unreachable: fetch failed")), "network");
  assert.equal(categoryOf(new DriveError(0, true, "Drive client not ready")), "unavailable");
  assert.equal(categoryOf(new DriveError(200, true, "token refresh returned no token")), "network");
  const abort = new Error("x");
  abort.name = "TimeoutError";
  assert.equal(categoryOf(abort), "timeout");
  assert.equal(categoryOf(new DOMException("aborted", "AbortError")), "timeout");
  assert.equal(categoryOf(new Error("sk-secret-token 1//rt")), "network");
});
