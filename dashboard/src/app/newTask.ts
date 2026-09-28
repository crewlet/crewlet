/**
 * Opening the one New task sheet from anywhere in the frame.
 *
 * # One sheet, several doors
 *
 * The sidebar's head `+`, the Projects group's `+`, a screen's own "New task"
 * in the page bar and a board lane's `+` all open the SAME sheet
 * (`routes/work/NewTask.tsx`), which the frame mounts (`app/Shell.tsx`). Four
 * forms would be four ideas of which fields a task is filed with and which
 * project it lands in, and the first one to learn about a new field would be
 * the only one that offered it.
 *
 * A door says only what it KNOWS: the lane a `+` sits in knows the value that
 * puts a task in it, a project's page knows its project. Everything else is
 * the sheet's to decide, the same way for every door.
 *
 * A CONTEXT RATHER THAN AN EVENT ON `window`: a screen rendered outside the
 * frame — a suite — gets a function that does nothing rather than one that
 * reaches a sheet that is not there.
 */

import { createContext, useContext } from "react";
import { STATUSES } from "~/lib/work.ts";

/** What a door knows about the task it opens the sheet for. */
export interface NewTaskPreset {
  /** The project key it lands in: the page or the lane it was opened from. */
  project?: string;
  /** The status it starts in — a board lane's own. */
  status?: string;
  /** The type it is filed as — a lane of a board grouped by type. */
  type?: string;
  /** The priority it is filed at — a lane of a board grouped by priority. */
  priority?: string;
  /** Who it is filed to — a lane of a board grouped by assignee. `""` is nobody. */
  assignee?: string;
  /** The labels it starts with — a lane of a board grouped by label. */
  labels?: string[];
}

export type OpenNewTask = (preset?: NewTaskPreset) => void;

export const NewTaskOpener = createContext<OpenNewTask>(() => {});

/** Open the New task sheet, with whatever the caller's place in the product says. */
export function useOpenNewTask(): OpenNewTask {
  return useContext(NewTaskOpener);
}

/**
 * The statuses nothing is FILED into: work that ended undelivered. See
 * [presetForLane] — the closed group's statuses, and the done group's
 * cancelled one.
 */
const NOT_FILED_INTO = new Set(
  STATUSES.filter((s) => s.group === "closed" || s.value === "cancelled").map((s) => s.value),
);

/**
 * What a board lane's `+` files, by the axis the board is grouped on — or
 * `undefined` when the lane takes no `+` at all.
 *
 * A LANE'S `+` FILES INTO THAT LANE, so it presets exactly the value that
 * puts a task there and is drawn only where the sheet can hold that value.
 * A `+` on a lane it could not file into opened the sheet with nothing preset
 * and filed the task into some OTHER lane — a Bug lane's `+` filed a `task`.
 *
 * - `status`, `type`, `priority`, `project`: the lane's own value.
 * - `status_group`: the group's FIRST status — the one a task entering the
 *   group reaches first (in progress before in review; done before cancelled,
 *   which is not what anybody files work as). Any status of the group lands
 *   the task in the lane, and the sheet shows which one it chose.
 * - `assignee`: the lane's holder, and the unset lane (`""`) is "nobody" —
 *   which the project routes as its own unassigned work, to its default
 *   assignee where it names one; the sheet says so under the field before the
 *   press.
 * - `tag`: that label (a label lane is a task carrying the label, whatever
 *   else it carries), and the Untagged lane nothing. A label the chosen
 *   project does not declare is dropped by the sheet in view of the reader.
 * - A lane of work that ENDED WITHOUT BEING DELIVERED — the Cancelled and
 *   Closed status lanes, the closed status group — takes no `+`, for the due
 *   bands' reason below turned the other way: the sheet COULD hold the value,
 *   but nobody files new work as already abandoned, and a `+` there filed a
 *   brand-new task that started cancelled or closed. Done keeps its `+`: work
 *   finished before anybody tracked it is recorded as done, and the engine
 *   stamps it finished from its first row.
 * - `due:bucket`: only the No due date lane, which a task with no date is
 *   filed into. Every other band is a SPAN — "this week" is seven days,
 *   "overdue" every day before today — and a date picked for the reader would
 *   file something they did not choose.
 * - `unit` and any other axis: none. The sheet has no unit field — a task is
 *   filed into its PROJECT's team — so a `+` there would file into whichever
 *   lane that team is, not the one pressed.
 *
 * THE UNSET LANE ON THE OTHER CLOSED AXES does not exist: there is no task
 * without a status, a type or a project, and "no priority" is the engine's
 * `none`, which is the key that lane already carries.
 */
export function presetForLane(axis: string, key: string): NewTaskPreset | undefined {
  switch (axis) {
    case "status":
      return key && !NOT_FILED_INTO.has(key) ? { status: key } : undefined;
    case "status_group": {
      if (key === "closed") return undefined;
      const first = STATUSES.find((s) => s.group === key);
      return first ? { status: first.value } : undefined;
    }
    case "type":
      return key ? { type: key } : undefined;
    case "priority":
      return key ? { priority: key } : undefined;
    case "project":
      return key ? { project: key } : undefined;
    case "assignee":
      return { assignee: key };
    case "tag":
      return key ? { labels: [key] } : {};
    case "due:bucket":
      return key === "" ? {} : undefined;
    default:
      return undefined;
  }
}
