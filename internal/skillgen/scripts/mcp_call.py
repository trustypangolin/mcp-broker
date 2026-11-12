#!/usr/bin/env python3
"""mcp_call: call a tool on one MCP server, with nothing to install.

Written by mcp-broker for skills that reach a server directly (no broker). Python 3.8+,
standard library only. The server is described by a server.json next to the skill:

  {"name": "...", "transport": "stdio", "command": "...", "args": [...], "cwd": "...",
   "env": {"NAME": "value or ${VAR}"}}
  {"name": "...", "transport": "http", "url": "https://...", "headers": {"Authorization": "Bearer ${TOKEN}"}}

${VAR} is read from this process's environment (and from server.json's own env), $$ is a
literal $. Secrets are never in server.json: set the variables it names before calling.

  python mcp_call.py --server server.json --list
  python mcp_call.py --server server.json --call TOOL --args '{"name": "value"}'
  python mcp_call.py --server server.json --call TOOL --args @body.json
  python mcp_call.py --server server.json --steps steps.json

Each run starts the server and stops it afterwards, so state (an open document, a
selection) does not survive between runs. For work that needs several calls on the same
state, put them in a steps file, run in order on one server:

  [{"tool": "new_document", "arguments": {}}, {"tool": "type_text", "arguments": {"text": "Hi"}}]

Prints the server's JSON-RPC response (a list of them for --steps); the tool's answer is
under "result" (read result.content[].text; result.isError true means the tool failed).
--steps stops at the first failing step unless --keep-going. Exit status: 0 ok, 2 a tool
reported an error, 1 anything else (message on stderr).
"""
import argparse
import json
import os
import queue
import shutil
import subprocess
import sys
import threading
import urllib.error
import urllib.request

PROTOCOL = "2025-06-18"
CLIENT = {"name": "mcp_call", "version": "1"}


def fail(msg):
    sys.stderr.write("mcp_call: " + msg + "\n")
    sys.exit(1)


def expand(s, env, missing):
    out, i = [], 0
    while i < len(s):
        c = s[i]
        if c == "$" and i + 1 < len(s):
            n = s[i + 1]
            if n == "$":
                out.append("$")
                i += 2
                continue
            if n == "{":
                j = s.find("}", i + 2)
                if j < 0:
                    fail("unclosed ${ in %r" % s)
                name = s[i + 2:j]
                if name in env:
                    out.append(env[name])
                else:
                    missing.add(name)
                i = j + 1
                continue
        out.append(c)
        i += 1
    return "".join(out)


def load_server(path):
    with open(path, encoding="utf-8") as f:
        srv = json.load(f)
    missing = set()
    env = dict(os.environ)
    own = {}
    for k, v in (srv.get("env") or {}).items():
        own[k] = expand(v, env, missing)
    env.update(own)
    srv["_env"] = env
    srv["command"] = expand(srv.get("command") or "", env, missing)
    srv["args"] = [expand(a, env, missing) for a in srv.get("args") or []]
    srv["cwd"] = expand(srv.get("cwd") or "", env, missing)
    srv["url"] = expand(srv.get("url") or "", env, missing)
    srv["headers"] = {k: expand(v, env, missing) for k, v in (srv.get("headers") or {}).items()}
    if missing:
        fail("set these environment variables first (server.json uses them): " + ", ".join(sorted(missing)))
    return srv


