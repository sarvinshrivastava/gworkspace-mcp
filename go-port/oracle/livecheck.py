#!/usr/bin/env python3
# Live-round harness: runs the same 20-tool scenario against the TS and Go
# services on a REAL (dummy) Google account and compares results.
#
# Comparison classes, by construction:
#   EXACT  - a mutation and the readback of the artifact THAT server just
#            created: normalized TS and Go payloads must be identical.
#   SHAPE  - list/search steps. Both servers share one dummy account and
#            there is no delete tool, so artifacts accumulate: exact set
#            equality is impossible. Assert isError=false, parseable content,
#            and that the step finds THIS server's own artifact; compare the
#            normalized entry shape (keys/types) between servers.
#
# Each server runs with its own label (<label>-ts / <label>-go) so its
# artifacts are distinguishable. LIVECHECK-* artifacts accumulate on the
# dummy account (no delete tool exists) — periodic manual binning is the
# owner's cleanup path.
#
#   python3 livecheck.py --ts http://... --go http://... --token <bearer> --label r1
import argparse, json, re, sys, urllib.request, urllib.error

p = argparse.ArgumentParser()
p.add_argument("--ts", default="http://127.0.0.1:13014")
p.add_argument("--go", default="http://127.0.0.1:13024")
p.add_argument("--token", required=True)
p.add_argument("--label", default="r1")
a = p.parse_args()

def call(base, tool, args, mid):
    body = {"jsonrpc": "2.0", "id": mid, "method": "tools/call",
            "params": {"name": tool, "arguments": args}}
    req = urllib.request.Request(base + "/mcp", data=json.dumps(body).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    req.add_header("Authorization", "Bearer " + a.token)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            raw = r.read().decode()
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
    except Exception as e:
        return {"transport_error": str(e)}
    if raw.startswith("event: message\ndata: "):
        raw = raw.split("data: ", 1)[1].rstrip("\n")
    try:
        env = json.loads(raw)
    except Exception:
        return {"unparseable": raw[:400]}
    res = env.get("result", env)
    out = {"isError": res.get("isError", False)}
    texts = [c.get("text", "") for c in res.get("content", []) if c.get("type") == "text"]
    parsed = []
    for t in texts:
        try:
            parsed.append(json.loads(t))
        except Exception:
            parsed.append(t)
    out["content"] = parsed
    return out

IDRE = re.compile(r'(?<![\w.-])[A-Za-z0-9_-]{25,}(?![\w.-])')
TSRE = re.compile(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})')
URLRE = re.compile(r'https://[^\s"]+')
LABELRE = re.compile(r'LIVECHECK-[\w.-]+')

def norm(x):
    if isinstance(x, dict):
        return {k: norm(v) for k, v in sorted(x.items())}
    if isinstance(x, list):
        return [norm(v) for v in x]
    if isinstance(x, str):
        x = LABELRE.sub("<LABEL>", x)
        x = URLRE.sub(lambda m: IDRE.sub("<ID>", m.group(0)), x)
        x = TSRE.sub("<TS>", x)
        x = IDRE.sub("<ID>", x)
        return x
    return x

def shape(x):
    # keys + scalar types only, recursively — set contents irrelevant
    if isinstance(x, dict):
        return {k: shape(v) for k, v in sorted(x.items())}
    if isinstance(x, list):
        return [shape(x[0])] if x else []
    return type(x).__name__

