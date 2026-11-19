"""Download benchmark worker for the spike.

Runs on a RunPod load-balancer worker (image: python:3.12-slim). It answers
GET /ping for RunPod and GET /bench?tool=<name> for the caller. Each /bench call
downloads the same file from Hugging Face with one tool into a fresh directory,
times it, deletes it, and returns JSON. Running every tool on the same worker,
in alternating order, is what makes the comparison fair.

Tools: aria2c (16 connections), hf (Hugging Face CLI, Xet transfer),
hf-hp (same with HF_XET_HIGH_PERFORMANCE=1), curl (one stream, the baseline).
"""
import http.server
import json
import os
import shutil
import socketserver
import subprocess
import threading
import time
import urllib.parse

REPO = os.environ.get("BENCH_REPO", "unsloth/Qwen3.8-27B-GGUF")
FILE = os.environ.get("BENCH_FILE", "Qwen3.8-27B-UD-Q4_K_XL.gguf")
TOKEN = os.environ.get("HF_TOKEN", "")
ROOT = "/bench"
URL = "https://huggingface.co/%s/resolve/main/%s" % (REPO, FILE)
state = {"setup": "pending", "setup_log": ""}
lock = threading.Lock()  # one download at a time, or the tools compete


def sh(cmd, env=None):
    full = dict(os.environ)
    full.update(env or {})
    return subprocess.run(cmd, shell=True, capture_output=True, text=True, env=full)


def setup():
    out = []
    for cmd in (
        "apt-get update -qq && apt-get install -y -qq aria2 curl >/dev/null",
        "pip install -q --root-user-action=ignore -U 'huggingface_hub[cli]' hf_xet",
    ):
        r = sh(cmd)
        out.append("$ %s -> %d %s" % (cmd, r.returncode, (r.stderr or "")[-300:]))
    for cmd in ("aria2c --version | head -1", "hf version || huggingface-cli version", "curl --version | head -1", "nproc", "df -h / | tail -1"):
        out.append(sh(cmd).stdout.strip()[:200])
    state["setup_log"] = "\n".join(out)
    state["setup"] = "done"
    print("bench: setup done\n" + state["setup_log"], flush=True)


def command(tool, dest):
    auth = ('--header "Authorization: Bearer %s" ' % TOKEN) if TOKEN else ""
    if tool == "aria2c":
        return ("aria2c -q -x16 -s16 -k1M --file-allocation=none --console-log-level=warn "
                + auth + '-d %s -o model.bin "%s"' % (dest, URL)), {}
    if tool == "curl":
        return "curl -sS -L " + auth.replace("--header", "-H") + '-o %s/model.bin "%s"' % (dest, URL), {}
    if tool in ("hf", "hf-hp"):
        env = {"HF_HOME": dest + "/hf-home", "HF_HUB_DISABLE_PROGRESS_BARS": "1"}
        if TOKEN:
            env["HF_TOKEN"] = TOKEN
        if tool == "hf-hp":
            env["HF_XET_HIGH_PERFORMANCE"] = "1"
        cli = "hf" if shutil.which("hf") else "huggingface-cli"
        return "%s download %s %s --local-dir %s/out" % (cli, REPO, FILE, dest), env
    return None, None


def bench(tool):
    dest = "%s/%s-%d" % (ROOT, tool, int(time.time()))
    cmd, env = command(tool, dest)
    if cmd is None:
        return {"error": "unknown tool " + tool}
    os.makedirs(dest, exist_ok=True)
    with lock:
        start = time.time()
        r = sh(cmd, env)
        secs = time.time() - start
    size = 0
    for base, _, names in os.walk(dest):
        for n in names:
            p = os.path.join(base, n)
            if not os.path.islink(p) and n.endswith((".gguf", ".bin")):
                size = max(size, os.path.getsize(p))
    shutil.rmtree(dest, ignore_errors=True)
    res = {
        "tool": tool, "token": bool(TOKEN), "exit": r.returncode, "seconds": round(secs, 2),
        "bytes": size, "mb_per_s": round(size / 1e6 / secs, 1) if secs else 0,
        "stderr": (r.stderr or "")[-400:],
    }
    print("bench: " + json.dumps(res), flush=True)
    return res


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        if url.path == "/ping":
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
        elif url.path == "/status":
            self.reply(200, state)
        elif url.path == "/bench":
            if state["setup"] != "done":
                self.reply(503, state)
                return
            tool = urllib.parse.parse_qs(url.query).get("tool", [""])[0]
            self.reply(200, bench(tool))
        else:
            self.reply(404, {"error": "not found"})


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


if __name__ == "__main__":
    os.makedirs(ROOT, exist_ok=True)
    threading.Thread(target=setup, daemon=True).start()
    print("bench: listening on :8080 for %s (token: %s)" % (URL, bool(TOKEN)), flush=True)
    Server(("0.0.0.0", 8080), Handler).serve_forever()
