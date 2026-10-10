import { test, after } from "node:test";
import assert from "node:assert/strict";
import http, { type Server } from "node:http";

import { Audit } from "../audit.js";
import { createAdminApp, parseFolderInput, type AdminDeps } from "../admin/app.js";
import { esc, html } from "../admin/html.js";
import {
  LoginLimiter,
  SessionStore,
  firstHosts,
  gatewaysFromRoute,
  hashPassword,
  isAllowedHost,
  makeAllowedSources,
  usableHash,
  verifyPassword,
} from "../admin/security.js";
import { ScopeResolver } from "../enforce/resolver.js";
import { SubtreeCache } from "../enforce/search.js";
import { DEFAULT_GRANTS } from "../policy/derive.js";
import type { PolicyStore } from "../policy/store.js";
import { FakeDrive, TODAYS_TOOLS, listen, quietly, tempStore } from "./helpers.js";

const servers: Server[] = [];
after(() => servers.forEach((s) => s.close()));

const PASSWORD = "correct horse battery";
const HASH = hashPassword(PASSWORD);

async function boot(
  opts: { allowed?: Set<string>; hash?: string | null; store?: PolicyStore | null; now?: () => number } = {},
) {
  const store = opts.store === undefined ? tempStore() : opts.store;
  store?.ensureDefaultAgent(DEFAULT_GRANTS);
  const drive = new FakeDrive();
  drive.files.set("root", { id: "my-drive-root", name: "My Drive", mimeType: "application/vnd.google-apps.folder", parents: [], trashed: false });
  drive.folder("my-drive-root", []);
  drive.folder("scope-F-0001", ["my-drive-root"], { name: "Project F" });
  drive.folder("evil-folder1", ["my-drive-root"], { name: '<img src=x onerror=alert(1)>' });
  drive.folder("trashed-fold", ["my-drive-root"], { trashed: true });
  drive.add("a-plain-file", ["my-drive-root"]);
  drive.folder("shared-noparent", [], { name: "Shared with me" });
  const audit = store ? new Audit(store) : null;
  const now = opts.now ?? Date.now;
  const deps: AdminDeps = {
    store: () => store,
    dbError: () => (store ? null : "disk on fire"),
    audit: () => audit,
    drive: () => drive,
    resolver: new ScopeResolver(() => drive),
    subtree: new SubtreeCache(() => drive),
    passwordHash: () => (opts.hash === undefined ? HASH : opts.hash),
    exposedOrder: () => TODAYS_TOOLS,
    withheld: () => ({ scoped: new Set(), all: new Set() }),
    allowed: () => opts.allowed ?? new Set(["127.0.0.1", "::ffff:127.0.0.1"]),
    sessions: new SessionStore(now),
    limiter: new LoginLimiter(now),
  };
  const { url, server } = await listen(createAdminApp(deps));
  servers.push(server);
  return { url, store: store!, drive, audit };
}

type Reply = { status: number; text: string; location: string | null; cookie: string | null };

async function request(url: string, path: string, opts: { method?: string; cookie?: string; form?: Record<string, string | string[]>; headers?: Record<string, string> } = {}): Promise<Reply> {
  let body: string | undefined;
  if (opts.form) {
    const params = new URLSearchParams();
    for (const [k, v] of Object.entries(opts.form)) for (const item of [v].flat()) params.append(k, item);
    body = params.toString();
  }
  const res = await fetch(url + path, {
    method: opts.method ?? (body !== undefined ? "POST" : "GET"),
    redirect: "manual",
    headers: {
      ...(body !== undefined ? { "Content-Type": "application/x-www-form-urlencoded" } : {}),
      ...(opts.cookie ? { Cookie: opts.cookie } : {}),
      ...opts.headers,
    },
    body,
  });
  return { status: res.status, text: await res.text(), location: res.headers.get("location"), cookie: res.headers.get("set-cookie") };
}

async function login(url: string) {
  const reply = await request(url, "/login", { form: { password: PASSWORD } });
  assert.equal(reply.status, 303, reply.text);
  const cookie = reply.cookie!.split(";")[0]!;
  const home = await request(url, "/", { cookie });
  const csrf = /name="_csrf" value="([^"]+)"/.exec(home.text)![1]!;
  return { cookie, csrf, setCookie: reply.cookie! };
}

