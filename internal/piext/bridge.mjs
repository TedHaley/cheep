#!/usr/bin/env node
// cheep pi-bridge — loads pi coding-agent extensions (https://pi.dev) and
// exposes the custom tools they register as an MCP stdio server, which cheep
// consumes through its normal MCP client.
//
// Scope: TOOLS ONLY. Pi extensions can also hook events, commands, shortcuts,
// renderers, and providers — those need pi's own runtime and are skipped here
// (each skip is reported once on stderr at startup).
//
// Usage: node bridge.mjs <extension-spec>...
//   spec = /abs/path/to/ext.ts | /abs/path/to/package-dir | npm-package-name
//   npm packages are resolved from the node_modules next to this file
//   (populated by `cheep pi add <pkg>`).

import fs from "node:fs";
import path from "node:path";
import readline from "node:readline";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import { execFile } from "node:child_process";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const req = createRequire(path.join(HERE, "package.json"));

const tools = new Map(); // name -> pi tool definition
const skipped = new Map(); // "<ext>.<api>" -> count

// ---- pi ExtensionAPI / ExtensionContext stubs ------------------------------

const ui = new Proxy(
  {},
  {
    get(_, prop) {
      if (prop === "confirm") return async () => false; // headless: never confirm
      if (prop === "input" || prop === "select") return async () => undefined;
      return () => {};
    },
  },
);

const extCtx = {
  cwd: process.cwd(),
  hasUI: false,
  mode: "headless",
  ui,
  isProjectTrusted: () => true,
  isIdle: () => true,
  hasPendingMessages: () => false,
  abort: () => {},
  getContextUsage: () => ({ tokens: 0, percent: 0 }),
  getSystemPrompt: () => "",
};

function piExec(command, args = [], options = {}) {
  return new Promise((resolve) => {
    execFile(
      command,
      args,
      { cwd: options.cwd ?? process.cwd(), timeout: options.timeout ?? 120000, maxBuffer: 16 * 1024 * 1024 },
      (err, stdout, stderr) => {
        resolve({
          stdout: String(stdout ?? ""),
          stderr: String(stderr ?? ""),
          code: err ? (err.code === undefined ? -1 : err.code) : 0,
        });
      },
    );
  });
}

function stubPi(extName) {
  const skip = (prop) => (...args) => {
    skipped.set(`${extName}.${String(prop)}`, (skipped.get(`${extName}.${String(prop)}`) ?? 0) + 1);
    // registerCommand-style APIs return undefined in pi too.
  };
  return new Proxy(
    {},
    {
      get(_, prop) {
        switch (prop) {
          case "registerTool":
            return (def) => {
              if (def && def.name && typeof def.execute === "function") tools.set(def.name, def);
            };
          case "exec":
            return piExec;
          case "getActiveTools":
          case "getAllTools":
            return () => [...tools.keys()];
          case "getCommands":
            return () => [];
          case "getSessionName":
            return () => "cheep";
          case "on":
          case "registerCommand":
          case "registerShortcut":
          case "registerFlag":
          case "registerProvider":
          case "registerMessageRenderer":
          case "registerEntryRenderer":
            return skip(prop);
          default:
            return () => {}; // silent no-op for the long tail
        }
      },
    },
  );
}

// ---- extension discovery + loading -----------------------------------------

// entriesFor expands one spec into concrete extension entry files, honoring a
// pi package manifest ({"pi":{"extensions":[...]}}) and pi's conventional
// extensions/ directory.
function entriesFor(spec) {
  let root = spec;
  if (!fs.existsSync(root)) {
    // npm package installed next to this bridge
    try {
      root = path.dirname(req.resolve(path.posix.join(spec, "package.json")));
    } catch {
      return [req.resolve(spec)]; // package with a "main" entry only
    }
  }
  const st = fs.statSync(root);
  if (st.isFile()) return [root];

  let dirs = null;
  try {
    const pkg = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8"));
    const decl = pkg?.pi?.extensions;
    if (Array.isArray(decl) && decl.length) dirs = decl.map((d) => path.join(root, d));
  } catch {}
  if (!dirs) {
    const conv = path.join(root, "extensions");
    if (fs.existsSync(conv)) dirs = [conv];
    else return [root]; // let import resolve the package/dir entry itself
  }

  const found = [];
  for (const d of dirs) {
    if (!fs.existsSync(d)) continue;
    if (fs.statSync(d).isFile()) {
      found.push(d);
      continue;
    }
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isFile() && /\.(ts|mts|js|mjs|cjs)$/.test(e.name)) found.push(p);
      else if (e.isDirectory()) {
        for (const idx of ["index.ts", "index.mts", "index.js", "index.mjs"]) {
          const ip = path.join(p, idx);
          if (fs.existsSync(ip)) {
            found.push(ip);
            break;
          }
        }
      }
    }
  }
  return found;
}

