// @vitest-environment node
import { readFileSync } from "node:fs";
import { expect, it } from "vitest";
import { FORK_IDENTITY, LOCAL_DISTRIBUTION } from "../shared/fork-identity";
it("freezes the fork identity and local acquisition policy", () => {
 expect(LOCAL_DISTRIBUTION).toBe(true);
 expect(FORK_IDENTITY).toMatchObject({name:"Icosa Orchestrator",bundleId:"com.icosa2050.orchestrator",executable:"icosa-orchestrator",scheme:"icosa-orchestrator",updaterCache:"icosa-orchestrator-updater"});
 const auth = readFileSync(new URL("./cloud-auth.ts",import.meta.url),"utf8");
 expect(auth).not.toContain('"ao-app"');
 expect(auth).toContain('"icosa-orchestrator"');
 const updater = readFileSync(new URL("./auto-updater.ts",import.meta.url),"utf8");
 for (const name of ["startAutoUpdates","setUpdateSettings","checkForUpdatesNow","returnToHome","downloadUpdateNow","setMacDifferentialUpdates","quitAndInstallUpdate","ensureUpdatePrefs"]) {
  expect(updater).toMatch(new RegExp(`export async function ${name}\\([\\s\\S]*?\\): Promise<[^\\n]+> \\{\\n  if \\(LOCAL_DISTRIBUTION\\) return;`));
 }
});
