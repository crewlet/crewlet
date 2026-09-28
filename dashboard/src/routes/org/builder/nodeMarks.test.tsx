/**
 * What a builder node's marks say, and which seat has a live state at all.
 *
 * WHY THESE ARE HERE RATHER THAN ON A SURFACE. `LiveState`'s rule was held by
 * one case on the canvas, and when the owner had the chart stop drawing the
 * trailing slot that case went with it: the rule was suddenly unguarded
 * everywhere, although the table still draws the component and still depends
 * on it. A rule about a COMPONENT belongs to the component, so it survives any
 * surface deciding not to draw it; what a surface does with it is that
 * surface's own case.
 *
 * What these protect: only a saved agent seat has a live state; it is found by
 * the handle the seat RUNS under rather than the name this draft gives it, so
 * a rename does not lose it; a seat this draft created has none, because
 * nothing is running it yet; and a seat that becomes human in the draft, or
 * was already human when it was saved, has none either. Plus the size promise
 * every mark on a row makes: a push changes a word and never a measured box.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import type { AgentRow, SandboxEntry } from "~/protocol/index.ts";
import type { BuilderApi } from "./BuilderContext.tsx";
import type { SeatView } from "./chartModel.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { LiveState, seatKindLabel, unitTypeLabel } from "./nodeMarks.tsx";

afterEach(cleanup);

/** A seat with only what `LiveState` reads decided, and sane nothing else. */
function seat(over: Partial<SeatView> = {}): SeatView {
  return {
    type: "seat",
    key: "seat:dev" as NodeKey,
    name: "Dev",
    kind: "agent",
    handle: "dev",
    saved: { handle: "dev", name: "Dev" },
    running: true,
    placedByRef: false,
    danglingUnitRef: null,
    danglingNote: undefined,
    datadogFallback: false,
    manager: null,
    parent: COMPANY_KEY,
    ...over,
  };
}

/** Only the two fields `LiveState` reads. */
function api(agents: AgentRow[], sandboxes: SandboxEntry[] = []): BuilderApi {
  return { agents, sandboxes } as unknown as BuilderApi;
}

const agent = (over: Partial<AgentRow> = {}): AgentRow =>
  ({ id: "a1", role: "Dev", handle: "dev", activity: "idle", ...over }) as AgentRow;

describe("a seat's live state", () => {
  /*
   * FOUND BY THE HANDLE IT RUNS UNDER, never by the name this draft gives it.
   * A rename is a change to a document nobody has applied yet; the seat on the
   * engine is still called what it was called, so a renamed seat that lost its
   * state would read as a seat that had gone offline when nothing happened.
   */
  test("is found by the saved handle, through a rename in the draft", () => {
    render(
      <LiveState
        api={api([agent({ activity: "working" })])}
        view={seat({ name: "Builder", handle: "builder" })}
      />,
    );
    expect(screen.getByText("working")).toBeDefined();
  });

  /*
   * AND BY THE SAVED NAME when no handle was ever reported, which is what a
   * company written without handles gives every seat.
   */
  test("falls back to the saved name when the seat runs under no handle", () => {
    render(
      <LiveState
        api={api([agent({ handle: undefined, role: "Dev", activity: "stopped" })])}
        view={seat({ saved: { handle: undefined, name: "Dev" }, handle: undefined })}
      />,
    );
    expect(screen.getByText("stopped")).toBeDefined();
  });

  /*
   * A SEAT THIS DRAFT CREATED HAS NONE. Nothing is running it, so the honest
   * answer is silence rather than "offline", which would say the engine had
   * looked and found it down.
   *
   * `running` IS LEFT TRUE HERE ON PURPOSE. The chart sets both for a created
   * seat, so a case that turns both off is answered by whichever check comes
   * first and pins neither: dropping the `saved` guard left it green. What the
   * component promises is that a seat with nothing saved says nothing AND does
   * not read a handle off `null`, which is the state this pins.
   */
  test("says nothing for a seat the saved company does not hold", () => {
    const { container } = render(<LiveState api={api([agent()])} view={seat({ saved: null })} />);
    expect(container.firstChild).toBeNull();
  });

  /* And the pair the chart actually produces for a created seat. */
  test("a created seat, as the chart marks one, says nothing", () => {
    const { container } = render(
      <LiveState api={api([agent()])} view={seat({ saved: null, running: false })} />,
    );
    expect(container.firstChild).toBeNull();
  });

  /*
   * NOR DOES A HUMAN SEAT, whether it was saved as one or becomes one in this
   * draft: a human seat is not something the engine runs, so there is no state
   * for it to be in. `running` is the chart's one answer to both.
   */
  test("says nothing for a seat that is not a running agent seat", () => {
    const { container } = render(
      <LiveState api={api([agent()])} view={seat({ kind: "human", running: false })} />,
    );
    expect(container.firstChild).toBeNull();
  });

  /*
   * A SEAT THE ENGINE DOES NOT REPORT AT ALL is offline, which is a fact about
   * a seat that IS running rather than silence: the saved company holds it, so
   * the engine was asked and had nothing to say.
   */
  test("a saved agent seat the engine never mentions is offline", () => {
    render(<LiveState api={api([])} view={seat()} />);
    expect(screen.getByText("offline")).toBeDefined();
  });

  /*
   * THE ROW'S WORD IS THE ENGINE'S, a seat's coding runs included: the chart
   * folds nothing in. A running box beside an idle row is a row the engine
   * has not yet re-sent, not a seat this client may call busy.
   */
  test("a seat says the engine's word, and a box beside it changes nothing", () => {
    render(
      <LiveState
        api={api([agent({ activity: "needs" })], [{ role: "Dev" } as SandboxEntry])}
        view={seat()}
      />,
    );
    expect(screen.getByText("needs you")).toBeDefined();
  });

  /*
   * THE TABLE WRITES THE WORD OUT, in a slot that keeps its room: a push
   * twice per tool-loop round changes the word and never the row's height.
   * The chart node draws no state at all (see `CanvasView.tsx`), so there is
   * one drawing of this component and it is this one.
   */
  test("the state is the engine's word, in its own slot", () => {
    const { container } = render(
      <LiveState api={api([agent({ activity: "working" })])} view={seat()} />,
    );
    const slot = container.querySelector(".bnode-state");
    expect(slot).not.toBeNull();
    expect(slot!.textContent).toBe("working");
  });
});

describe("what a node is called", () => {
  test("a seat says which of the two kinds it is", () => {
    expect(seatKindLabel({ kind: "agent" })).toBe("Agent seat");
    expect(seatKindLabel({ kind: "human" })).toBe("Human seat");
  });

  /*
   * A UNIT'S TYPE IS THE FOUNDER'S OWN WORD, capitalised in ONE place: written
   * out on both surfaces it drifted, and one draft read "Team" on the card and
   * "team" on the row beside it.
   */
  test("a unit's type is the founder's word, capitalised, or Unit for none", () => {
    expect(unitTypeLabel({ unitType: "team" })).toBe("Team");
    expect(unitTypeLabel({ unitType: "Guild" })).toBe("Guild");
    expect(unitTypeLabel({ unitType: "  " })).toBe("Unit");
    expect(unitTypeLabel({ unitType: "" })).toBe("Unit");
  });
});
