#!/usr/bin/env node
// Builds the plugin marketplace of one Litopys installation: a copy of this folder with the
// installation's address written into the plugin. claude.ai attaches a plugin's connector only at a fixed address,
// not a template, so the address is stamped here and the source keeps none. It copies the working tree as it is:
// build a release from a clean checkout.
//
//   node build-marketplace.mjs --url https://memory.example.org --out <empty or new folder>
import { createHash } from "node:crypto";
import { cpSync, existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, writeFileSync } from "node:fs";
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

// serverAddress: the base address of an installation, without a trailing slash. Only https: claude.ai does not
// connect to anything else.
export function serverAddress(raw) {
  let u;
  try {
    u = new URL(raw);
  } catch {
    throw new Error(`not an address: ${raw}`);
  }
  if (u.protocol !== "https:") throw new Error(`the address must be https: ${raw}`);
  if (u.username || u.password || /[?#]/.test(raw)) throw new Error(`the address must have no credentials, query or fragment: ${raw}`);
  // the plugin adds /v1/mcp itself; a connector's address pasted here would break the whole installation
  if (/\/(v1|mcp|v1\/mcp)\/*$/i.test(u.pathname)) throw new Error(`give the server's base address, without /mcp, /v1 or /v1/mcp: ${raw}`);
  return u.href.replace(/\/+$/, "");
}

// realOf: the real path of a folder that may not exist yet, so a junction or a symlink cannot hide where it is
function realOf(path) {
  let existing = resolve(path);
  const rest = [];
  while (!existsSync(existing) && dirname(existing) !== existing) {
    rest.unshift(basename(existing));
    existing = dirname(existing);
  }
  return join(realpathSync(existing), ...rest);
}

export function buildMarketplace({ url, out, source = HERE }) {
  const address = serverAddress(url);
  const target = resolve(out);
  const inside = relative(realOf(source), realOf(target));
  // another drive on Windows gives an absolute path: that is outside too
  if (inside !== ".." && !inside.startsWith(`..${sep}`) && !isAbsolute(inside)) throw new Error(`the output folder must be outside ${source}`);
  if (existsSync(target) && readdirSync(target).length) throw new Error(`the output folder is not empty: ${target}`);
  mkdirSync(target, { recursive: true });
  cpSync(join(source, ".claude-plugin"), join(target, ".claude-plugin"), { recursive: true });
  const from = join(source, "litopys");
  cpSync(from, join(target, "litopys"), { recursive: true, filter: (f) => f !== join(from, "test") });

  const mcpFile = join(target, "litopys", ".mcp.json");
  const mcp = JSON.parse(readFileSync(mcpFile, "utf8"));
  mcp.mcpServers.litopys.url = `${address}/v1/mcp`;
  writeFileSync(mcpFile, JSON.stringify(mcp, null, 2) + "\n");
  // the hooks and the CLI read the same address (lib/core.mjs); with LITOPYS_URL elsewhere the plugin refuses to work
  writeFileSync(join(target, "litopys", "install.json"), JSON.stringify({ url: address }, null, 2) + "\n");
  // Claude Code updates a plugin only when its version changes: without this, a rebuild for a new address leaves
  // clients on the old server while reporting "already at the latest version"
  const manifestFile = join(target, "litopys", ".claude-plugin", "plugin.json");
  const manifest = JSON.parse(readFileSync(manifestFile, "utf8"));
  manifest.version = `${manifest.version}+${createHash("sha256").update(address).digest("hex").slice(0, 8)}`;
  writeFileSync(manifestFile, JSON.stringify(manifest, null, 2) + "\n");
  return { address, out: target, version: manifest.version };
}

// realpath: started through a junction or a symlink, argv and import.meta.url name the file differently
function startedDirectly() {
  try {
    return !!process.argv[1] && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false; // argv[1] is not a file: this module was imported
  }
}

if (startedDirectly()) {
  const arg = (name) => {
    const i = process.argv.indexOf(`--${name}`);
    return i > 0 ? process.argv[i + 1] : undefined;
  };
  try {
    if (!arg("url") || !arg("out")) throw new Error("usage: build-marketplace.mjs --url https://<address> --out <folder>");
    const r = buildMarketplace({ url: arg("url"), out: arg("out") });
    process.stdout.write(`marketplace for ${r.address}, plugin ${r.version}, in ${r.out}\n`);
  } catch (e) {
    process.stderr.write(`build-marketplace: ${e.message}\n`);
    process.exitCode = 1;
  }
}
