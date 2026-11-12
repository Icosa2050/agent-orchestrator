// Fork consent is explicit, persisted, and checked before SDK initialization.
import { lstatSync, readFileSync } from "node:fs";
import path from "node:path";

export const TELEMETRY_OPT_OUT_FILE = "telemetry_opt_out";
export const TELEMETRY_OPT_IN_FILE = "telemetry_opt_in";

/** A marker that cannot be inspected counts as present, including dangling links. */
export function telemetryOptOutMarkerPresent(dataDir: string): boolean {
	try {
		lstatSync(path.join(dataDir, TELEMETRY_OPT_OUT_FILE));
		return true;
	} catch (error) {
		return (error as NodeJS.ErrnoException).code !== "ENOENT";
	}
}

export function analyticsConsentGranted(dataDir: string): boolean {
	if (telemetryOptOutMarkerPresent(dataDir)) return false;
	try {
		const file = path.join(dataDir, TELEMETRY_OPT_IN_FILE);
		const stat = lstatSync(file);
		return stat.isFile() && !stat.isSymbolicLink() &&
			(process.platform === "win32" || (stat.mode & 0o077) === 0) &&
			readFileSync(file, "utf8") === "1\n";
	} catch {
		return false;
	}
}
