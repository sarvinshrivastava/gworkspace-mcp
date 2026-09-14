# Relaying a stdio MCP server over Streamable HTTP

> **Rule:** never spread `process.env` into the upstream child — build its environment explicitly.

Notes from building and reviewing this bridge. All of it was measured against
`@a-bonus/google-docs-mcp@1.11.3` and `@modelcontextprotocol/sdk@1.30.0`.

## The child's stderr is the only diagnostic, and it is off by default

- `StdioClientTransport({ stderr: "pipe" })` builds a `PassThrough` in its
  **constructor** and pipes the child into it on `start()`. Nothing reads it
  unless you do.
- Unread, it wedges: 16 KB in the PassThrough, 16 KB on its writable side, 64 KB
  in the OS pipe, and then the child blocks in `write(2)`.
- Attach the reader **before** `client.connect()`. The boot handshake is the
  failure you most need explained, and after connect is too late. Concretely:

  ```
  before: MCP error -32000: Connection closed
  after:  [upstream] Error: Cannot find module '/app/node_modules/...'
  ```

- Do not pipe it raw to your own stderr. The upstream logs whole error objects,
  and a `google-auth-library` refresh failure throws a `GaxiosError` whose
  `config` carries the POST body — client secret and refresh token both.

## A dead child is invisible unless you ask for it

- The SDK's `Protocol._onclose` clears **its own** `_transport` and calls
  `this.onclose?.()`. It never touches your module-level `Client` reference.
- So without `client.onclose`, the client stays non-null forever: `/healthz`
  stays 200, the Docker healthcheck stays green, `autoheal` never fires,
  `restart: unless-stopped` never fires (PID 1 is alive), and every tool call
  returns `Failed: Not connected` until a human restarts it.
- Verified: `pkill` the child, and health goes `200 {"status":true}` →
  `503 {"status":false}` only once `onclose` sets the flag and `/healthz`
  returns a non-200.

## `tools/list` is not the same as the upstream's source registrations

- The upstream's source registers `appendTableRows` twice — once in `tools/docs`
  (documentId, tableId, rows), once in `tools/sheets` (spreadsheetId,
  tableIdentifier, values).
- `registerTool` in `@modelcontextprotocol/server` **throws** on a duplicate name
  rather than warning. `createServer()` runs per request, so that throw would be
  a 500 on every `POST /mcp` — including `initialize` — with `/healthz` green.
- But measured: `tools/list` returns **119 entries, all unique**. FastMCP
  collapses the collision upstream, so it never reaches the relay.
- The collapse is lossy in a way nothing reports: **the sheets implementation
  wins**. The docs `appendTableRows` is unreachable through the relay. Keep it
  out of the allowlist rather than exposing a tool that cannot do what its docs
  counterpart's name suggests.

## The upstream does no network I/O at boot

- `authorize()` reads `token.json`, `JSON.parse`s it, and constructs an
  `OAuth2Client`. The first Google call happens on the first tool invocation.
- So a **revoked or expired grant boots perfectly** and then fails every user
  action, with health green throughout. One throwaway tool call after connect
  turns that into a log line at the moment it becomes true.
- Corollary for CI: a syntactically valid `token.json` with a fake
  `refresh_token` boots the child completely and lists all its tools. No
  credentials needed to test the whole relay path.

## A malformed token does not fail — it hangs

- `loadSavedCredentialsIfExist` wraps read + parse in `try {} catch { return null }`.
  On a parse failure `authorize()` falls through to `authenticate()`, the
  **interactive browser consent flow**, which binds a random localhost port and
  waits. Its 5-minute timer calls `server.close()` but never settles the promise.
- The observable is `client.connect()` rejecting after the SDK's 60 s default
  with `MCP error -32001: Request timed out`, and — with stderr piped and unread
  — an empty log.
- Validate the token JSON before writing it, and say so in the error.

## Two Node processes, one cgroup

- Measured inside the capped container: parent 112 MiB, child 208 MiB
  (`googleapis` instantiates six API clients at boot), combined idle 320.5 MiB.
- `NODE_OPTIONS` set on the container is inherited by the child through any
  `...process.env` spread, so one `--max-old-space-size` is double-counted.
  Give the child its own budget explicitly.
- When the cgroup OOMs, the kernel kills the **child** (larger RSS), not PID 1 —
  Docker does not set `memory.oom.group`. That lands you in the dead-child case
  above, which is why detecting it matters.

## `ssh host VAR="$X" bash -s` does not keep a secret out of the shell

- ssh joins every argument into one string that the **remote login shell**
  parses. A secret containing `;` or `$( )` executes as root, and the values sit
  in the remote argv for `ps` to read.
- Send them on stdin ahead of the script, each quoted with `printf '%q'`.
  Verified with a hostile value: `ghs_tok'en;touch /tmp/PWNED` arrives byte-exact
  and creates no file.

## Validate a credential by shape, and say what you saw

- `GOOGLE_TOKEN_JSON` was rejected twice in a row with only the raw `JSON.parse`
  message. That message says *where* parsing stopped and nothing about what was
  actually stored, so each round trip cost a deploy.
- The error now reports length, line count, and the first and last two characters
  — enough to tell "bare token", "two objects glued" and "wrong file" apart, and
  no part of the secret.
- A bare `1//...` refresh token is now accepted and wrapped, with a warning. It
  is what you get from copying the token out of `token.json` instead of copying
  the file, it is the obvious mistake to make through a vault UI, and there is no
  ambiguity about what was meant. Trailing garbage is still refused — that could
  be two different credentials, and guessing which one wins is worse than failing.
- Distinguish "parsed but has no refresh_token" from "did not parse", and list
  the keys that *were* found: `client_id`/`client_secret` means the OAuth client
  file was stored instead of the token, which is a different fix.
