/**
 * This node's LAST health push, kept through a dropped socket: for a reader
 * whose question is what this node last said about a configuration write,
 * never whether it is up.
 *
 * The store CLEARS the health slice when the socket drops
 * (`Store.setConnected`), because a stale "healthy" is a lie with a timestamp
 * nobody can see. A follower of a write asks something else — where this node
 * stands on the revision the write activated (`nodeStanding`) — and a node
 * that went quiet has not taken back an apply, or a refusal. Read off the live
 * slice, every follower turned what it had been told back into "applying"
 * while the socket was down and in the gap before the first push after a
 * reconnect: the ceiling refused by this node drew the saved figure marked
 * applying again, the charter card the saved words, the lead's after-save
 * strip took back its "Applied." and the org chart said it was still applying
 * a save it had applied.
 *
 * KEPT BY THE TAB, NOT BY THE MOUNT (`StoreState.lastHealth`). Two followers
 * follow a save that outlives them, read from the tab-lived `savedRevision`
 * store: the org chart's note, mounted afresh on the chart, on Teams and on a
 * unit's page, and the Builder's strip, mounted afresh on every return to the
 * Builder. Kept per mount, a follower mounted while the socket was down had
 * no push to read: the note said "Still applying … Showing the previous
 * revision." over a chart drawing the save this node had applied, or in place
 * of its refusal, and the strip took back its "Applied.".
 *
 * Null only before this TAB has had a push: what a follower then says is its
 * write's first standing, never one read off nothing.
 */

import type { EngineHealth } from "~/contract/health.ts";
import { useSlice } from "./store-hooks.ts";

export function useLastHealth(): EngineHealth | null {
  // Under `health`, the slice every frame that moves it emits.
  return useSlice(["health"], (s) => s.lastHealth);
}
