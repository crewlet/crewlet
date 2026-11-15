/**
 * A BOARD CARD'S MENU SITS BESIDE ITS HOLDER, never on them. The task's own
 * menu is drawn over the card's top-right corner, where the holder's badge
 * is; on a screen with no hover it is always shown, so every card's badge was
 * half covered, a person's initials cut to one letter. Laid out in a real browser, at a lane's
 * width, with the card and menu the board draws.
 */
import "~/test/layout.tsx";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { WorkCard } from "~/components/work.tsx";
import { TaskRowMenu, type TaskActions } from "../taskMenu.tsx";
import { Router } from "~/app/router.tsx";
import type { WorkSummary } from "~/protocol/index.ts";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

const ROW = {
  id: "t-39",
  key: "LEAD-39",
  project: "LEAD",
  title: "Prepare the investor update",
  type: "task",
  status: "todo",
  assignee: "ada",
} as WorkSummary;

/**
 * The actions a menu is built from, none of which this case presses. TYPED AS
 * THE HOOK'S ANSWER, so an action the menu gains is one this fixture is told
 * to carry by the typecheck, rather than a menu that throws in the browser.
 */
const ACTIONS: TaskActions = {
  remove: () => {},
  restore: () => {},
  purge: () => {},
  move: () => {},
  moveHeld: undefined,
  merge: () => {},
  mergeHeld: undefined,
  prioritise: () => {},
  prioritised: () => false,
  prioritiesHeld: undefined,
  removeHeld: undefined,
  restoreHeld: undefined,
  purgeHeld: undefined,
  busy: false,
  dialog: null,
};

const overlap = (a: DOMRect, b: DOMRect) =>
  a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;

test("a card's menu and its holder's badge do not cover each other", () => {
  host = document.createElement("div");
  host.style.width = "280px";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <Router>
        <div className="work-col-slot">
          <WorkCard
            row={ROW}
            href="#/worklet/LEAD-39"
            chrome={{ seatName: () => "Ada Okonkwo", seatKind: () => "human" }}
          />
          <TaskRowMenu row={ROW} actions={ACTIONS} inTrash={false} />
        </div>
      </Router>,
    ),
  );
  const top = host.querySelector<HTMLElement>(".work-card-top")!;
  const badge = top.lastElementChild as HTMLElement;
  const menu = host.querySelector<HTMLElement>(".task-row-menu button")!;
  expect(badge.getBoundingClientRect().width).toBeGreaterThan(0);
  expect(overlap(badge.getBoundingClientRect(), menu.getBoundingClientRect())).toBe(false);
});
