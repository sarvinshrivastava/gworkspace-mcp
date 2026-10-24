# gworkspace-mcp

Google Docs, Sheets, Drive and Calendar in a self-hosted Open WebUI.

## What it is

A relay, not a reimplementation. The upstream `@a-bonus/google-docs-mcp` speaks
**stdio**; Open WebUI's MCP client speaks **Streamable HTTP and nothing else**
(its `utils/mcp/client.py` imports `streamablehttp_client` alone). This runs the
upstream as a child process and relays between the two, passing tool schemas
through untouched — so it keeps working when the upstream adds tools.

Two SDK majors are used on purpose: v2 has no stdio client transport, v1 does,
and both speak the same wire protocol.

## Why the upstream is a pinned dependency

`npx -y` at runtime would fetch a package over the network on every boot, resolve
a version nobody tested, and put a live download in the startup path of a service
holding Google credentials. It is pinned at `1.11.3` and installed at build time.

## The tool allowlist matters more than it looks

The upstream exposes **~120 tools**, and Open WebUI re-lists tools on *every chat
turn* — so exposing all of them puts a large schema payload in front of the model
on every message. `TOOL_ALLOWLIST` narrows it to the ones actually used; an
empty allowlist exposes nothing, by design.

## Secrets

Four required values in the vault under `Root`, and one optional:

| Key | What |
|---|---|
| `GWORKSPACE_MCP_TOKEN` | the bearer Open WebUI presents |
| `GOOGLE_CLIENT_ID` | Desktop-app OAuth client id |
| `GOOGLE_CLIENT_SECRET` | its secret |
| `GOOGLE_TOKEN_JSON` | the contents of `~/.config/google-docs-mcp/token.json` |
| `GWORKSPACE_NTFY_TOPIC` | optional: the ntfy topic for broken-only pushes (see below). Any unguessable name you choose; share it with every agent using the service. Delete the key to turn pushes off: an empty stored value, or a 401/403 reading it, stops boot like the other optional keys |

The vault is the only copy that matters. `token.json` is written into a tmpfs at
every boot from `GOOGLE_TOKEN_JSON` and nothing persists it: the upstream writes
that file only from its interactive consent flow, which cannot run in a
container, and it installs no `tokens` listener, so a refresh never rewrites it.
Nothing here needs backing up — losing the container costs nothing.

Losing `Root/GOOGLE_TOKEN_JSON`, or having the grant revoked, is the expensive
one:

1. On the Mac: `GOOGLE_CLIENT_ID=… GOOGLE_CLIENT_SECRET=… npx -y @a-bonus/google-docs-mcp auth`,
   then consent (Testing-mode apps show "unverified app" → Advanced → Go to → Allow).
2. `cat ~/.config/google-docs-mcp/token.json` — it holds only `{"refresh_token": "..."}`.
   That is expected; the package strips the rest.
3. Store that exact content as `Root/GOOGLE_TOKEN_JSON` in the vault.
4. Run the Deploy workflow.

If the OAuth consent screen is still in **Testing** status, Google expires the
refresh token every 7 days and this breaks weekly. Move the client to
"In production" — no verification review is needed for a single internal user.

## Push alerts

When `GWORKSPACE_NTFY_TOPIC` is set the service posts **only when something is broken**, to `NTFY_BASE` (default `https://ntfy.sh`, set in `docker-compose.yml`). Without the key it only logs. There is no recovery message: recovery shows in the log (`[recovered]`) and in `/healthz`.

On public ntfy.sh the topic is a credential: anyone who knows it can read it **and post to it**. These four notices are the only authentic shapes; agent integrations should ignore anything else on the topic.

| title | message |
|---|---|
| `gworkspace-mcp: Google access rejected` | `Google access rejected (<category>). Workspace tools will fail until the owner re-consents and replaces GOOGLE_TOKEN_JSON, or enables the API. Since <iso>.` |
| `gworkspace-mcp: Google Drive failing` | `<n> consecutive Drive failures since <iso>. Last: <category>. Usually transient.` |
| `gworkspace-mcp: Google credential check failed at boot` | `The boot credential check failed. Workspace tools may fail. Since <iso>.` |
| `gworkspace-mcp: Workspace server exited` | `The Google Workspace server process exited. Tool calls fail until the service restarts. Since <iso>.` |

Cadence: at most one delivered push per kind per 30 minutes. While an outage continues and agents keep calling, the same push repeats every 30 minutes per kind to every subscriber. A push that could not be delivered is retried at the next event of that kind, never sooner than a minute later. A failed boot probe pushes once per boot and is not retried.

## Diagnosing it

The container is deliberately unpublished, so `curl` from the VPS host always
fails and that is correct. Go in through docker:

```bash
docker exec gworkspace-mcp node -e "fetch('http://localhost:3014/healthz').then(r=>r.text()).then(console.log)"
docker logs --tail 100 gworkspace-mcp
```

| symptom | meaning |
|---|---|
| `/healthz` 503, `[upstream] the Workspace server exited` | the child died — restart the container; `autoheal` should already have |
| `/healthz` 200 and `CREDENTIAL CHECK FAILED` in the log | the grant is revoked or expired — re-run consent above |
| `/healthz` 200, `google.state` `rejected`, `[alert] Google access rejected (invalid_grant)` | Google refuses the refresh token — re-run consent above. Tool calls say "do not retry". `[drive]` lines carry the cause |
| `/healthz` 200, `google.state` `failing`, `[alert] Google Drive failing` | Drive keeps erroring (5xx, timeouts, network) — usually Google or egress; it clears itself on the next good Drive answer |
| `/healthz` 200, `"tools": 0` | `TOOL_ALLOWLIST` is empty. That exposes nothing, by design |
| every call returns `Failed: …insufficient authentication scopes` | the grant is missing a scope; re-consent with the full set |
| container crash-looping, log names a vault key | that key is absent from the vault |

Rotating `GWORKSPACE_MCP_TOKEN` takes three steps, and skipping any one of them
breaks chat: update the vault, `docker restart gworkspace-mcp` (it is read once
at boot), and update the bearer in Open WebUI's MCP settings.

**The OAuth client must be a Desktop app, not Web** — the consent flow binds a
random loopback port, which a Web client cannot pre-register, and it fails with
`redirect_uri_mismatch`.
