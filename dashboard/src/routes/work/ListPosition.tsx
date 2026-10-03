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
 * CARRIED FROM EVERY WAY A LIST OPENS A TASK: a row's link, and the peek's
 * Open, which takes the list's question from what the list published
 * (`app/frame/PeekHost.tsx`) — so a task reached from a board through its
 * peek still knows where it sits.
 *
 * ABSENT WHERE THERE IS NOTHING TRUE TO SAY: no `list=` (a pasted link, a
 * mention), or `around: null` — the task has moved off that list since, which
 * is the ordinary case of opening a task somebody just finished. A position
 * past the engine's counting ceiling reads as a count with no place in it.
 */

import { ChevronDownGlyph, ChevronUpGlyph } from "@crewlethq/icons/glyphs";
import { anyGridDriving } from "~/app/frame/DataGrid.tsx";
import { useKeymap } from "~/app/keymap.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtExact } from "~/lib/format.ts";
import { aroundParams } from "~/lib/work.ts";

/**
 * `address` is the task's ADDRESS (`itemAddress`): its key, or its id where
 * another task claimed the key first — which is what `around=` places, and
 * what the answer's `prev` and `next` are in turn, so a step from one of two
 * tasks sharing a key never lands on the other.
 */
export function ListPosition({ address }: { address: string }) {
  const [list] = useParam("list", "");
  const nav = useNavigator();
  const params = aroundParams(list, address);
  const answer = useQuery("work_items", params ?? undefined, { enabled: params !== null });
  const around = params ? answer.data?.around : undefined;
  // `j` AND `k` STEP THROUGH THE LIST, as they step through a list's rows —
  // the same two rows of the keymap, because on a task opened from a list the
  // task IS the row. A grid on this page holds them first, so one press is
  // never two actions — ASKED AT THE KEYSTROKE (`anyGridDriving`), since a
  // grid mounting or emptying re-renders nothing here.
  const next = around?.next ?? "";
  const prev = around?.prev ?? "";
  useKeymap({
    "list.next": {
      run: () => next && nav.to(["work", next], { list }),
      when: () => next !== "" && !anyGridDriving(),
    },
    "list.previous": {
      run: () => prev && nav.to(["work", prev], { list }),
      when: () => prev !== "" && !anyGridDriving(),
    },
  });
  if (!params || !around) return null;
  const total = `${fmtExact(around.total_hint)}${around.total_capped ? "+" : ""}`;
  const step = (key: string) => href(["work", key], { list });
  // UP AND DOWN, then the place, as the approved Issue page draws it: the list
  // a task was opened from runs top to bottom, so its neighbours are above and
  // below it rather than beside it.
  return (
    <nav className="list-position" aria-label="In the list this task was opened from">
      {around.prev ? (
        <a
          className="list-position-step"
          href={step(around.prev)}
          aria-label={`Previous task: ${around.prev}`}
          title={`Previous task: ${around.prev} (k)`}
        >
          <ChevronUpGlyph size="sm" aria-hidden="true" />
        </a>
      ) : (
        <span className="list-position-step" aria-hidden="true">
          <ChevronUpGlyph size="sm" />
        </span>
      )}
      {around.next ? (
        <a
          className="list-position-step"
          href={step(around.next)}
          aria-label={`Next task: ${around.next}`}
          title={`Next task: ${around.next} (j)`}
        >
          <ChevronDownGlyph size="sm" aria-hidden="true" />
        </a>
      ) : (
        <span className="list-position-step" aria-hidden="true">
          <ChevronDownGlyph size="sm" />
        </span>
      )}
      <span className="list-position-where">
        {around.position === null ? `one of ${total}` : `${around.position} of ${total}`}
      </span>
    </nav>
  );
}
