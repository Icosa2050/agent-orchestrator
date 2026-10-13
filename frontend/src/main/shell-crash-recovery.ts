import type { WebContents } from "electron";

// A renderer that dies again right after loading would otherwise reload forever.
const MAX_RELOADS = 3;
const RELOAD_WINDOW_MS = 60_000;

export type ShellCrashRecoveryOptions = {
	contents: Pick<WebContents, "on" | "reload" | "isDestroyed">;
	now?: () => number;
	log?: (message: string) => void;
};

// The shell renderer is the whole window UI: when its process dies the native
// window keeps showing only its background colour, and nothing brings the UI
// back unless the main process reloads it.
export function installShellCrashRecovery(options: ShellCrashRecoveryOptions): void {
	const { contents } = options;
	const now = options.now ?? Date.now;
	const log = options.log ?? ((message: string) => console.error(message));
	let reloads: number[] = [];

	contents.on("render-process-gone", (_event, details) => {
		if (details.reason === "clean-exit" || contents.isDestroyed()) return;
		const at = now();
		reloads = reloads.filter((time) => at - time < RELOAD_WINDOW_MS);
		const cause = `${details.reason}, exit code ${details.exitCode}`;
		if (reloads.length >= MAX_RELOADS) {
			log(`shell renderer gone (${cause}); not reloading after ${MAX_RELOADS} crashes in ${RELOAD_WINDOW_MS / 1000}s`);
			return;
		}
		reloads.push(at);
		log(`shell renderer gone (${cause}); reloading`);
		contents.reload();
	});
}
