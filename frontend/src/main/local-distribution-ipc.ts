import type { IpcMain } from "electron";
import { LOCAL_DISTRIBUTION, LOCAL_DISTRIBUTION_MESSAGE } from "../shared/fork-identity";

/** Local remotes/mobile IPC is deliberately outside this hosted-feature gate. */
export function localDistributionIpc(ipc: IpcMain): IpcMain {
 if (!LOCAL_DISTRIBUTION) return ipc;
 const wrapper = Object.create(ipc) as IpcMain;
 wrapper.handle = (channel, listener) => {
  if (channel === "featureBuilds:list") return ipc.handle(channel, () => []);
  if (channel === "featureBuilds:getActive") return ipc.handle(channel, () => null);
  if (["cloud:getSession", "cloud:takePendingSignIn", "cloud:takePendingShareLink"].includes(channel)) return ipc.handle(channel, () => null);
  if (channel === "cloudShare:available") return ipc.handle(channel, () => false);
  if (channel.startsWith("cloud:") || channel.startsWith("cloudCp:") || channel.startsWith("cloudShare:")) {
   return ipc.handle(channel, () => { throw new Error(LOCAL_DISTRIBUTION_MESSAGE); });
  }
  return ipc.handle(channel, listener);
 };
 return wrapper;
}
