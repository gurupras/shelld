// Wire-compatibility transcript for a shelld binary.
//
//   node /path/to/shelld/tools/compat/compat.mjs <shelld-binary> <fixture.raw> > out.json
//
// Run it from any directory where the `ws` npm package is installed
// (`npm install ws`): it loads `ws` from the current directory's node_modules.
//
// Boots `shelld run` on a temp socket, drives the HTTP API (including
// malformed requests) and a WebSocket attach session with the `ws` client,
// and prints a normalized transcript. Run it against two daemon builds (or
// shelld and shelld-rs) and diff the outputs.
import { spawn } from "node:child_process";
import { request } from "node:http";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { createRequire } from "node:module";

const require = createRequire(join(process.cwd(), "package.json"));
const WebSocket = require("ws");

const [bin, fixture] = process.argv.slice(2).map((p) => resolve(p));
const dir = mkdtempSync(join(tmpdir(), "shelld-compat-"));
const socket = join(dir, "s.sock");
const out = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function http(method, path, body, headers = {}) {
  return new Promise((resolve) => {
    const hdrs = { ...headers };
    let payload;
    if (body !== undefined) {
      payload = typeof body === "string" ? body : JSON.stringify(body);
      if (!("Content-Type" in hdrs)) hdrs["Content-Type"] = "application/json";
      hdrs["Content-Length"] = Buffer.byteLength(payload);
    }
    const req = request({ socketPath: socket, path, method, headers: hdrs }, (res) => {
      let data = "";
      res.on("data", (c) => (data += c));
      res.on("end", () => resolve({ status: res.statusCode, type: res.headers["content-type"] ?? null, body: data }));
    });
    req.on("error", (e) => resolve({ error: String(e) }));
    if (payload !== undefined) req.write(payload);
    req.end();
  });
}

function normalize(body) {
  let v;
  try {
    v = JSON.parse(body);
  } catch {
    return body;
  }
  const walk = (x) => {
    if (Array.isArray(x)) return x.map(walk).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));
    if (x && typeof x === "object") {
      const o = {};
      for (const [k, val] of Object.entries(x)) {
        if (["pid", "startedAt", "uptime", "createdAt", "totalLines"].includes(k) && typeof val === "number") o[k] = `<${k}>`;
        else if (k === "socket") o[k] = "<socket>";
        else o[k] = walk(val);
      }
      return o;
    }
    return x;
  };
  return walk(v);
}

// Raw scrollback splits lines at PTY read boundaries, which the kernel picks
// per run; those bodies differ between two runs of the same binary.
const VOLATILE = /\/(capture(\?(?!mode=visible)|$)|scroll)/;

async function step(name, method, path, body, headers) {
  const r = await http(method, path, body, headers);
  const volatile = r.status === 200 && VOLATILE.test(path);
  out.push({ name, method, path, status: r.status, type: r.type, body: volatile ? "<volatile>" : normalize(r.body ?? r.error) });
  return r;
}

async function waitFor(fn, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await fn()) return true;
    await sleep(50);
  }
  return false;
}

const daemon = spawn(bin, ["run", "--socket", socket, "--pidfile", join(dir, "p"), "--logfile", join(dir, "l")], {
  stdio: ["ignore", "ignore", "pipe"],
  env: { ...process.env, SHELLD_SOCKET: "", PS1: "$ " },
  cwd: dir,
});
let stderr = "";
daemon.stderr.on("data", (c) => (stderr += c));

