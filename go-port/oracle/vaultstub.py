import json
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRETS = {
    "GWORKSPACE_MCP_TOKEN": "oracle-legacy-token-0123456789abcdef",
    "GOOGLE_CLIENT_ID": "dummy-client-id.apps.googleusercontent.com",
    "GOOGLE_CLIENT_SECRET": "dummy-client-secret",
    "GOOGLE_TOKEN_JSON": "1//dummy-refresh-token",
    "GWORKSPACE_PLR_TOKEN": "oracle-plr-token-0123456789abcdef0123456789",
}

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        # /api/secrets/Root/<key>
        parts = self.path.strip("/").split("/")
        key = parts[-1] if len(parts) >= 4 else ""
        if key in SECRETS:
            body = json.dumps({"value": SECRETS[key]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
    def log_message(self, *a):
        pass

# CI runs this stub on a private bridge network where the gateway
# containers reach it as http://vault:18000; locally it is only hit
# from localhost. The container publishes no ports, so 0.0.0.0 here
# is the bridge interface only.
HTTPServer(("0.0.0.0", 18000), H).serve_forever()