def scenario(base, label):
    L = "LIVECHECK-" + label
    steps = {}
    ids = {}
    def step(key, tool, args):
        r = call(base, tool, args, len(steps) + 1)
        steps[key] = (tool, r)
        return r
    def grab(r, *keys):
        for c in r.get("content", []):
            if isinstance(c, dict):
                for k in keys:
                    if isinstance(c.get(k), str):
                        return c[k]
        return None

    r = step("createFolder", "createFolder", {"name": L})
    folder = grab(r, "id", "folderId")
    ids["folder"] = folder
    r = step("createDocument", "createDocument",
             {"title": L + " doc", "parentFolderId": folder, "initialContent": "alpha beta gamma"})
    doc = grab(r, "documentId", "id")
    ids["doc"] = doc
    step("appendText", "appendText", {"documentId": doc, "text": "\ndelta epsilon"})
    step("insertText", "insertText", {"documentId": doc, "text": "PRE ", "index": 1})
    step("readDocument", "readDocument", {"documentId": doc})
    step("findAndReplace", "findAndReplace", {"documentId": doc, "findText": "beta", "replaceText": "BETA"})
    step("appendMarkdown", "appendMarkdown", {"documentId": doc, "markdown": "## Heading\n\n- one\n- two"})
    step("readback-doc", "readDocument", {"documentId": doc})
    step("listDocuments", "listDocuments", {"query": L, "maxResults": 5})
    step("searchDocuments", "searchDocuments", {"query": L})
    r = step("createSpreadsheet", "createSpreadsheet", {"title": L + " sheet", "parentFolderId": folder})
    sheet = grab(r, "spreadsheetId", "id")
    ids["sheet"] = sheet
    step("writeSpreadsheet", "writeSpreadsheet",
         {"spreadsheetId": sheet, "range": "A1:B2", "values": [["k1", 1], ["k2", 2.5]]})
    step("appendRows", "appendRows", {"spreadsheetId": sheet, "range": "A1:B2", "values": [["k3", True]]})
    step("readback-sheet", "readSpreadsheet", {"spreadsheetId": sheet, "range": "A1:B3"})
    step("listSpreadsheets", "listSpreadsheets", {"query": L})
    if folder:
        step("listDriveFiles", "listDriveFiles", {"folderId": folder})
    step("searchDriveFiles", "searchDriveFiles", {"query": L})
    step("quickAddEvent", "quickAddEvent", {"text": "Livecheck review 2026-11-09 10:00"})
    r = step("createEvent", "createEvent", {
        "summary": L + " event",
        "start": {"dateTime": "2026-11-10T14:00:00+05:30"},
        "end": {"dateTime": "2026-11-10T15:00:00+05:30"}})
    ev2 = grab(r, "id", "eventId")
    if ev2:
        step("updateEvent", "updateEvent", {"eventId": ev2, "summary": L + " event (updated)"})
    step("listEvents", "listEvents",
         {"timeMin": "2026-11-09T00:00:00+05:30", "timeMax": "2026-11-11T00:00:00+05:30", "singleEvents": True})
    return steps, ids

EXACT = ["createFolder", "createDocument", "appendText", "insertText", "readDocument",
         "findAndReplace", "appendMarkdown", "readback-doc", "createSpreadsheet",
         "writeSpreadsheet", "appendRows", "readback-sheet", "quickAddEvent",
         "createEvent", "updateEvent"]
SHAPE = ["listDocuments", "searchDocuments", "listSpreadsheets",
         "listDriveFiles", "searchDriveFiles", "listEvents"]

ts_steps, ts_ids = scenario(a.ts, a.label + "-ts")
go_steps, go_ids = scenario(a.go, a.label + "-go")

fails = 0
for key in EXACT:
    if key not in ts_steps or key not in go_steps:
        print(f"=== {key}: MISSING (ts={key in ts_steps} go={key in go_steps})"); fails += 1; continue
    tr, gr = norm(ts_steps[key][1]), norm(go_steps[key][1])
    # An identical failure is not parity: EXACT steps must have SUCCEEDED.
    if tr.get("isError") or gr.get("isError"):
        fails += 1
        print(f"=== {key}: FAIL isError ts={tr.get('isError')} go={gr.get('isError')}")
        print("  TS :", json.dumps(tr)[:400])
        print("  GO :", json.dumps(gr)[:400])
    elif json.dumps(tr) == json.dumps(gr):
        print(f"=== {key}: SAME")
    else:
        fails += 1
        print(f"=== {key}: DIFF")
        print("  TS :", json.dumps(tr)[:600])
        print("  GO :", json.dumps(gr)[:600])

def contains_own(steps_side, key, label):
    r = steps_side[key][1]
    if r.get("isError"):
        return False, "isError"
    return label in json.dumps(r.get("content", [])), "own artifact absent"

for key in SHAPE:
    if key not in ts_steps or key not in go_steps:
        print(f"=== {key}: MISSING"); fails += 1; continue
    ok_ts, why_ts = contains_own(ts_steps, key, "LIVECHECK-" + a.label + "-ts")
    ok_go, why_go = contains_own(go_steps, key, "LIVECHECK-" + a.label + "-go")
    s_ts, s_go = shape(norm(ts_steps[key][1])), shape(norm(go_steps[key][1]))
    shape_same = json.dumps(s_ts) == json.dumps(s_go)
    if ok_ts and ok_go and shape_same:
        print(f"=== {key}: SHAPE-SAME (own artifact present both sides)")
    else:
        fails += 1
        print(f"=== {key}: SHAPE-DIFF ts_ok={ok_ts}({why_ts}) go_ok={ok_go}({why_go}) shape_same={shape_same}")
        if not shape_same:
            print("  TS shape:", json.dumps(s_ts)[:400])
            print("  GO shape:", json.dumps(s_go)[:400])

print("RESULT: %d/%d pass; ts_ids=%s go_ids=%s" % (
    len(EXACT) + len(SHAPE) - fails, len(EXACT) + len(SHAPE),
    {k: bool(v) for k, v in ts_ids.items()}, {k: bool(v) for k, v in go_ids.items()}))
sys.exit(1 if fails else 0)
