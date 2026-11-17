import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { DICTATION_HELPER_EXECUTABLE } from "./build-dictation-helper.mjs";

test("native CI verifies the executable path actually built by the fork", () => {
 const workflow = readFileSync(new URL("../../.github/workflows/frontend.yml", import.meta.url), "utf8");
 for (const arch of ["x86_64", "arm64"]) assert.ok(workflow.includes(`lipo "${DICTATION_HELPER_EXECUTABLE}" -verify_arch ${arch}`));
 assert.ok(!workflow.includes("dictation-helper/Orchestrator.app/"));
});
