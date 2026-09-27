/**
 * "3 of 18" on a task page, and the tasks either side — in the list the task
 * was opened from.
 *
 * THE PAGE HOLDS THE LIST'S QUESTION, NOT ITS ROWS. A task opened from a board
 * or a list carries that list's query as `list=` (`lib/work.ts` [listParam]),
 * and this asks the engine the same question with `around=<key>`: where the
 * task sits in the WHOLE answer, in the order it is drawn, and which keys are
 * before and after it (`internal/tracker/around.go`). A client that counted its
 * own loaded page stopped dead at row fifty of a four-hundred-task lane and
 * called it the end.
 *
 * ABSENT WHERE THERE IS NOTHING TRUE TO SAY: no `list=` (a pasted link, a
 * mention), or `around: null` — the task has moved off that list since, which
 * is the ordinary case of opening a task somebody just finished. A position
 * past the engine's counting ceiling reads as a count with no place in it.
 */

import { ChevronLeftGlyph, ChevronRightGlyph } from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { aroundParams } from "~/lib/work.ts";

export function ListPosition({ itemKey }: { itemKey: string }) {
  const [list] = useParam("list", "");
  const params = aroundParams(list, itemKey);
  const answer = useQuery("work_items", params ?? undefined, { enabled: params !== null });
  const around = answer.data?.around;
  if (!params || !around) return null;
  const total = `${around.total_hint.toLocaleString()}${around.total_capped ? "+" : ""}`;
  const step = (key: string) => href(["work", key], { list });
  return (
    <nav className="list-position" aria-label="In the list this task was opened from">
      {around.prev ? (
        <a
          className="list-position-step"
          href={step(around.prev)}
          aria-label={`Previous: ${around.prev}`}
        >
          <ChevronLeftGlyph size="sm" aria-hidden="true" />
        </a>
      ) : (
        <span className="list-position-step" aria-hidden="true">
          <ChevronLeftGlyph size="sm" />
        </span>
      )}
      <span className="list-position-where">
        {around.position === null ? `one of ${total}` : `${around.position} of ${total}`}
      </span>
      {around.next ? (
        <a
          className="list-position-step"
          href={step(around.next)}
          aria-label={`Next: ${around.next}`}
        >
          <ChevronRightGlyph size="sm" aria-hidden="true" />
        </a>
      ) : (
        <span className="list-position-step" aria-hidden="true">
          <ChevronRightGlyph size="sm" />
        </span>
      )}
    </nav>
  );
}