// --- primitives ------------------------------------------------------------

test("html`` escapes values and attributes, passes Safe through", () => {
  assert.equal(esc(`<a href="x">'&`), "&lt;a href=&quot;x&quot;&gt;&#39;&amp;");
  const evil = "<script>alert(1)</script>";
  assert.equal(html`<p title="${evil}">${evil}</p>`.__html, `<p title="${esc(evil)}">${esc(evil)}</p>`);
  assert.equal(html`<div>${html`<b>ok</b>`}</div>`.__html, "<div><b>ok</b></div>");
  assert.equal(html`${[1, "<", null, false, undefined]}`.__html, "1&lt;");
});

test("gatewaysFromRoute reads little-endian hex", () => {
  const text = "Iface\tDestination\tGateway \tFlags\neth0\t00000000\t010012AC\t0003\neth0\t000012AC\t00000000\t0001\n";
  assert.deepEqual(gatewaysFromRoute(text), ["172.18.0.1"]);
});

test("firstHosts derives each bridge's gateway", () => {
  const got = firstHosts({
    eth0: [{ address: "172.18.0.5", netmask: "255.255.0.0", family: "IPv4", internal: false, mac: "", cidr: null }],
    eth1: [{ address: "10.9.8.7", netmask: "255.255.255.0", family: "IPv4", internal: false, mac: "", cidr: null }],
    lo: [{ address: "127.0.0.1", netmask: "255.0.0.0", family: "IPv4", internal: true, mac: "", cidr: null }],
  });
  assert.deepEqual(got.sort(), ["10.9.8.1", "172.18.0.1"]);
});

test("allowed sources recompute after 60 s and survive no /proc/net/route", () => {
  let now = 0;
  let ifaces: Record<string, any> = {
    eth0: [{ address: "172.18.0.5", netmask: "255.255.0.0", family: "IPv4", internal: false }],
  };
  const allowed = makeAllowedSources(["192.0.2.9"], { interfaces: () => ifaces, routeText: () => null }, () => now);
  const first = allowed();
  assert.ok(first.has("172.18.0.1") && first.has("::ffff:172.18.0.1") && first.has("192.0.2.9"));
  // Loopback is the child's address, not an admin's.
  assert.ok(!first.has("127.0.0.1") && !first.has("::1"));
  ifaces = { eth1: [{ address: "10.5.0.3", netmask: "255.255.255.0", family: "IPv4", internal: false }] };
  assert.ok(!allowed().has("10.5.0.1"));
  now += 60_001;
  assert.ok(allowed().has("10.5.0.1"));
});

test("hashPassword / verifyPassword round-trip; malformed hashes fail", async () => {
  assert.equal(await verifyPassword(PASSWORD, HASH), true);
  assert.equal(await verifyPassword("wrong", HASH), false);
  assert.equal(await verifyPassword(PASSWORD, "plain"), false);
  assert.equal(await verifyPassword(PASSWORD, "scrypt$1048576$8$1$AAAA$AAAA"), false);
});

test("LoginLimiter locks one source after 5 failures, not others", () => {
  let now = 0;
  const limiter = new LoginLimiter(() => now);
  for (let i = 0; i < 5; i++) limiter.fail("a");
  assert.equal(limiter.locked("a"), true);
  assert.equal(limiter.locked("b"), false);
  now += 15 * 60_000 + 1;
  assert.equal(limiter.locked("a"), false);
});

test("sessions expire when idle for 30 min and after 8 h regardless", () => {
  let now = 0;
  const store = new SessionStore(() => now);
  const idle = store.create();
  now += 30 * 60_000 + 1;
  assert.equal(store.get(idle.id), null);
  const busy = store.create();
  for (let i = 0; i < 20; i++) {
    now += 25 * 60_000;
    if (i < 18) assert.ok(store.get(busy.id), `alive at step ${i}`);
  }
  assert.equal(store.get(busy.id), null);
  const swept = store.create();
  now += 31 * 60_000;
  store.sweep();
  assert.equal(store.size, 0);
  assert.equal(store.get(swept.id), null);
});

