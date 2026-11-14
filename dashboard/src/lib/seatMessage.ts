/**
 * What Message does for a seat, and the tracker's ask of it, for the controls
 * that draw them: `components/writes.tsx`' Message button, and the seat page's
 * and the seat panel's "More" (`routes/agents/AskTask.tsx`).
 *
 * HOOKS, NOT CONTROLS, so they live beside the access rules they read
 * (`useWriteAccess.ts`) rather than among the write controls, every one of
 * which `app/writeGate.test.tsx` draws for every reader.
 */

import { useOpenNewTask } from "~/app/newTask.ts";
import { useNavigator } from "~/app/router.tsx";
import { useServicesRun } from "~/lib/services.ts";
import { useDirectSeatAccess, useMessageSeatAccess } from "~/lib/useWriteAccess.ts";

/**
 * What Message does for a seat here, or null where it does nothing.
 *
 * A DIRECT CONVERSATION WHERE ONE CAN BE: the company runs the engine's own
 * chat and the reader holds Chatlet (`useDirectSeatAccess`), so Message opens
 * chat's New direct message with the seat already chosen (`#/chatlet?dm=`),
 * whose Open is the `open_dm` press. A person writes to a seat there as to a
 * colleague, and that is what the word means. It used to file a task that
 * ASKED the seat even there, so what somebody typed as a message became work
 * in a project they never chose; the ask is still offered, as More's "Ask as a
 * task" ([useSeatAsk]), and filing work is Assign task.
 *
 * THE TRACKER'S ASK ELSEWHERE: on a vendor's chat the conversation is the
 * vendor's and this engine holds no room for it, and a reader without Chatlet
 * has none to open, so Message opens the one New task sheet with the seat as
 * assignee and `ask` naming it, and the answer lands in the asker's Inbox.
 * Where the company runs no Worklet either there is nothing to file it in,
 * and no Message.
 *
 * HELD, with the press's own reason, and on the reader's own seat, either way
 * (`useMessageSeatAccess`, `useDirectSeatAccess`): a door onto a dialog that
 * then refused would teach the reason one press too late.
 */
export interface SeatMessage {
  /** `direct`: a direct conversation in Chatlet; `ask`: a task that asks the seat. */
  kind: "direct" | "ask";
  /** Why it is held for this reader, or "". */
  held: string;
  /** Open it: chat's New direct message, or the New task sheet. */
  open: () => void;
}

/** [SeatMessage] for the seat `handle`, for this browser now. */
export function useSeatMessage(handle: string): SeatMessage | null {
  const direct = useDirectSeatAccess(handle);
  const ask = useSeatAsk(handle, true);
  const nav = useNavigator();
  if (direct !== null) {
    return {
      kind: "direct",
      held: direct.can ? "" : direct.reason,
      open: () => nav.to(["chatlet"], { dm: handle }),
    };
  }
  return ask;
}

/**
 * The tracker's ask of a seat: a task that asks it, filed through the New
 * task sheet. `always` answers it wherever the company runs Worklet; without
 * it, only where Message is something else (a direct conversation), which is
 * where it is offered as More's "Ask as a task".
 */
export function useSeatAsk(handle: string, always = false): SeatMessage | null {
  const access = useMessageSeatAccess(handle);
  const direct = useDirectSeatAccess(handle);
  const openNewTask = useOpenNewTask();
  // AN ASK IS FILED AS A TASK, so where the company runs no Worklet there is
  // nothing to file it in (`lib/services.ts`).
  const runs = useServicesRun();
  if (!runs.tracker || (!always && direct === null)) return null;
  return {
    kind: "ask",
    held: access.can ? "" : access.reason,
    open: () => openNewTask({ assignee: handle, ask: handle }),
  };
}
