import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

test("unavailable notary profile stops before installing dependencies or producing artifacts", () => {
 const root = mkdtempSync(join(tmpdir(), "icosa-notary-"));
 try {
  const scripts = join(root, "frontend/scripts"), bin = join(root, "bin");
  mkdirSync(scripts, { recursive: true }); mkdirSync(bin);
  const recipe = join(scripts, "build-icosa-local.sh");
  writeFileSync(recipe, readFileSync(new URL("./build-icosa-local.sh", import.meta.url)));
  for (const [name, body] of Object.entries({uname: "echo arm64", git: "exit 0", xcrun: "echo unavailable >&2; exit 1", nice: "echo PACKAGING_REACHED; exit 99"})) {
   writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`, {mode: 0o755});
  }
  const result = spawnSync("/bin/bash", [recipe], {encoding: "utf8", env: {...process.env, PATH: `${bin}:${process.env.PATH}`}});
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /Notarization is required/);
  assert.doesNotMatch(result.stdout + result.stderr, /PACKAGING_REACHED/);
 } finally { rmSync(root, {recursive: true, force: true}); }
});