try {
  if (!(await waitFor(async () => (await http("GET", "/health")).status === 200))) throw new Error("daemon did not start: " + stderr);

  await step("health", "GET", "/health");
  await step("sessions empty", "GET", "/sessions");
  await step("create session", "POST", "/sessions", { name: "s1" });
  await step("dup session", "POST", "/sessions", { name: "s1" });
  await step("session name empty", "POST", "/sessions", { name: "" });
  await step("session name null", "POST", "/sessions", '{"name":null}');
  await step("session name number", "POST", "/sessions", '{"name":5}');
  await step("session no content-type", "POST", "/sessions", '{"name":"x"}', { "Content-Type": "text/plain" });
  await step("session bad json", "POST", "/sessions", "{nope");
  await step("session exists", "GET", "/sessions/s1");
  await step("session missing", "GET", "/sessions/missing");
  await step("windows missing session", "GET", "/sessions/missing/windows");
  await step("method not allowed", "PUT", "/sessions");
  await step("unknown route", "GET", "/nope");
  await step("trailing slash", "GET", "/sessions/");

  const cmd = `/bin/sh -c "cat ${fixture}; exec sleep 1000"`;
  await step("create window", "POST", "/sessions/s1/windows", { name: "w1", cols: 100, rows: 30, shell: cmd, env: { ZED: "1", ALPHA: "2" } });
  await step("dup window", "POST", "/sessions/s1/windows", { name: "w1" });
  await step("window cols negative", "POST", "/sessions/s1/windows", { name: "w2", cols: -1 });
  await step("window cols float", "POST", "/sessions/s1/windows", '{"name":"w2","cols":80.5}');
  await step("window cols overflow", "POST", "/sessions/s1/windows", { name: "w2", cols: 70000 });
  await step("window shell null", "POST", "/sessions/s1/windows", '{"name":"w2","shell":null}');
  await step("window bad shell", "POST", "/sessions/s1/windows", { name: "w3", shell: "/nonexistent/prog" });
  await step("list windows", "GET", "/sessions/s1/windows");

  // Let the fixture finish printing.
  let last = "";
  await waitFor(async () => {
    const r = await http("GET", "/sessions/s1/windows/w1/frame");
    const same = r.body === last;
    last = r.body;
    await sleep(250);
    return same;
  }, 8000);
  await sleep(1000);

  await step("frame", "GET", "/sessions/s1/windows/w1/frame");
  await step("capture raw default", "GET", "/sessions/s1/windows/w1/capture");
  await step("capture raw 7", "GET", "/sessions/s1/windows/w1/capture?lines=7");
  await step("capture raw 0", "GET", "/sessions/s1/windows/w1/capture?lines=0");
  await step("capture visible", "GET", "/sessions/s1/windows/w1/capture?mode=visible&lines=12");
  await step("capture visible all", "GET", "/sessions/s1/windows/w1/capture?mode=visible&lines=0");
  await step("capture bad lines", "GET", "/sessions/s1/windows/w1/capture?lines=abc");
  await step("scroll", "GET", "/sessions/s1/windows/w1/scroll?offset=3&lines=4");
  await step("scroll past end", "GET", "/sessions/s1/windows/w1/scroll?offset=999999&lines=4");
  await step("window missing", "GET", "/sessions/s1/windows/nope/frame");

  await step("colors ok", "POST", "/sessions/s1/windows/w1/colors", { background: "#1e1e2e", foreground: "c0c0c0", palette: { 1: "#ff0000", 300: "#00ff00", x: "#0000ff" } });
  await step("colors none valid", "POST", "/sessions/s1/windows/w1/colors", { background: "nope" });
  await step("colors palette non-string", "POST", "/sessions/s1/windows/w1/colors", '{"palette":{"1":5}}');
  await step("resize zero", "POST", "/sessions/s1/windows/w1/resize", { cols: 0, rows: 10 });
  await step("resize missing", "POST", "/sessions/s1/windows/w1/resize", { cols: 10 });
  await step("keys missing", "POST", "/sessions/s1/windows/w1/keys", { enter: true });
  await step("keys enter null", "POST", "/sessions/s1/windows/w1/keys", '{"keys":"","enter":null}');
  await step("raw-key", "POST", "/sessions/s1/windows/w1/raw-key", { key: "" });
  await step("inject empty", "POST", "/sessions/s1/windows/w1/inject", { payload: "" });
  await step("inject", "POST", "/sessions/s1/windows/w1/inject", { payload: "hi", priority: 2 });
  await step("inject queue", "GET", "/sessions/s1/windows/w1/inject/queue");
  await step("move missing fields", "POST", "/move-window", { srcSession: "s1" });
  await step("move missing src", "POST", "/move-window", { srcSession: "zz", srcName: "w1", dstSession: "s2" });

  // WebSocket attach: non-upgrade request, then a real session.
  await step("attach plain GET", "GET", "/sessions/s1/windows/w1/attach");

  const ws = new WebSocket("ws+unix://" + socket + ":/sessions/s1/windows/w1/attach");
  const frames = [];
  ws.on("message", (data, isBinary) => frames.push({ isBinary, data: Buffer.from(data) }));
  await new Promise((res, rej) => {
    ws.on("open", res);
    ws.on("error", rej);
  });
  await sleep(200);
  out.push({ name: "frames before handshake", count: frames.length });
  ws.send("\x01" + JSON.stringify({ resize: { cols: 90, rows: 25 } }));
  await waitFor(async () => frames.length >= 2, 3000);
  out.push({ name: "initial frame", isBinary: frames[0]?.isBinary, hex: frames[0]?.data.toString("hex") });
  out.push({ name: "hello", isBinary: frames[1]?.isBinary, text: frames[1]?.data.toString() });
  await step("frame after attach", "GET", "/sessions/s1/windows/w1/frame");

  // Scroll back into history and read the settled status.
  frames.length = 0;
  ws.send("\x01" + JSON.stringify({ scroll: { op: "delta", lines: -7 } }));
  await sleep(400);
  const statuses = frames.filter((f) => !f.isBinary).map((f) => f.data.toString());
  out.push({ name: "status after scroll", last: statuses.at(-1) ?? null });
  ws.send("\x01" + JSON.stringify({ scroll: { op: "exit" } }));
  await sleep(300);

  // A second, smaller client shrinks the PTY; closing it restores the size.
  const ws2 = new WebSocket("ws+unix://" + socket + ":/sessions/s1/windows/w1/attach");
  await new Promise((res) => ws2.on("open", res));
  ws2.send("\x01" + JSON.stringify({ resize: { cols: 60, rows: 20 } }));
  await sleep(300);
  await step("windows with two clients", "GET", "/sessions/s1/windows");
  ws2.close();
  await sleep(300);
  await step("windows after close", "GET", "/sessions/s1/windows");
  await step("frame after resizes", "GET", "/sessions/s1/windows/w1/frame");

  ws.close();
  await sleep(200);
  await step("move", "POST", "/move-window", { srcSession: "s1", srcName: "w1", dstSession: "s2", dstName: "renamed" });
  await step("sessions after move", "GET", "/sessions");
  await step("delete window", "DELETE", "/sessions/s2/windows/renamed");
  await step("delete session", "DELETE", "/sessions/s1");
  await step("delete missing session", "DELETE", "/sessions/nope");
  await step("shutdown", "POST", "/shutdown");
  await sleep(400);
  out.push({ name: "exit code", code: daemon.exitCode });
} finally {
  daemon.kill("SIGKILL");
  rmSync(dir, { recursive: true, force: true });
}
process.stdout.write(JSON.stringify(out, null, 2) + "\n");
