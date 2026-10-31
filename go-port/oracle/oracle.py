import json, os, sys, urllib.request, urllib.error

OUT_DIR = os.environ.get("ORACLE_OUT_DIR", os.path.dirname(os.path.abspath(__file__)))

TS = "http://127.0.0.1:13014"
GO = "http://127.0.0.1:13024"
TOK = "oracle-legacy-token-0123456789abcdef"

def call(base, method, path, body=None, bearer=None, raw_headers=None):
    url = base + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if method == "POST":
        req.add_header("Content-Type", "application/json")
    if path == "/mcp":
        req.add_header("Accept", "application/json, text/event-stream")
    if bearer is not None:
        req.add_header("Authorization", bearer)
    if raw_headers:
        for k, v in raw_headers.items():
            req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return -1, str(e)

def norm(body):
    if body.startswith("event: message\ndata: "):
        body = body.split("data: ", 1)[1].rstrip("\n")
    try:
        return json.dumps(json.loads(body), sort_keys=True)
    except Exception:
        return body

CASES = []
def case(name, method, path, body=None, bearer=TOK):
    CASES.append((name, method, path, body, bearer))

case("healthz", "GET", "/healthz", None, None)
case("mcp-noauth", "POST", "/mcp", {"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}, None)
case("mcp-wrongbearer", "POST", "/mcp", {"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}, "Bearer wrong")
case("initialize", "POST", "/mcp", {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"oracle","version":"0"}}})
case("tools-list", "POST", "/mcp", {"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}})
case("unknown-tool", "POST", "/mcp", {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gmailSend","arguments":{}}})
case("disabled-tool", "POST", "/mcp", {"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"getFilePermissions","arguments":{}}})
case("strict-unknown-key", "POST", "/mcp", {"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"listDriveFiles","arguments":{"bogus":1}}})
case("null-name", "POST", "/mcp", {"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"createFolder","arguments":{"name":None}}})
case("fractional-maxResults", "POST", "/mcp", {"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"listEvents","arguments":{"maxResults":2.5}}})
case("null-row-values", "POST", "/mcp", {"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"writeSpreadsheet","arguments":{"spreadsheetId":"x","range":"A1","values":[[1],None]}}})
case("whole-args-null", "POST", "/mcp", {"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"listDriveFiles","arguments":None}})
case("nested-unknown-key", "POST", "/mcp", {"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"createEvent","arguments":{"calendarId":"primary","summary":"s","start":{"date":"2026-04-17","junk":1},"end":{"date":"2026-04-18"}}}})
case("missing-required", "POST", "/mcp", {"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"createFolder","arguments":{}}})
case("batch", "POST", "/mcp", [{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}])
case("get-mcp", "GET", "/mcp", None, TOK)
case("args-array", "POST", "/mcp", {"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"listDriveFiles","arguments":[1,2]}})
case("enum-bad", "POST", "/mcp", {"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"readDocument","arguments":{"documentId":"x","format":"yaml"}}})
case("minlength-name", "POST", "/mcp", {"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"createFolder","arguments":{"name":""}}})
case("absent-args", "POST", "/mcp", {"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"createFolder"}})
case("max-over", "POST", "/mcp", {"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"listDriveFiles","arguments":{"maxResults":101}}})

case("multi-error", "POST", "/mcp", {"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"createFolder","arguments":{"name":None,"bogus":1,"bogus2":2}}})
case("multi-error-required", "POST", "/mcp", {"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"createFolder","arguments":{"bogus":1,"bogus2":2}}})
case("drive-call-google401", "POST", "/mcp", {"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"listDriveFiles","arguments":{}}})
case("write-raw-default", "POST", "/mcp", {"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"writeSpreadsheet","arguments":{"spreadsheetId":"x","range":"A1","values":[[1]]}}})
case("search-served-google401", "POST", "/mcp", {"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"searchDriveFiles","arguments":{"query":"x"}}})
case("nested-array-item-type", "POST", "/mcp", {"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"writeSpreadsheet","arguments":{"spreadsheetId":"x","range":"A1","values":[["a",{}]]}}})
DUMPS = {"tools-list", "initialize", "unknown-tool"}
for name, method, path, body, bearer in CASES:
    ts = call(TS, method, path, body, bearer)
    go = call(GO, method, path, body, bearer)
    if name in DUMPS:
        open(f"{OUT_DIR}/ts-{name}.txt", "w").write(ts[1])
        open(f"{OUT_DIR}/go-{name}.txt", "w").write(go[1])
    match = "SAME" if (ts[0] == go[0] and norm(ts[1]) == norm(go[1])) else "DIFF"
    print(f"=== {name}: {match} (ts {ts[0]} / go {go[0]})")
    if match == "DIFF":
        print("  TS :", norm(ts[1])[:600])
        print("  GO :", norm(go[1])[:600])

# raw invalid JSON with proper content-type
import http.client
for label, base in (("ts", "127.0.0.1:13014"), ("go", "127.0.0.1:13024")):
    c = http.client.HTTPConnection(base, timeout=15)
    c.request("POST", "/mcp", body="{not json", headers={"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + TOK})
    r = c.getresponse()
    print(f"=== garbage-json-{label}: {r.status}")
    print("   ", r.read().decode()[:200])
    c.close()
