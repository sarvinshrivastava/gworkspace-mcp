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

The upstream exposes **118 tools**, and Open WebUI re-lists tools on *every chat
turn* — so exposing all of them puts a large schema payload in front of the model
on every message. `TOOL_ALLOWLIST` narrows it to the ones actually used; empty
means all of them.

## Secrets

Four values in the vault under `Root`:

| Key | What |
|---|---|
| `GWORKSPACE_MCP_TOKEN` | the bearer Open WebUI presents |
| `GOOGLE_CLIENT_ID` | Desktop-app OAuth client id |
| `GOOGLE_CLIENT_SECRET` | its secret |
| `GOOGLE_TOKEN_JSON` | the contents of `~/.config/google-docs-mcp/token.json` |

The refreshed token is written into the `gworkspace-config` volume, so it survives
a deploy. Losing that volume means re-running consent by hand.

**The OAuth client must be a Desktop app, not Web** — the consent flow binds a
random loopback port, which a Web client cannot pre-register, and it fails with
`redirect_uri_mismatch`.