test("parseFolderInput accepts an ID or a folder URL, nothing else", () => {
  assert.equal(parseFolderInput("scope-F-0001"), "scope-F-0001");
  assert.equal(parseFolderInput("https://drive.google.com/drive/folders/scope-F-0001?usp=sharing"), "scope-F-0001");
  assert.equal(parseFolderInput("https://drive.google.com/open?id=scope-F-0001"), "scope-F-0001");
  assert.equal(parseFolderInput("x' or '1"), null);
  assert.equal(parseFolderInput(["scope-F-0001"]), null);
});

// --- the app ---------------------------------------------------------------

test("a source outside the allowlist is 403, whatever X-Forwarded-For says", async () => {
  const { url } = await boot({ allowed: new Set(["10.0.0.1"]) });
  assert.equal((await request(url, "/login")).status, 403);
  assert.equal((await request(url, "/login", { headers: { "X-Forwarded-For": "10.0.0.1" } })).status, 403);
});

test("security headers are present", async () => {
  const { url } = await boot();
  const res = await fetch(`${url}/login`);
  assert.match(res.headers.get("content-security-policy") ?? "", /default-src 'none'/);
  assert.equal(res.headers.get("x-content-type-options"), "nosniff");
});

test("login page states: starting, not configured, form", async () => {
  assert.match((await request((await boot({ hash: null })).url, "/login")).text, /Starting/);
  assert.match((await request((await boot({ hash: "" })).url, "/login")).text, /not configured/);
  assert.match((await request((await boot()).url, "/login")).text, /type="password"/);
});

test("POST /login needs no session; the cookie is HttpOnly, SameSite=Strict, not Secure on http", async () => {
  const { url } = await boot();
  const { setCookie } = await login(url);
  assert.match(setCookie, /^gws_admin_sid=/);
  assert.match(setCookie, /HttpOnly/);
  assert.match(setCookie, /SameSite=Strict/);
  assert.match(setCookie, /Path=\//);
  assert.doesNotMatch(setCookie, /Secure/);
});

test("five wrong passwords lock the source, even against the right one", async () => {
  const { url, store } = await boot();
  for (let i = 0; i < 5; i++) {
    assert.equal((await request(url, "/login", { form: { password: "nope" } })).status, 401);
  }
  assert.equal((await request(url, "/login", { form: { password: PASSWORD } })).status, 429);
  assert.ok(store.queryAudit({ limit: 10 }).some((r) => r.reason === "login"));
});

test("pages without a session redirect to login", async () => {
  const { url } = await boot();
  const reply = await request(url, "/");
  assert.equal(reply.status, 303);
  assert.equal(reply.location, "/login");
});

test("a POST without a CSRF token, or with an array, is 403", async () => {
  const { url } = await boot();
  const { cookie, csrf } = await login(url);
  assert.equal((await request(url, "/agents", { cookie, form: { slug: "x1", name: "X" } })).status, 403);
  assert.equal((await request(url, "/agents", { cookie, form: { slug: "x1", name: "X", _csrf: [csrf, csrf] } })).status, 403);
});

test("create agent: confined, with warnings", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  const created = await request(url, "/agents", { cookie, form: { slug: "ops-agent", name: "Ops", _csrf: csrf } });
  assert.equal(created.location, "/agents/ops-agent");
  assert.equal(store.getAgent("ops-agent")?.scoped, true);
  const pageText = (await request(url, "/agents/ops-agent", { cookie })).text;
  assert.match(pageText, /No folder assigned/);
  const dup = await request(url, "/agents", { cookie, form: { slug: "ops-agent", name: "Ops", _csrf: csrf } });
  assert.equal(dup.location, "/");
  assert.match((await request(url, "/", { cookie })).text, /already exists/);
});

test("grants: one value, two values, unknown key", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "g1", name: "G", _csrf: csrf } });
  assert.equal((await request(url, "/agents/g1/grants", { cookie, form: { g: "docs:read", _csrf: csrf } })).status, 303);
  assert.deepEqual(store.getGrants("g1"), [{ area: "docs", op: "read" }]);
  await request(url, "/agents/g1/grants", { cookie, form: { g: ["docs:read", "drive:read"], _csrf: csrf } });
  assert.equal(store.getGrants("g1").length, 2);
  assert.equal((await request(url, "/agents/g1/grants", { cookie, form: { g: ["docs:read", "gmail:read"], _csrf: csrf } })).status, 400);
  assert.equal(store.getGrants("g1").length, 2);
  await request(url, "/agents/g1/grants", { cookie, form: { g: "docs:read", _csrf: csrf } });
  assert.match((await request(url, "/agents/g1", { cookie })).text, /No discovery tool/);
});

