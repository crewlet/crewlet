/**
 * A whole `work_my_work` answer with nothing in it — every block and every
 * count the engine always sends, so a suite that only needs a quiet day
 * answers with the wire shape rather than `{}`.
 */

import type { WorkMyWork } from "~/protocol/types.ts";

export function emptyDay(handle: string, over: Partial<WorkMyWork> = {}): WorkMyWork {
  return {
    handle,
    priorities: [],
    assigned: [],
    asked_of_me: [],
    checklist_items: [],
    collaborating: [],
    watching_recent: [],
    unblocked_recent: [],
    totals: {
      priorities: { total: 0 },
      assigned: { total: 0 },
      asked_of_me: { total: 0 },
      checklist_items: { total: 0 },
      collaborating: { total: 0 },
      watching_recent: { total: 0 },
      unblocked_recent: { total: 0 },
    },
    complete: true,
    ...over,
  };
}
