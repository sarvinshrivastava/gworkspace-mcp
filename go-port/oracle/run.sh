#!/bin/bash
# Boot the TS reference service and the Go port side by side against the dummy
# vault stub, then run the oracle matrix. Requires a checkout of the TS
# reference at TS_DIR (with dependencies installed) and a built Go binary.
set -u
RIG_DIR="${RIG_DIR:-$(cd "$(dirname "$0")" && pwd)}"
TS_DIR="${TS_DIR:?set TS_DIR to the TS reference checkout}"
GO_BIN="${GO_BIN:?set GO_BIN to the built Go binary}"
UPSTREAM_ENTRY="${UPSTREAM_ENTRY:-$TS_DIR/node_modules/@a-bonus/google-docs-mcp/dist/index.js}"
cd "$RIG_DIR"
ALLOWLIST='readDocument,appendText,insertText,findAndReplace,createDocument,listDocuments,searchDocuments,appendMarkdown,readSpreadsheet,writeSpreadsheet,appendRows,createSpreadsheet,listSpreadsheets,searchDriveFiles,listDriveFiles,createFolder,listEvents,createEvent,updateEvent,quickAddEvent'
pkill -f vaultstub.py 2>/dev/null; pkill -f gworkspace-mcp-go 2>/dev/null; pkill -f "dist/index.js" 2>/dev/null; sleep 0.5
python3 vaultstub.py & VPID=$!
cd "$TS_DIR"
PORT=13014 ADMIN_PORT=13015 DB_PATH="$RIG_DIR/tsdb/db.sqlite" BACKUP_DIR="$RIG_DIR/tsback" \
  SECRETS_MANAGER_URL=http://127.0.0.1:18000 SECRETS_MANAGER_TOKEN=dummy \
  TOOL_ALLOWLIST="$ALLOWLIST" NTFY_BASE=http://127.0.0.1:9 CONFIG_HOME="$RIG_DIR/tsdb/config" TOKEN_DIR="$RIG_DIR/tsdb/config/google-docs-mcp" UPSTREAM_ENTRY="$UPSTREAM_ENTRY" \
  node dist/index.js > "$RIG_DIR/ts.log" 2>&1 & TSPID=$!
PORT=13024 ADMIN_PORT=13025 DB_PATH="$RIG_DIR/godb/db.sqlite" BACKUP_DIR="$RIG_DIR/goback" \
  SECRETS_MANAGER_URL=http://127.0.0.1:18000 SECRETS_MANAGER_TOKEN=dummy \
  TOOL_ALLOWLIST="$ALLOWLIST" NTFY_BASE=http://127.0.0.1:9 \
  "$GO_BIN" > "$RIG_DIR/go.log" 2>&1 & GOPID=$!
# wait for readiness
for i in $(seq 1 45); do
  TS=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:13014/healthz)
  GO=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:13024/healthz)
  if [ "$TS" = "200" ] && [ "$GO" = "200" ]; then echo "READY after ${i}s (ts=$TS go=$GO)"; break; fi
  sleep 1
done
echo "final health: ts=$TS go=$GO"
python3 "$RIG_DIR/oracle.py"
kill $VPID $TSPID $GOPID 2>/dev/null
wait 2>/dev/null
echo DONE
