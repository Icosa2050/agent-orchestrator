// @vitest-environment node
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, expect, it } from "vitest";
import { applyHostLaunchProfile, parseHostLaunchProfile } from "./host-launch-profile";
const home = mkdtempSync(path.join(os.tmpdir(), "icosa-profile-"));
afterEach(() => rmSync(path.join(home, ".ao"), { recursive: true, force: true }));
it("defaults to dev state with explicit environment precedence", () => {
 const env = { AO_PORT: "4444" } as NodeJS.ProcessEnv;
 applyHostLaunchProfile(env, home);
 expect(env).toMatchObject({ AO_DATA_DIR: `${home}/.ao/dev/data`, AO_RUN_FILE: `${home}/.ao/dev/running.json`, AO_PORT: "4444" });
});
it("reads owner-only profile and refuses broad permissions", () => {
 mkdirSync(`${home}/.ao`); writeFileSync(`${home}/.ao/icosa-desktop.json`, JSON.stringify({version:1,dataDir:`${home}/.ao/custom`,runFile:`${home}/.ao/custom.json`,port:4002}), {mode:0o600});
 const env = {}; applyHostLaunchProfile(env, home); expect(env).toMatchObject({AO_PORT:"4002",AO_DATA_DIR:`${home}/.ao/custom`});
 chmodSync(`${home}/.ao/icosa-desktop.json`,0o644); expect(()=>applyHostLaunchProfile({},home)).toThrow("owner-only");
});
it.each([null, {}, {version:1,dataDir:"/tmp/data",runFile:`${home}/.ao/run.json`,port:3002}, {version:1,dataDir:`${home}/.ao/../data`,runFile:`${home}/.ao/run.json`,port:3002}])("rejects malformed profiles: %j", value => expect(()=>parseHostLaunchProfile(value,home)).toThrow());
it("fails closed on malformed JSON", () => {
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