async function importModule(entry) {
  // jiti (pi's own loader) handles TypeScript without compilation; fall back
  // to plain dynamic import for compiled packages.
  try {
    const { createJiti } = req("jiti");
    const jiti = createJiti(import.meta.url, { interopDefault: true, moduleCache: false });
    return await jiti.import(entry);
  } catch (e) {
    if (/\.(ts|mts)$/.test(entry)) throw new Error(`TypeScript entry needs jiti (cheep pi add installs it): ${e.message}`);
    return await import(pathToFileURL(entry).href);
  }
}

async function loadSpec(spec) {
  const entries = entriesFor(spec);
  if (!entries.length) throw new Error("no extension entries found");
  for (const entry of entries) {
    const mod = await importModule(entry);
    const factory = mod?.default ?? mod;
    if (typeof factory !== "function") {
      process.stderr.write(`pi-bridge: ${entry}: no default-export factory, skipped\n`);
      continue;
    }
    await factory(stubPi(path.basename(entry)));
  }
}

// ---- MCP stdio server -------------------------------------------------------

// TypeBox parameter schemas are JSON-Schema-shaped at runtime; a stringify
// round-trip strips symbols/functions into clean JSON Schema.
function cleanSchema(params) {
  try {
    const s = JSON.parse(JSON.stringify(params));
    if (s && typeof s === "object" && s.type) return s;
  } catch {}
  return { type: "object", properties: {} };
}

function toolList() {
  return [...tools.values()].map((d) => ({
    name: d.name,
    description: String(d.description ?? d.label ?? d.name),
    inputSchema: cleanSchema(d.parameters),
  }));
}

async function callTool(name, args) {
  const def = tools.get(name);
  if (!def) return { content: [{ type: "text", text: `unknown tool ${name}` }], isError: true };
  try {
    const ac = new AbortController();
    const result = await def.execute(`cheep-${Date.now()}`, args ?? {}, ac.signal, undefined, extCtx);
    let content = (result?.content ?? [])
      .filter((c) => c && c.type === "text" && typeof c.text === "string")
      .map((c) => ({ type: "text", text: c.text }));
    if (!content.length) {
      const fallback = typeof result === "string" ? result : JSON.stringify(result?.details ?? result ?? "(no output)");
      content = [{ type: "text", text: fallback }];
    }
    return { content, isError: false };
  } catch (e) {
    return { content: [{ type: "text", text: String(e?.message ?? e) }], isError: true };
  }
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

async function main() {
  const specs = process.argv.slice(2);
  for (const spec of specs) {
    try {
      await loadSpec(spec);
    } catch (e) {
      process.stderr.write(`pi-bridge: failed to load ${spec}: ${e?.message ?? e}\n`);
    }
  }
  for (const [what, n] of skipped) {
    process.stderr.write(`pi-bridge: ${what} needs pi's runtime — skipped (${n}x)\n`);
  }
  process.stderr.write(`pi-bridge: serving ${tools.size} tool(s) from ${specs.length} extension spec(s)\n`);

  const rl = readline.createInterface({ input: process.stdin, terminal: false });
  rl.on("line", async (line) => {
    line = line.trim();
    if (!line) return;
    let msg;
    try {
      msg = JSON.parse(line);
    } catch {
      return;
    }
    const { id, method, params } = msg;
    if (id === undefined || id === null) return; // notification
    switch (method) {
      case "initialize":
        send({
          jsonrpc: "2.0",
          id,
          result: {
            protocolVersion: "2024-11-05",
            capabilities: { tools: {} },
            serverInfo: { name: "cheep-pi-bridge", version: "0.1.0" },
          },
        });
        break;
      case "tools/list":
        send({ jsonrpc: "2.0", id, result: { tools: toolList() } });
        break;
      case "tools/call": {
        const r = await callTool(params?.name, params?.arguments);
        send({ jsonrpc: "2.0", id, result: r });
        break;
      }
      default:
        send({ jsonrpc: "2.0", id, error: { code: -32601, message: `method ${method} not supported` } });
    }
  });
}

main();
