import type { Plugin } from "vite";

/** Hosted-component fixtures run only in a browser dev server, never a package. */
export function hostedFixturePolicyEnabled(command: string, mode: string, noElectron: string | undefined): boolean {
 return command === "serve" && mode === "hosted-renderer-fixture" && noElectron === "1";
}

export function hostedFixturePolicy(): Plugin {
 let enabled = false;
 return {
  name: "hosted-renderer-fixture-policy",
  apply: "serve",
  enforce: "pre",
  configResolved(config) { enabled = hostedFixturePolicyEnabled(config.command, config.mode, process.env.VITE_NO_ELECTRON); },
  transform(code, id) {
   if (!enabled || !id.replaceAll("\\", "/").endsWith("/src/shared/fork-identity.ts")) return;
   return { code: code.replace("export const LOCAL_DISTRIBUTION = true;", "export const LOCAL_DISTRIBUTION = false;"), map: null };
  },
 };
}