def answer_server_request(msg):
    """A reply to a request the server sends us: we have no roots, no sampling, and
    decline elicitations (there is no user to ask)."""
    m = msg.get("method")
    if m == "ping":
        return {"jsonrpc": "2.0", "id": msg["id"], "result": {}}
    if m == "roots/list":
        return {"jsonrpc": "2.0", "id": msg["id"], "result": {"roots": []}}
    if m == "elicitation/create":
        return {"jsonrpc": "2.0", "id": msg["id"], "result": {"action": "decline"}}
    return {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": "not supported by mcp_call"}}


class Stdio:
    def __init__(self, srv, timeout):
        cmd = shutil.which(srv["command"]) or srv["command"]
        try:
            self.p = subprocess.Popen([cmd] + srv["args"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                      stderr=subprocess.PIPE, env=srv["_env"], cwd=srv["cwd"] or None)
        except OSError as e:
            fail("cannot start %s: %s (is it installed here? fix the path in server.json)" % (srv["command"], e))
        self.timeout = timeout
        self.lines = queue.Queue()
        self.err = []
        threading.Thread(target=self._read, daemon=True).start()
        threading.Thread(target=self._drain, daemon=True).start()

    def _read(self):
        for raw in self.p.stdout:
            self.lines.put(raw)
        self.lines.put(None)

    def _drain(self):
        for raw in self.p.stderr:
            self.err = (self.err + [raw.decode("utf-8", "replace").rstrip()])[-20:]

    def send(self, msg):
        try:
            self.p.stdin.write((json.dumps(msg) + "\n").encode("utf-8"))
            self.p.stdin.flush()
        except OSError:
            self._died()

    def _died(self):
        fail("the server exited; its last output:\n  " + "\n  ".join(self.err or ["(none)"]))

    def request(self, msg):
        self.send(msg)
        while True:
            try:
                raw = self.lines.get(timeout=self.timeout)
            except queue.Empty:
                fail("no answer from the server within %ds" % self.timeout)
            if raw is None:
                self._died()
            try:
                m = json.loads(raw)
            except ValueError:
                continue  # a server that prints non-JSON to stdout
            if "method" in m and "id" in m:
                self.send(answer_server_request(m))
            elif m.get("id") == msg["id"]:
                return m
            elif "error" in m and m.get("id") is None:
                # The server couldn't read a message; no answer to ours will follow.
                fail("the server could not read the request: %s" % json.dumps(m["error"]))

    def notify(self, msg):
        self.send(msg)

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(timeout=5)
        except Exception:
            self.p.kill()


class HTTP:
    def __init__(self, srv, timeout):
        self.url, self.headers, self.timeout, self.sid = srv["url"], srv["headers"], timeout, None
        self.version = None

    def _post(self, msg):
        h = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
        h.update(self.headers)
        if self.sid:
            h["Mcp-Session-Id"] = self.sid
        if self.version:
            h["MCP-Protocol-Version"] = self.version
        req = urllib.request.Request(self.url, data=json.dumps(msg).encode("utf-8"), headers=h, method="POST")
        try:
            resp = urllib.request.urlopen(req, timeout=self.timeout)
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")[:500]
            hint = " (sign-in or token needed: check the header variables)" if e.code in (401, 403) else ""
            fail("HTTP %d from %s%s: %s" % (e.code, self.url, hint, body))
        except urllib.error.URLError as e:
            fail("cannot reach %s: %s" % (self.url, e.reason))
        self.sid = resp.headers.get("Mcp-Session-Id") or self.sid
        return resp

    def _messages(self, resp):
        ctype = resp.headers.get("Content-Type", "")
        if resp.status == 202:
            return
        if "text/event-stream" in ctype:
            data = []
            for raw in resp:
                line = raw.decode("utf-8", "replace").rstrip("\r\n")
                if line.startswith("data:"):
                    data.append(line[5:].lstrip())
                elif line == "" and data:
                    yield json.loads("\n".join(data))
                    data = []
            if data:
                yield json.loads("\n".join(data))
        else:
            body = resp.read().decode("utf-8")
            if body.strip():
                m = json.loads(body)
                for x in (m if isinstance(m, list) else [m]):
                    yield x

    def request(self, msg):
        resp = self._post(msg)
        for m in self._messages(resp):
            if "method" in m and "id" in m:
                for _ in self._messages(self._post(answer_server_request(m))):
                    pass
            elif m.get("id") == msg["id"]:
                return m
        fail("the server closed the response without answering")

    def notify(self, msg):
        for _ in self._messages(self._post(msg)):
            pass

    def close(self):
        if not self.sid:
            return
        h = dict(self.headers)
        h["Mcp-Session-Id"] = self.sid
        try:
            urllib.request.urlopen(urllib.request.Request(self.url, headers=h, method="DELETE"), timeout=5)
        except Exception:
            pass


def main():
    ap = argparse.ArgumentParser(description="Call a tool on an MCP server described by server.json.")
    ap.add_argument("--server", required=True, help="server.json")
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--list", action="store_true", help="list the server's tools")
    g.add_argument("--call", metavar="TOOL", help="tool to call")
    g.add_argument("--steps", metavar="FILE", help='JSON list of {"tool": ..., "arguments": {...}} run in order on one server')
    ap.add_argument("--args", default="{}", help="tool arguments as JSON, or @file.json")
    ap.add_argument("--keep-going", action="store_true", help="--steps: run every step even after one fails")
    ap.add_argument("--timeout", type=int, default=120, help="seconds to wait for each answer")
    a = ap.parse_args()
    try:
        sys.stdout.reconfigure(encoding="utf-8")
    except AttributeError:
        pass

    args_text = a.args
    if args_text.startswith("@"):
        with open(args_text[1:], encoding="utf-8") as f:
            args_text = f.read()
    try:
        arguments = json.loads(args_text)
    except ValueError as e:
        fail("--args is not valid JSON: %s" % e)
    steps = None
    if a.steps:
        try:
            with open(a.steps, encoding="utf-8") as f:
                steps = json.load(f)
        except (OSError, ValueError) as e:
            fail("--steps: %s" % e)
        if not isinstance(steps, list) or not all(isinstance(s, dict) and s.get("tool") for s in steps):
            fail('--steps must be a JSON list of {"tool": "...", "arguments": {...}}')

    srv = load_server(a.server)
    if srv.get("transport") == "http":
        if not srv["url"]:
            fail("server.json has no url")
        conn = HTTP(srv, a.timeout)
    else:
        if not srv["command"]:
            fail("server.json has no command")
        conn = Stdio(srv, a.timeout)
    try:
        init = conn.request({"jsonrpc": "2.0", "id": 1, "method": "initialize",
                             "params": {"protocolVersion": PROTOCOL, "capabilities": {}, "clientInfo": CLIENT}})
        if "error" in init:
            fail("initialize failed: %s" % json.dumps(init["error"]))
        if isinstance(conn, HTTP):
            conn.version = init.get("result", {}).get("protocolVersion", PROTOCOL)
        conn.notify({"jsonrpc": "2.0", "method": "notifications/initialized"})
        if a.list:
            tools, cursor, n = [], None, 2
            while True:
                params = {"cursor": cursor} if cursor else {}
                r = conn.request({"jsonrpc": "2.0", "id": n, "method": "tools/list", "params": params})
                if "error" in r:
                    print(json.dumps(r, ensure_ascii=False, indent=2))
                    return 1
                tools += r["result"].get("tools", [])
                cursor = r["result"].get("nextCursor")
                n += 1
                if not cursor:
                    break
            print(json.dumps({"jsonrpc": "2.0", "id": 2, "result": {"tools": tools}}, ensure_ascii=False, indent=2))
            return 0
        if steps is not None:
            out, status = [], 0
            for n, s in enumerate(steps, start=2):
                r = conn.request({"jsonrpc": "2.0", "id": n, "method": "tools/call",
                                  "params": {"name": s["tool"], "arguments": s.get("arguments") or {}}})
                out.append(r)
                bad = 1 if "error" in r else 2 if r.get("result", {}).get("isError") else 0
                status = status or bad
                if bad and not a.keep_going:
                    break
            print(json.dumps(out, ensure_ascii=False, indent=2))
            return status
        r = conn.request({"jsonrpc": "2.0", "id": 2, "method": "tools/call",
                          "params": {"name": a.call, "arguments": arguments}})
        print(json.dumps(r, ensure_ascii=False, indent=2))
        if "error" in r:
            return 1
        return 2 if r.get("result", {}).get("isError") else 0
    finally:
        conn.close()


if __name__ == "__main__":
    sys.exit(main())
