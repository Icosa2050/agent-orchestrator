// @vitest-environment node
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, expect, it, vi } from "vitest";
import { applyHostLaunchProfile, parseHostLaunchProfile } from "./host-launch-profile";
const home = mkdtempSync(path.join(os.tmpdir(), "icosa-profile-"));
afterEach(() => { vi.unstubAllGlobals(); rmSync(path.join(home, ".ao"), { recursive: true, force: true }); });
it("defaults to dev state with explicit environment precedence", () => {
 const env = { AO_PORT: "4444" } as NodeJS.ProcessEnv;
 applyHostLaunchProfile(env, home);
 expect(env).toMatchObject({ AO_DATA_DIR: path.join(home,".ao/dev/data"), AO_RUN_FILE: path.join(home,".ao/dev/running.json"), AO_PORT: "4444" });
});
it.skipIf(process.platform === "win32")("reads owner-only profile and refuses broad permissions", () => {
 mkdirSync(`${home}/.ao`); writeFileSync(`${home}/.ao/icosa-desktop.json`, JSON.stringify({version:1,dataDir:`${home}/.ao/custom`,runFile:`${home}/.ao/custom.json`,port:4002}), {mode:0o600});
 const env = {}; applyHostLaunchProfile(env, home); expect(env).toMatchObject({AO_PORT:"4002",AO_DATA_DIR:`${home}/.ao/custom`});
 chmodSync(`${home}/.ao/icosa-desktop.json`,0o644); expect(()=>applyHostLaunchProfile({},home)).toThrow("owner-only");
});
it.each([null, {}, {version:1,dataDir:"/tmp/data",runFile:`${home}/.ao/run.json`,port:3002}, {version:1,dataDir:`${home}/.ao/../data`,runFile:`${home}/.ao/run.json`,port:3002}])("rejects malformed profiles: %j", value => expect(()=>parseHostLaunchProfile(value,home)).toThrow());
it.skipIf(process.platform === "win32")("fails closed on malformed JSON", () => {
 mkdirSync(`${home}/.ao`); writeFileSync(`${home}/.ao/icosa-desktop.json`, "{", {mode:0o600});
 expect(()=>applyHostLaunchProfile({AO_DATA_DIR:"/override"},home)).toThrow();
});
it("does not pollute CLI stdout with host profile diagnostics", async () => {
 const { vi } = await import("vitest");
 const info = vi.spyOn(console, "info");
 const error = vi.spyOn(console, "error").mockImplementation(() => {});
 try { applyHostLaunchProfile({}, home); expect(info).not.toHaveBeenCalled(); expect(error).toHaveBeenCalledWith(expect.stringContaining("dataDir=")); }
 finally { info.mockRestore(); error.mockRestore(); }
});

interface ProfileVector { name: string; absent?: boolean; valid: boolean; profile?: unknown; raw?: string; env?: Record<string,string> }
const vectors = JSON.parse(readFileSync(new URL("../../../test/fixtures/icosa-host-profile.json", import.meta.url), "utf8")) as ProfileVector[];
const platformVectors = vectors.flatMap(vector => [...new Set([process.platform, "win32" as const])].map(platform => ({...vector,platform})));
it.each(platformVectors)("shared Go/desktop profile vector: $platform/$name", vector => {
 const windows = vector.platform === "win32";
 vi.stubGlobal("process", {...process, platform:vector.platform});
 const expand = (s: string) => s.replaceAll("/", path.sep).replaceAll("__HOME__", home);
 const profile = vector.profile && !Array.isArray(vector.profile) && typeof vector.profile === "object"
  ? Object.fromEntries(Object.entries(vector.profile).map(([key,value]) => [key, typeof value === "string" ? expand(value) : value]))
  : vector.profile;
 const env = Object.fromEntries(Object.entries(vector.env ?? {}).map(([key,value]) => [key,expand(value)])) as NodeJS.ProcessEnv;
 if (!vector.absent) {
  mkdirSync(path.join(home,".ao"));
  writeFileSync(path.join(home,".ao","icosa-desktop.json"), vector.raw ?? JSON.stringify(profile), {mode:0o600});
 }
 if (!vector.valid && !windows) { expect(() => applyHostLaunchProfile(env,home)).toThrow(); return; }
 applyHostLaunchProfile(env,home);
 const p = profile as {dataDir:string;runFile:string;port:number};
 expect(env).toMatchObject({
  AO_DATA_DIR: vector.env?.AO_DATA_DIR ? expand(vector.env.AO_DATA_DIR) : (vector.absent || windows) ? path.join(home,".ao/dev/data") : p.dataDir,
  AO_RUN_FILE: vector.env?.AO_RUN_FILE ? expand(vector.env.AO_RUN_FILE) : (vector.absent || windows) ? path.join(home,".ao/dev/running.json") : p.runFile,
  AO_PORT: vector.env?.AO_PORT || ((vector.absent || windows) ? "3002" : String(p.port)),
 });
});

it("ignores profiles on Windows and preserves explicit environment overrides", async () => {
 const { vi } = await import("vitest");
 mkdirSync(path.join(home,".ao"));
 writeFileSync(path.join(home,".ao","icosa-desktop.json"), "{", {mode:0o644});
 vi.stubGlobal("process", {...process, platform:"win32"});
 try {
  const env = {AO_PORT:"4444",AO_RUN_FILE:"explicit-running.json"} as NodeJS.ProcessEnv;
  applyHostLaunchProfile(env,home);
  expect(env).toMatchObject({AO_DATA_DIR:path.join(home,".ao/dev/data"),AO_RUN_FILE:"explicit-running.json",AO_PORT:"4444"});
 } finally { vi.unstubAllGlobals(); }
});
