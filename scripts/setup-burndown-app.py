#!/usr/bin/env python3
# file: scripts/setup-burndown-app.py
# version: 1.0.0
"""Bootstrap the burndown-bot GitHub App via the manifest flow.

End-to-end:
  1. Spin up a localhost callback server.
  2. Open the user's browser to GitHub's App-creation page with our manifest.
  3. Catch the redirect's `?code=...` and exchange it via
     POST /app-manifests/{code}/conversions for the App id + PEM.
  4. Open the install URL; poll /app/installations until the user installs.
  5. Print the three secrets the workflow needs.

Requires: PyJWT + cryptography (already present in this venv).
"""

from __future__ import annotations

import http.server
import json
import os
import secrets
import socketserver
import sys
import threading
import time
import urllib.parse
import urllib.request
import webbrowser
from typing import Any

import jwt

CALLBACK_PORT = 8765
CALLBACK_PATH = "/callback"
APP_NAME = "burndown-bot"
HOMEPAGE = "https://github.com/jdfalk/overnight-burndown"

MANIFEST: dict[str, Any] = {
    "name": APP_NAME,
    "url": HOMEPAGE,
    "redirect_url": f"http://127.0.0.1:{CALLBACK_PORT}{CALLBACK_PATH}",
    "description": "Overnight task-burndown agent. Reads issues/checks, opens PRs, never touches workflows or admin.",
    "public": False,
    "default_permissions": {
        "contents": "write",
        "pull_requests": "write",
        "issues": "read",
        "checks": "read",
        "metadata": "read",
    },
    "default_events": [],
}


def http_get_json(url: str, token: str | None = None) -> Any:
    req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json"})
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req) as r:
        return json.loads(r.read())


def http_post_json(url: str, token: str | None = None, body: dict | None = None) -> Any:
    data = json.dumps(body).encode() if body is not None else b""
    req = urllib.request.Request(
        url,
        data=data,
        headers={
            "Accept": "application/vnd.github+json",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req) as r:
        return json.loads(r.read())


class CallbackHandler(http.server.BaseHTTPRequestHandler):
    code_holder: dict[str, str] = {}
    state_expected = ""

    def log_message(self, *_args):
        pass

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != CALLBACK_PATH:
            self.send_response(404)
            self.end_headers()
            return
        params = urllib.parse.parse_qs(parsed.query)
        code = params.get("code", [""])[0]
        state = params.get("state", [""])[0]
        if not code or state != self.state_expected:
            self.send_response(400)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(b"Missing or mismatched code/state. Check the terminal.")
            return
        self.code_holder["code"] = code
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(
            b"<h2>Got it.</h2><p>You can close this tab and return to the terminal.</p>"
        )


def wait_for_code(state: str, timeout_seconds: int = 600) -> str:
    CallbackHandler.state_expected = state
    CallbackHandler.code_holder = {}
    server = socketserver.TCPServer(("127.0.0.1", CALLBACK_PORT), CallbackHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    deadline = time.time() + timeout_seconds
    try:
        while time.time() < deadline:
            if "code" in CallbackHandler.code_holder:
                return CallbackHandler.code_holder["code"]
            time.sleep(0.5)
        raise TimeoutError("No callback received within 10 minutes")
    finally:
        server.shutdown()
        server.server_close()


def make_app_jwt(app_id: int, pem: str) -> str:
    now = int(time.time())
    payload = {"iat": now - 60, "exp": now + 540, "iss": str(app_id)}
    return jwt.encode(payload, pem, algorithm="RS256")


def main() -> int:
    print("=== burndown-bot setup (GitHub App manifest flow) ===\n", file=sys.stderr)

    # Step 1 — open creation page with manifest in POST body via auto-submit form.
    # GitHub doesn't accept the manifest as a query param for new-style URLs, so
    # we serve a tiny HTML page locally that auto-POSTs to GitHub.
    state = secrets.token_urlsafe(16)

    create_html = f"""<!doctype html>
<html><body onload="document.forms[0].submit()">
  <p>Submitting manifest to GitHub...</p>
  <form action="https://github.com/settings/apps/new?state={state}" method="post">
    <input type="hidden" name="manifest" value='{json.dumps(MANIFEST).replace("'", "&#39;")}'>
    <button type="submit">Click if it does not auto-submit</button>
  </form>
</body></html>"""

    create_path = "/tmp/burndown-app-create.html"
    with open(create_path, "w") as f:
        f.write(create_html)
    create_url = f"file://{create_path}"

    print(f"Opening browser → {create_url}", file=sys.stderr)
    print(
        "On the GitHub page that appears, review the App settings and click "
        "'Create GitHub App'.\n",
        file=sys.stderr,
    )
    webbrowser.open(create_url)

    # Step 2 — wait for the redirect.
    code = wait_for_code(state)
    print(f"\nReceived temporary code (length {len(code)}). Exchanging...", file=sys.stderr)

    # Step 3 — exchange code for App credentials.
    conversion = http_post_json(f"https://api.github.com/app-manifests/{code}/conversions")
    app_id = conversion["id"]
    pem = conversion["pem"]
    slug = conversion["slug"]
    print(f"App created: id={app_id} slug={slug}\n", file=sys.stderr)

    # Step 4 — install the App (open install URL, poll for installation).
    install_url = f"https://github.com/apps/{slug}/installations/new"
    print(f"Opening install URL → {install_url}", file=sys.stderr)
    print(
        "Select the 'jdfalk' account, choose 'Only select repositories' and pick "
        "'audiobook-organizer' (and any other repos you want burndown to act on). "
        "Click 'Install'.\n",
        file=sys.stderr,
    )
    webbrowser.open(install_url)

    print("Polling /app/installations for the new installation...", file=sys.stderr)
    jwt_token = make_app_jwt(app_id, pem)
    install_id = None
    for _ in range(120):  # 10 min @ 5s poll
        installs = http_get_json("https://api.github.com/app/installations", token=jwt_token)
        if installs:
            install_id = installs[0]["id"]
            account = installs[0]["account"]["login"]
            print(f"Installation found: id={install_id} account={account}\n", file=sys.stderr)
            break
        time.sleep(5)
    if install_id is None:
        print("Timed out waiting for install. Re-run after installing.", file=sys.stderr)
        return 1

    # Step 5 — emit secrets.
    out = {
        "GH_APP_ID": str(app_id),
        "GH_APP_INSTALLATION_ID": str(install_id),
        "GH_APP_PRIVATE_KEY": pem,
        "_app_slug": slug,
        "_app_html_url": conversion["html_url"],
    }
    out_path = os.environ.get("BURNDOWN_APP_OUT", "/tmp/burndown-app-secrets.json")
    with open(out_path, "w") as f:
        os.chmod(out_path, 0o600) if os.path.exists(out_path) else None
        json.dump(out, f, indent=2)
    os.chmod(out_path, 0o600)
    print(f"Wrote secrets to {out_path} (mode 0600).", file=sys.stderr)
    print(json.dumps({k: v for k, v in out.items() if not k.startswith("_")}, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