test("a minted token is shown exactly once, then revocable", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "t1", name: "T", _csrf: csrf } });
  await request(url, "/agents/t1/tokens", { cookie, form: { label: "owui", _csrf: csrf } });
  const first = (await request(url, "/agents/t1", { cookie })).text;
  const token = /<code>(gws_[A-Za-z0-9_-]{43})<\/code>/.exec(first)?.[1];
  assert.ok(token, "token shown");
  assert.doesNotMatch((await request(url, "/agents/t1", { cookie })).text, /gws_[A-Za-z0-9_-]{43}/);
  const [row] = store.listTokens("t1");
  const revoke = await request(url, `/tokens/${row!.id}/revoke`, { cookie, form: { _csrf: csrf } });
  assert.equal(revoke.location, "/agents/t1");
  assert.notEqual(store.listTokens("t1")[0]!.revokedAt, null);
});

test("adding scopes validates against Drive", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "s1", name: "S", _csrf: csrf } });
  const add = (folder: string) => request(url, "/agents/s1/scopes", { cookie, form: { folder, _csrf: csrf } });
  const flash = async () => (await request(url, "/agents/s1", { cookie })).text;

  await add("a-plain-file");
  assert.match(await flash(), /a file, not a folder/);
  await add("trashed-fold");
  assert.match(await flash(), /in the trash/);
  await add("my-drive-root");
  assert.match(await flash(), /Drive root/);
  await add("does-not-exist");
  assert.match(await flash(), /cannot find/);
  await add("https://drive.google.com/drive/folders/scope-F-0001");
  assert.deepEqual(store.getScopes("s1").map((s) => [s.folderId, s.folderName, s.isPrimary]), [["scope-F-0001", "Project F", true]]);
  await add("shared-noparent");
  assert.equal(store.getScopes("s1").length, 2);
});

test("a hostile folder name renders inert on the agent and browse pages", async () => {
  const { url } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "x1", name: "X", _csrf: csrf } });
  await request(url, "/agents/x1/scopes", { cookie, form: { folder: "evil-folder1", _csrf: csrf } });
  // The fake does not alias `root` inside a parents query; use the real ID.
  for (const path of ["/agents/x1", "/agents/x1/browse?parent=my-drive-root"]) {
    const text = (await request(url, path, { cookie })).text;
    assert.doesNotMatch(text, /<img src=x/);
    assert.match(text, /&lt;img src=x onerror=alert\(1\)&gt;/);
  }
});

test("browse rejects a parent that is not an ID", async () => {
  const { url } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "b1", name: "B", _csrf: csrf } });
  assert.equal((await request(url, `/agents/b1/browse?parent=${encodeURIComponent("x' or '1")}`, { cookie })).status, 400);
});

test("unconfine needs confirmation and zero scopes; default cannot be scoped or archived", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "u1", name: "U", _csrf: csrf } });
  await request(url, "/agents/u1/scopes", { cookie, form: { folder: "scope-F-0001", _csrf: csrf } });
  await request(url, "/agents/u1/unconfine", { cookie, form: { confirm: "yes", _csrf: csrf } });
  assert.equal(store.getAgent("u1")?.scoped, true);
  await request(url, "/agents/u1/scopes/scope-F-0001/remove", { cookie, form: { _csrf: csrf } });
  await request(url, "/agents/u1/unconfine", { cookie, form: { _csrf: csrf } });
  assert.equal(store.getAgent("u1")?.scoped, true);
  await request(url, "/agents/u1/unconfine", { cookie, form: { confirm: "yes", _csrf: csrf } });
  assert.equal(store.getAgent("u1")?.scoped, false);

  await request(url, "/agents/default/scopes", { cookie, form: { folder: "scope-F-0001", _csrf: csrf } });
  assert.equal(store.getScopes("default").length, 0);
  await request(url, "/agents/default/archive", { cookie, form: { _csrf: csrf } });
  assert.equal(store.getAgent("default")?.archivedAt, null);
});

test("the audit page escapes what it shows", async () => {
  const { url, audit } = await boot();
  audit!.tool({ slug: "default" }, "readDocument", "deny", "outside", ["<script>x</script>"]);
  const { cookie } = await login(url);
  const text = (await request(url, "/audit?decision=deny", { cookie })).text;
  assert.doesNotMatch(text, /<script>x/);
  assert.match(text, /&lt;script&gt;x/);
});

