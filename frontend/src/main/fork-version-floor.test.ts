// @vitest-environment node
import { readFileSync } from "node:fs";
import { expect, it } from "vitest";

const startup = readFileSync(new URL("../main.ts", import.meta.url), "utf8");

it("does not load or invoke the upstream version floor during fork startup", () => {
	// The upstream floor can fetch a remote policy and force a local app to quit.
	// Keep its module unwired even when a future sync updates the implementation.
	expect(startup).not.toMatch(/desktop-version-floor|checkDesktopVersionFloor/);
});

it("retains the independent app-update.yml updater at startup", () => {
	expect(startup).toMatch(/\binitAutoUpdates\(\);/);
	expect(startup).toMatch(/\bstartAutoUpdates\(stateDir\)/);
});
