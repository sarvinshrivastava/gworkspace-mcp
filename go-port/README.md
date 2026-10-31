# gworkspace-mcp (Go port)

Google Workspace MCP relay on Streamable HTTP: one policy-scoped identity per
agent, 20 tools across Docs, Sheets, Drive and Calendar. Go-only port of the
TypeScript service — no Node child process; the tool handlers run in-process.

## Build and run

```
docker build -t gworkspace-mcp .
docker run -p 3014:3014 -p 3015:3015 \
  -e SECRETS_MANAGER_URL=... -e SECRETS_MANAGER_TOKEN=... \
  -v gworkspace-state:/state \
  gworkspace-mcp
```

The image is scratch-based: a static binary, CA roots and tzdata, nothing
else. The container health check is the binary itself
(`gworkspace-mcp -healthcheck`).

## Configuration (environment)

| Variable | Default | Purpose |
|----------|---------|---------|
| PORT | 3014 | MCP listener |
| ADMIN_PORT | 3015 | Admin dashboard listener |
| DB_PATH | /state/gworkspace-v2.sqlite | Policy database (hashes only, no secrets) |
| BACKUP_DIR | /state/backups | Daily DB backups |
| HOUSEKEEPING_SCHEDULER | internal | `internal` (24h interval) or `external` (POST /internal/housekeeping) |
| ADMIN_ALLOWED_SOURCES | — | Extra exact source addresses on the admin port |
| ADMIN_ALLOWED_HOSTS | — | Extra Host header names on the admin port |
| NTFY_BASE | https://ntfy.sh | Broken-only push alerts; the topic comes from the vault |
| TOOL_ALLOWLIST | — | Expose only these tools; empty exposes NOTHING |
| SECRETS_MANAGER_URL | http://secret-manager:8000 | Vault base URL |
| SECRETS_MANAGER_FOLDER | Root | Vault folder |
| SECRETS_MANAGER_TOKEN | — | Vault bearer (deleted from the environment after boot) |

## Layout

- `cmd/gworkspace-mcp` — boot wiring
- `internal/gateway` — Streamable HTTP + JSON-RPC boundary, schema validation
- `internal/enforce` — per-agent policy pipeline and folder-confined search
- `internal/tools` — the 20 tool handlers (see PARITY.md)
- `internal/drive` — Google Drive client, OAuth refresh, health state machine
- `internal/policy` — agents, tokens, scopes, audit (SQLite)
- `internal/admin` — dashboard
- `oracle/` — side-by-side TS-vs-Go compatibility rig (see oracle/README.md)