test("collision warning when two credentialed agents share a tool", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "c1", name: "C", _csrf: csrf } });
  await request(url, "/agents/c1/grants", { cookie, form: { g: "calendar:read", _csrf: csrf } });
  assert.doesNotMatch((await request(url, "/", { cookie })).text, /both expose/);
  await request(url, "/agents/c1/tokens", { cookie, form: { _csrf: csrf } });
  assert.match((await request(url, "/", { cookie })).text, /default and c1 both expose listEvents/);
  assert.ok(store.listTokens("c1").length === 1);
});

test("without a database every page explains the error", async () => {
  const { url } = await boot({ store: null });
  const reply = await request(url, "/login");
  assert.equal(reply.status, 503);
  assert.match(reply.text, /disk on fire/);
});

test("verifyPassword never throws on hashes scrypt would reject", async () => {
  const salt = Buffer.alloc(16).toString("base64");
  const key = Buffer.alloc(32).toString("base64");
  for (const params of ["1000$8$1", "131072$16$1", "16384$0$1", "16384$8$9"]) {
    assert.equal(await verifyPassword(PASSWORD, `scrypt$${params}$${salt}$${key}`), false, params);
  }
  assert.equal(usableHash(HASH), true);
  assert.equal(usableHash(`scrypt$1000$8$1$${salt}$${key}`), false);
  assert.equal(usableHash(`scrypt$16384$8$9$${salt}$${key}`), false);
  assert.equal(usableHash(`scrypt$2$131072$4$${salt}$${key}`), false);
  assert.equal(usableHash("plain"), false);
});

test("the failure count decays: five failures must fall within 15 minutes", () => {
  let now = 0;
  const limiter = new LoginLimiter(() => now);
  for (let i = 0; i < 4; i++) limiter.fail("a");
  now += 16 * 60_000;
  assert.equal(limiter.fail("a"), false);
  assert.equal(limiter.locked("a"), false);
  for (let i = 0; i < 3; i++) limiter.fail("a");
  assert.equal(limiter.fail("a"), true);
  assert.equal(limiter.locked("a"), true);
});

test("isAllowedHost: loopback names only, plus configured ones", () => {
  assert.equal(isAllowedHost("localhost:3015"), true);
  assert.equal(isAllowedHost("127.0.0.1:3015"), true);
  assert.equal(isAllowedHost("[::1]:3015"), true);
  assert.equal(isAllowedHost("evil.example:3015"), false);
  assert.equal(isAllowedHost(undefined), false);
  assert.equal(isAllowedHost("admin.internal", ["admin.internal"]), true);
  assert.equal(isAllowedHost("admin.internal:3015", ["Admin.Internal:443"]), true);
});

test("a foreign Host header is refused (DNS rebinding)", async () => {
  const { url } = await boot();
  // fetch() will not send a custom Host header; a raw request will.
  const status = (host: string) =>
    new Promise<number>((resolve, reject) => {
      const req = http.request(`${url}/login`, { headers: { Host: host } }, (res) => {
        res.resume();
        resolve(res.statusCode ?? 0);
      });
      req.on("error", reject);
      req.end();
    });
  assert.equal(await status("attacker.example:3015"), 403);
  assert.equal(await status("localhost:3015"), 200);
});

test("a malformed stored hash says so instead of failing every login", async () => {
  const { url } = await boot({ hash: "scrypt$1000$8$1$AAAA$AAAA" });
  assert.match((await request(url, "/login")).text, /not a usable scrypt hash/);
  assert.equal((await request(url, "/login", { form: { password: PASSWORD } })).status, 503);
});

test("logout ends the session server-side", async () => {
  const { url } = await boot();
  const { cookie, csrf } = await login(url);
  assert.equal((await request(url, "/", { cookie })).status, 200);
  const out = await request(url, "/logout", { cookie, form: { _csrf: csrf } });
  assert.equal(out.location, "/login");
  const after = await request(url, "/", { cookie });
  assert.equal(after.status, 303);
  assert.equal(after.location, "/login");
});

