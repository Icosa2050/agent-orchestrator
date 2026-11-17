// @vitest-environment node
import { expect, it, vi } from "vitest";
import type { IpcMain } from "electron";
import { localDistributionIpc } from "./local-distribution-ipc";
it("blocks hosted requests before handlers and preserves manual pairing", () => {
 const handlers = new Map<string, (...args: unknown[])=>unknown>();
 const ipc = { handle: (channel: string, fn: (...args: unknown[])=>unknown) => handlers.set(channel,fn) } as unknown as IpcMain;
 const guarded = localDistributionIpc(ipc); const network = vi.fn();
 for (const channel of ["cloud:signIn","cloudShare:publish","cloudCp:request","cloudCp:openStream"]) {
  guarded.handle(channel,network); expect(()=>handlers.get(channel)!()).toThrow("local build");
 }
 guarded.handle("cloud:getSession",network); expect(handlers.get("cloud:getSession")!()).toBeNull();
 guarded.handle("remotes:connect",network); handlers.get("remotes:connect")!(); expect(network).toHaveBeenCalledTimes(1);
});

it("keeps unconditional signed-out startup reads inert", async () => {
 const handlers = new Map<string, (...args: unknown[])=>unknown>();
 const ipc = {handle:(channel:string, fn:(...args:unknown[])=>unknown)=>handlers.set(channel,fn)} as unknown as IpcMain;
 const network = vi.fn(); const guarded = localDistributionIpc(ipc);
 for (const channel of ["cloud:takePendingSignIn","cloud:takePendingShareLink"]) { guarded.handle(channel,network); expect(await handlers.get(channel)!()).toBeNull(); }
 guarded.handle("cloudShare:available",network); expect(await handlers.get("cloudShare:available")!()).toBe(false);
 expect(network).not.toHaveBeenCalled();
});
