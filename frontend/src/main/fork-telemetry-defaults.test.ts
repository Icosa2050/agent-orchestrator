import { chmod, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, expect, it, vi } from "vitest";
import { analyticsConsentGranted, telemetryOptOutMarkerPresent } from "../shared/telemetry-consent";
import { buildTelemetryBootstrap } from "../shared/telemetry";
import { TelemetryIdentityController } from "./telemetry-identity";
import { TelemetryPolicyAuthority } from "./telemetry-policy-file";
import { DesktopTelemetryController } from "./desktop-telemetry-controller";

const dirs: string[] = [];
async function freshDir() { const dir = await mkdtemp(path.join(os.tmpdir(), "ao-fork-consent-")); dirs.push(dir); return dir; }
afterEach(async () => { vi.unstubAllGlobals(); vi.resetModules(); await Promise.all(dirs.splice(0).map(dir => rm(dir, { recursive: true, force: true }))); });

it("a fresh packaged launch neither loads PostHog nor initializes or sends error telemetry", async () => {
 const dataDir = await freshDir();
 const authority = new TelemetryPolicyAuthority({ dataDir, packagedDefault: true });
 const transportFactory = vi.fn();
 const controller = new DesktopTelemetryController({ authority, transportFactory, environmentAllowsEvents: true, productionEnabled: true });
 await controller.initialize();
 expect(controller.snapshot().eventsEnabled).toBe(false);
 expect(controller.capture({ consentGeneration: controller.snapshot().consentGeneration, kind: "exception", message: "startup error" })).toBe(false);
 expect(transportFactory).not.toHaveBeenCalled();
 const getBootstrap = () => buildTelemetryBootstrap({ AO_DATA_DIR: dataDir, AO_TELEMETRY_RENDERER: "on" }, "1.0.0", "linux", dataDir, true, controller.snapshot());
 expect(await getBootstrap()).toBeNull();
 const sdkLoaded = vi.fn();
 vi.doMock("posthog-js/dist/module.full.no-external", () => { sdkLoaded(); throw new Error("must not load PostHog without consent"); });
 vi.doMock("../renderer/lib/bridge", () => ({ aoBridge: { telemetry: { getBootstrap } } }));
 const network = vi.fn(() => { throw new Error("unexpected network"); });
 vi.stubGlobal("fetch", network);
 const { initTelemetry, captureRendererEvent } = await import("../renderer/lib/telemetry");
 expect(await initTelemetry()).toBe(false);
 await captureRendererEvent("ao.app.loaded", {});
 expect(sdkLoaded).not.toHaveBeenCalled();
 expect(network).not.toHaveBeenCalled();
});

it("explicit choices survive restart, are independent, and the old opt-out marker vetoes analytics", async () => {
 const dataDir = await freshDir();
 const identity = new TelemetryIdentityController(dataDir, () => null);
 const authority = new TelemetryPolicyAuthority({ dataDir, packagedDefault: true });
 await authority.load();
 await authority.setEventsEnabled(true);
 expect(identity.isOptedOut()).toBe(true);
 await identity.setOptedOut(false);
 expect(new TelemetryIdentityController(dataDir, () => null).isOptedOut()).toBe(false);
 const restart = new TelemetryPolicyAuthority({ dataDir, packagedDefault: true });
 expect((await restart.load()).eventsEnabled).toBe(true);
 await restart.setEventsEnabled(false);
 expect(identity.isOptedOut()).toBe(false);
 await writeFile(path.join(dataDir, "telemetry_opt_out"), "1\n");
 expect(identity.isOptedOut()).toBe(true);
 expect(await buildTelemetryBootstrap({ AO_DATA_DIR: dataDir }, "1.0.0", "linux", dataDir, true, restart.snapshot())).toBeNull();
});

it.each(["missing", "malformed", "directory-parent", "symlink", "unsafe-mode", "dangling-opt-out"])("fails closed for %s consent", async kind => {
 let dataDir = await freshDir();
 const consent = path.join(dataDir, "telemetry_opt_in");
 if (kind === "malformed") await writeFile(consent, "true", { mode: 0o600 });
 if (kind === "directory-parent") { const file = path.join(dataDir, "file"); await writeFile(file, ""); dataDir = file; }
 if (kind === "symlink") { const target = path.join(dataDir, "target"); await writeFile(target, "1\n", { mode: 0o600 }); await symlink(target, consent); }
 if (kind === "unsafe-mode") { await writeFile(consent, "1\n", { mode: 0o600 }); if (process.platform !== "win32") await chmod(consent, 0o644); else await writeFile(consent, "bad"); }
 if (kind === "dangling-opt-out") { await writeFile(consent, "1\n", { mode: 0o600 }); await symlink(path.join(dataDir, "missing"), path.join(dataDir, "telemetry_opt_out")); expect(telemetryOptOutMarkerPresent(dataDir)).toBe(true); }
 expect(analyticsConsentGranted(dataDir)).toBe(false);
});