test("failed logins and the lockout are each audited, not deduplicated", async () => {
  const { url, store } = await boot();
  for (let i = 0; i < 5; i++) await request(url, "/login", { form: { password: "nope" } });
  const rows = store.queryAudit({ limit: 50 }).filter((r) => r.kind === "auth");
  assert.equal(rows.filter((r) => r.reason === "login").length, 5);
  assert.equal(rows.filter((r) => r.reason === "login_lockout").length, 1);
});

test("dashboard changes are filed under the agent they touched", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "f1", name: "F", _csrf: csrf } });
  await request(url, "/agents/f1/grants", { cookie, form: { g: "docs:read", _csrf: csrf } });
  const rows = store.queryAudit({ agent: "f1", limit: 10 });
  assert.ok(rows.some((r) => r.kind === "admin" && r.reason === "grants.set"));
  assert.ok(rows.some((r) => r.kind === "admin" && r.reason === "agent.create"));
});

test("an archived agent can be viewed but not changed", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "z1", name: "Z", _csrf: csrf } });
  await request(url, "/agents/z1/archive", { cookie, form: { _csrf: csrf } });
  assert.equal((await request(url, "/agents/z1", { cookie })).status, 200);
  for (const route of [
    "/agents/z1/grants",
    "/agents/z1/tokens",
    "/agents/z1/archive",
    "/agents/z1/unconfine",
    "/agents/z1/scopes",
    "/agents/z1/scopes/scope-F-0001/remove",
    "/agents/z1/scopes/scope-F-0001/primary",
  ]) {
    const reply = await request(url, route, { cookie, form: { g: "docs:read", folder: "scope-F-0001", confirm: "yes", _csrf: csrf } });
    assert.equal(reply.status, 409, route);
  }
  assert.equal(store.getScopes("z1").length, 0);
  assert.equal(store.listTokens("z1").length, 0);
  assert.equal(store.getGrants("z1").length, 0);
});

test("removing a folder that is not in scope says so", async () => {
  const { url } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "r1", name: "R", _csrf: csrf } });
  await request(url, "/agents/r1/scopes/not-a-scope-1/remove", { cookie, form: { _csrf: csrf } });
  assert.match((await request(url, "/agents/r1", { cookie })).text, /Not found/);
});

test("an unhandled error is the dashboard's own 500 page, not a stack trace", async () => {
  const { url, store } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "e1", name: "E", _csrf: csrf } });
  store.setGrants = () => {
    throw new Error("SQLITE_BUSY: database is locked");
  };
  const { logs } = await quietly(async () => {
    const reply = await request(url, "/agents/e1/grants", { cookie, form: { g: "docs:read", _csrf: csrf } });
    assert.equal(reply.status, 500);
    assert.match(reply.text, /Something went wrong/);
    assert.doesNotMatch(reply.text, /SQLITE_BUSY|at .*\.js/);
  });
  assert.ok(logs.some((l) => /SQLITE_BUSY/.test(l)));
});

test("the agents page explains first-run when only default exists", async () => {
  const { url } = await boot();
  const { cookie } = await login(url);
  assert.match((await request(url, "/", { cookie })).text, /Only the <code>default<\/code> agent exists/);
});

test("the lockout window slides: a burst across a window edge still locks", () => {
  let now = 0;
  const limiter = new LoginLimiter(() => now);
  now = 10 * 60_000;
  for (let i = 0; i < 4; i++) limiter.fail("a");
  now = 16 * 60_000;
  assert.equal(limiter.fail("a"), true);
  assert.equal(limiter.locked("a"), true);
});

test("an oversized form is 413 on the dashboard's own page", async () => {
  const { url } = await boot();
  const { cookie, csrf } = await login(url);
  const reply = await request(url, "/agents", { cookie, form: { slug: "big", name: "x".repeat(70_000), _csrf: csrf } });
  assert.equal(reply.status, 413);
  assert.match(reply.text, /could not be read/);
});

test("the first-run note is not shown when archived agents exist", async () => {
  const { url } = await boot();
  const { cookie, csrf } = await login(url);
  await request(url, "/agents", { cookie, form: { slug: "gone", name: "G", _csrf: csrf } });
  await request(url, "/agents/gone/archive", { cookie, form: { _csrf: csrf } });
  assert.doesNotMatch((await request(url, "/", { cookie })).text, /Only the <code>default<\/code> agent exists/);
});
