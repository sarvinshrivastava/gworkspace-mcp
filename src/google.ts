/**
 * The boot-time wiring between the shared Drive client, the Google health
 * tracker and ntfy, in one place so a test can pin it.
 */

import { DriveClient, type DriveCreds } from "./drive/client.js";
import type { GoogleHealth } from "./drive/health.js";
import { makeBroadcaster, notifierFrom } from "./notify.js";

export function wireGoogle(
  google: GoogleHealth,
  opts: { creds: DriveCreds; ntfyTopic: string; ntfyBase: string; fetchImpl?: typeof fetch },
): { drive: DriveClient; onExit: () => void } {
  const fetchImpl = opts.fetchImpl ?? fetch;
  google.setBroadcast(makeBroadcaster(notifierFrom(opts.ntfyTopic, opts.ntfyBase, fetchImpl)));
  return {
    drive: new DriveClient(opts.creds, fetchImpl, 10_000, Date.now, { onOk: (startedAt) => google.ok(startedAt) }),
    onExit: () => google.upstreamDown(),
  };
}
