# Oracle rig

Side-by-side differential harness: boots the TS reference service and the Go
port against a dummy vault stub and compares responses across a boundary and
error-path matrix. Dummy data only — no real Google content is reachable with
the stub's credentials.

```
TS_DIR=/path/to/ts-checkout GO_BIN=/path/to/gworkspace-mcp ./run.sh
```

- `vaultstub.py` — dummy secrets vault on :18000
- `oracle.py` — the case matrix (boundary validation, auth, framing, health)
- `livecheck.py` — 20-tool live scenario for two real endpoints on a DUMMY
  Google account; EXACT steps compare mutation+readback payloads, SHAPE steps
  compare list/search structure (artifacts accumulate: no delete tool exists)
