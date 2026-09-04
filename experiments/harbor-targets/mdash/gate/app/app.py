#!/usr/bin/env python3
"""Two-endpoint toy HTTP app, stdlib only.

  POST /login       -> {"token": "..."}            (login_endpoint.py)
  GET  /notes/<id>   -> {"note": "..."}             (notes_endpoint.py, has the bug)

Usage: python3 app.py [port] [--fixed]
  --fixed applies the one-line authorization fix in notes_endpoint.py.
  Same effect as setting MDASH_FIXED=1 in the environment.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from login_endpoint import handle_login
from notes_endpoint import handle_notes

FIXED = os.environ.get("MDASH_FIXED") == "1" or "--fixed" in sys.argv
sessions = {}  # token -> user_id, shared for the life of the process


class Handler(BaseHTTPRequestHandler):
    def _reply(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path == "/login":
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length) or b"{}")
            status, payload = handle_login(body, sessions)
            self._reply(status, payload)
        else:
            self._reply(404, {"error": "no such route"})

    def do_GET(self):
        if self.path.startswith("/notes/"):
            try:
                note_id = int(self.path[len("/notes/"):])
            except ValueError:
                self._reply(400, {"error": "bad note id"})
                return
            auth = self.headers.get("Authorization", "")
            token = auth[len("Bearer "):] if auth.startswith("Bearer ") else ""
            status, payload = handle_notes(note_id, token, sessions, FIXED)
            self._reply(status, payload)
        else:
            self._reply(404, {"error": "no such route"})

    def log_message(self, fmt, *args):
        pass  # keep stdout clean for the proving plugin


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 and sys.argv[1].isdigit() else 8000
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"mdash toy app listening on 127.0.0.1:{port} fixed={FIXED}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
