/**
 * What a goal panel CLAIMS, in the two places its raw value means something
 * else on screen.
 *
 * A goal is a quarter's question and the numbers on it are read as answers,
 * so the two failures here are the ones that would be believed: a percentage
 * where nobody has measured anything, and a person named by the slug their
 * row is keyed on rather than by what they are called.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { GoalPanel, goalFacts, Goals } from "./Goals.tsx";
import { fmtDateCompact } from "~/lib/format.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { WorkGoal } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const NOW = Date.parse("2031-04-16T00:00:00Z");

/** NINETEEN DAYS AFTER `NOW` — the interval the audit caught on screen. */
const DUE = "2031-05-05T00:00:00Z";

const goal = (over: Partial<WorkGoal> = {}): WorkGoal => ({
  id: "g-1",
  name: "Ship the fleet console",
  owners: ["ada"],
  health: "on_track",
  created_at: "2031-01-06T00:00:00Z",
  updated_at: "2031-04-16T00:00:00Z",
  created_by: "founder",
  version: 1,
  ...over,
});

// A HANDLE IS THE DATABASE'S WORD FOR A PERSON. Every other surface resolves
// it through the chart, and this one printed the slug — so a goal owned by
// Ada Okonkwo was attributed to `ada-okonkwo` beside a board that calls her
// by name. The chip is also a FILTER, so the value it sends has to stay the
// handle while the word it shows is the name.
test("an owner chip shows a name and filters by the handle", () => {
  const onOwner = vi.fn();
  render(
    <GoalPanel
      goal={goal()}
      now={NOW}
      onOwner={onOwner}
      seatName={(h) => (h === "ada" ? "Ada Okonkwo" : h)}
    />,
  );
  const chip = screen.getByText("Ada Okonkwo");
  expect(screen.queryByText("ada")).toBeNull();
  chip.click();
  expect(onOwner).toHaveBeenCalledWith("ada");
});

// A GOAL WITH NO TARGETS GETS NO BAR, because "nothing has happened" and
// "there is nothing to measure" are different facts — and a 0% bar is the
// first of those stated as a measurement.
test("a goal with nothing to measure draws no bar and says why", () => {
  const { container } = render(
    <GoalPanel goal={goal()} now={NOW} onOwner={() => {}} seatName={(h) => h} />,
  );
  expect(container.querySelector(".meter-fill")).toBeNull();
  expect(screen.getByText(/nothing to measure/)).toBeTruthy();
});

// AN UNJUDGED GOAL WEARS NO HEALTH BADGE. A fresh goal nobody has assessed is
// not "on track", and rendering it as anything is the screen making the
// judgement on somebody's behalf.
test("a goal nobody has judged wears no health badge", () => {
  const { container } = render(
    <GoalPanel
      goal={goal({ health: undefined })}
      now={NOW}
      onOwner={() => {}}
      seatName={(h) => h}
    />,
  );
  expect(container.textContent).not.toContain("On track");
  cleanup();
  render(<GoalPanel goal={goal()} now={NOW} onOwner={() => {}} seatName={(h) => h} />);
  expect(screen.getByText("On track")).toBeTruthy();
});

// A DUE DATE IS A DATE, NOT AN AGE.
//
// This panel read "due in 19d" with the date itself only on a `title`, while the
// tasks under the goal — drawn by this workspace's one due mark — read "Sep 19".
// `relTime` hands a future instant to `inTime`, which counted days for ever and
// never reached an absolute date, so there was nothing on screen to line the two
// up with.
test("a goal's due date is drawn as a date, by the workspace's own due mark", () => {
  const { container } = render(
    <GoalPanel goal={goal({ due_at: DUE })} now={NOW} onOwner={() => {}} seatName={(h) => h} />,
  );
  const mark = container.querySelector(".work-due");
  expect(mark).toBeTruthy();
  expect(mark?.textContent).toContain(fmtDateCompact(DUE, NOW));
  expect(container.textContent).not.toMatch(/in \d+d/);
});

// THE PAGE AND THE RAIL READ THE SAME FACT, so it is asserted where it is BUILT:
// a fix applied to the panel alone would leave the header of every goal page and
// every goal peek still counting days.
test("the Due fact carries the date too, not a countdown", () => {
  const due = goalFacts({ goal: goal({ due_at: DUE }), now: NOW, seatName: (h) => h }).find(
    (f) => f.label === "Due",
  );
  const { container } = render(<>{due?.value}</>);
  expect(container.querySelector(".work-due")).toBeTruthy();
  expect(container.textContent).toContain(fmtDateCompact(DUE, NOW));
  expect(container.textContent).not.toMatch(/in \d+d/);
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/**
 * The goals screen over a socket answering `work_goals` with a fixture.
 *
 * A REAL PROVIDER rather than a module mock of `~/lib/store-hooks.ts`: this
 * screen never imports that module through the alias — it reaches it only inside
 * `useQuery`, by a relative path — so an aliased mock is a second instance of it
 * and the component keeps the real `useClient`, which throws.
 */
function mountGoals(goals: WorkGoal[]) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({ goals });
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Goals />
      </Router>
    </ClientContext.Provider>,
  );
}

// A CAPTION IS A QUALIFIER, NOT A LIST.
//
// `sub` is one ellipsized line, and a goal name is founder prose bounded at 256
// characters — so the joined names shipped as "Console reads honestly under
// uncertainty, Console reads hon…": cut mid-word, naming a goal nobody wrote,
// beside neighbours reading "not archived" and "across every goal on screen".
// The split is bounded by construction and says something the number alone does
// not.
test("the at-risk tile's caption states the split and names no goal", async () => {
  const { container } = mountGoals([
    goal({ id: "g-1", name: "Console reads honestly under uncertainty", health: "at_risk" }),
    goal({ id: "g-2", name: "Every refusal names the field it is about", health: "at_risk" }),
    goal({ id: "g-3", name: "A fleet boots on a cold disk", health: "off_track" }),
    goal({ id: "g-4", name: "The bundle is what the source builds", health: "on_track" }),
  ]);
  const sub = await waitFor(() => {
    const tile = [...container.querySelectorAll(".crewlet-statcard")].find((n) =>
      n.textContent?.includes("At risk or off track"),
    );
    if (!tile) throw new Error("the at-risk tile has not rendered");
    return tile.querySelector(".crewlet-statcard__sub")!;
  });
  expect(sub.textContent).toBe("2 at risk, 1 off track");
  // The names ARE on the screen — in the list below, which is where an unbounded
  // set belongs — so the assertion is scoped to the caption.
  expect(sub.textContent).not.toContain("Console reads");
  expect(screen.getByText("Console reads honestly under uncertainty")).toBeTruthy();
});
