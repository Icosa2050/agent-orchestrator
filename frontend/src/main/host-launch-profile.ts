import { readFileSync, statSync } from "node:fs";
import path from "node:path";

export interface HostLaunchProfile { version: 1; dataDir: string; runFile: string; port: number }
export function parseHostLaunchProfile(value: unknown, home: string): HostLaunchProfile {
 if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid Icosa host launch profile");
 const p = value as Record<string, unknown>;
 const aoHome = path.join(home, ".ao");
 for (const key of ["dataDir", "runFile"] as const) {
  const v = p[key];
  if (typeof v !== "string" || !path.isAbsolute(v) || path.normalize(v) !== v || v.endsWith(path.sep) || !v.startsWith(`${aoHome}${path.sep}`)) throw new Error(`Icosa profile ${key} must be an absolute path under ~/.ao`);
 }
 if (p.version !== 1 || !Number.isInteger(p.port) || Number(p.port) < 1024 || Number(p.port) > 65535) throw new Error("Invalid Icosa profile version or port");
 if (Object.keys(p).some(k => !["version", "dataDir", "runFile", "port"].includes(k))) throw new Error("Unknown Icosa profile field");
 return p as unknown as HostLaunchProfile;
}
/** Finder/deep-link launches use this durable profile; explicit environment wins. */
export function applyHostLaunchProfile(env: NodeJS.ProcessEnv, home: string): void {
 const file = path.join(home, ".ao", "icosa-desktop.json");
 let profile: HostLaunchProfile;
 try {
  const st = statSync(file);
  if ((st.mode & 0o077) !== 0 || (process.getuid && st.uid !== process.getuid())) throw new Error("Icosa host launch profile must be owner-only (chmod 600)");
  profile = parseHostLaunchProfile(JSON.parse(readFileSync(file, "utf8")), home);
 } catch (error) {
  if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  // Approved first-stage host policy; no data/profile migration or file creation.
  profile = { version: 1, dataDir: path.join(home, ".ao/dev/data"), runFile: path.join(home, ".ao/dev/running.json"), port: 3002 };
 }
 env.AO_DATA_DIR ||= profile.dataDir;
 env.AO_RUN_FILE ||= profile.runFile;
 env.AO_PORT ||= String(profile.port);
 console.error(`[Icosa host profile] dataDir=${env.AO_DATA_DIR} runFile=${env.AO_RUN_FILE} port=${env.AO_PORT}`);
}
