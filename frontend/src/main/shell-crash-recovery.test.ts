import { describe, expect, it, vi } from "vitest";
import { installShellCrashRecovery } from "./shell-crash-recovery";

function setup() {
	let gone: ((event: unknown, details: { reason: string; exitCode: number }) => void) | undefined;
	let now = 1_000_000;
	let destroyed = false;
	const reload = vi.fn();
	const log = vi.fn();
	installShellCrashRecovery({
		contents: {
			on: vi.fn((event: string, listener: typeof gone) => {
				if (event === "render-process-gone") gone = listener;
			}),
			reload,
			isDestroyed: () => destroyed,
		} as never,
		now: () => now,
		log,
	});
	return {
		advance: (ms: number) => {
			now += ms;
		},
		crash: (reason = "crashed") => gone?.({}, { reason, exitCode: 5 }),
		destroy: () => {
			destroyed = true;
		},
		log,
		reload,
	};
}

describe("installShellCrashRecovery", () => {
	it("reloads the shell when its renderer crashes", () => {
		const shell = setup();
		shell.crash("crashed");
		expect(shell.reload).toHaveBeenCalledTimes(1);
	});

	it.each(["oom", "killed", "abnormal-exit", "launch-failed", "integrity-failure", "memory-eviction"])(
		"reloads after a %s exit",
		(reason) => {
			const shell = setup();
			shell.crash(reason);
			expect(shell.reload).toHaveBeenCalledTimes(1);
		},
	);

	it("leaves a clean exit alone", () => {
		const shell = setup();
		shell.crash("clean-exit");
		expect(shell.reload).not.toHaveBeenCalled();
	});

	it("does not reload destroyed contents", () => {
		const shell = setup();
		shell.destroy();
		shell.crash();
		expect(shell.reload).not.toHaveBeenCalled();
	});

	it("stops reloading a renderer that keeps dying right after it loads", () => {
		const shell = setup();
		for (let i = 0; i < 5; i += 1) {
			shell.crash();
			shell.advance(1_000);
		}
		expect(shell.reload).toHaveBeenCalledTimes(3);
		expect(shell.log).toHaveBeenLastCalledWith(expect.stringContaining("not reloading"));
	});

	it("recovers again once earlier crashes have aged out", () => {
		const shell = setup();
		for (let i = 0; i < 4; i += 1) shell.crash();
		expect(shell.reload).toHaveBeenCalledTimes(3);
		shell.advance(60_001);
		shell.crash();
		expect(shell.reload).toHaveBeenCalledTimes(4);
	});
});
