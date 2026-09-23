import type { QuitPrompt, StreamerView } from "../types";

// Activities that represent in-flight work worth warning about before stopping
// the daemon: an active recording (including one lagging behind the live edge)
// vs. post-processing (remux / move). Mirrors the daemon's vlstate Activity
// constants.
//
// Both the app-quit flow and the update install flow stop the daemon, and both
// have to tell the user what that will interrupt — so the counting lives here
// rather than in either of them.
const ACTIVE_RECORDING = new Set<string>(["recording", "lagging"]);
const ACTIVE_PROCESSING = new Set<string>([
  "processing",
  "moving",
  "moving_retrying",
]);

/** countPendingWork summarizes what stopping the daemon right now would hit. */
export function countPendingWork(streamers: StreamerView[]): QuitPrompt {
  let recording = 0;
  let processing = 0;
  for (const s of streamers) {
    if (ACTIVE_RECORDING.has(s.activity)) recording++;
    else if (ACTIVE_PROCESSING.has(s.activity)) processing++;
  }
  return { recording, processing };
}

/** hasPendingWork is the "should we warn at all?" predicate. */
export function hasPendingWork(p: QuitPrompt): boolean {
  return p.recording > 0 || p.processing > 0;
}
