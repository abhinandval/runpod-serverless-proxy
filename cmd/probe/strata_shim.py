"""Shim that runs in front of the Strata server inside a RunPod load-balancer worker.

RunPod polls GET /ping on the worker's port (200 = ready, 204 = still loading) and
authenticates callers with the Authorization header. Strata has no /ping route,
opens its port only once the model is loaded, and wants its own key in that same
header. This shim listens on the public port, answers /ping, and forwards every
other request to Strata on a private port with Strata's key injected.

Standard library only: it runs on the Python that ships in the Strata image.
"""
import http.client
import http.server
import os
import socketserver
import sys

UPSTREAM = ("127.0.0.1", int(os.environ.get("SHIM_UPSTREAM_PORT", "8090")))
LISTEN_PORT = int(os.environ.get("SHIM_PORT", "8080"))
KEY = os.environ.get("API_KEY", "")
# Dropped in both directions: hop-by-hop headers, lengths (recomputed), and the
# caller's credentials (RunPod already checked them; Strata gets its own key).
SKIP = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te",
    "trailers", "transfer-encoding", "upgrade", "content-length", "host",
    "authorization", "x-api-key",
}
state = {"ready": None}


def log(msg):
    print("shim: " + msg, flush=True)


def engine_ready():
    try:
        conn = http.client.HTTPConnection(*UPSTREAM, timeout=3)
        conn.request("GET", "/v1/models", headers={"Authorization": "Bearer " + KEY})
        resp = conn.getresponse()
        resp.read()
        conn.close()
        ok = resp.status == 200
    except Exception:
        ok = False
    if ok != state["ready"]:
        state["ready"] = ok
        log("engine ready" if ok else "engine not ready yet")
    return ok


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def handle_any(self):
        if self.path.split("?")[0] == "/ping":
            self.send_response(200 if engine_ready() else 204)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        headers = {k: v for k, v in self.headers.items() if k.lower() not in SKIP}
        headers["Authorization"] = "Bearer " + KEY
        headers["x-api-key"] = KEY
        try:
            conn = http.client.HTTPConnection(*UPSTREAM, timeout=900)
            conn.request(self.command, self.path, body=body, headers=headers)
            resp = conn.getresponse()
        except Exception as exc:
            msg = ('{"error":{"type":"engine_unavailable","message":"%s"}}' % str(exc).replace('"', "'")).encode()
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            log("%s %s -> 503 (%s)" % (self.command, self.path, exc))
            return

        # Stream the body through unbuffered and end it by closing the connection,
        # so SSE events reach the client as they are produced.
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in SKIP:
                self.send_header(k, v)
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        sent = 0
        try:
            while True:
                chunk = resp.read1(65536)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
                sent += len(chunk)
        except Exception as exc:
            log("%s %s stream ended early: %s" % (self.command, self.path, exc))
        finally:
            conn.close()
        log("%s %s -> %d (%d bytes)" % (self.command, self.path, resp.status, sent))

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_OPTIONS = do_HEAD = handle_any


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    request_queue_size = 128


if __name__ == "__main__":
    if not KEY:
        log("API_KEY is not set")
        sys.exit(1)
    log("listening on :%d, forwarding to %s:%d" % (LISTEN_PORT, UPSTREAM[0], UPSTREAM[1]))
    Server(("0.0.0.0", LISTEN_PORT), Handler).serve_forever()
