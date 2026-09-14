/**
 * Leaving an entry that holds work nobody could bring back.
 *
 * What these protect: while a leave guard holds, every move to another entry
 * is put to it first (a push from code, a link, Back, Forward), a held move
 * is undone at once and made again only when the guard says to leave, a
 * replace is never held because it stays on the entry, and a reload or a
 * closed tab is asked about while anything holds. Back cannot be prevented,
 * only undone, so these drive the real history the way a browser does.
 */

import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  Router,
  useLeaveGuard,
  useNavigator,
  useRoute,
  useUnloadGuard,
  type LeaveGuard,
  type Navigator,
} from "./router.tsx";

let nav: Navigator | null = null;
let shown = "";

/** A guard that holds every move and keeps the last `leave` it was handed. */
function holdAll(): { guard: LeaveGuard; asked: string[]; leave: () => void } {
  const probe = {
    asked: [] as string[],
    leave: () => {},
    guard: ((to, leave) => {
      probe.asked.push(to.hash);
      probe.leave = leave;
      return true;
    }) as LeaveGuard,
  };
  return probe;
}

function Probe({ guard, unload = false }: { guard: LeaveGuard | null; unload?: boolean }) {
  nav = useNavigator();
  shown = useRoute().hash;
  useLeaveGuard(guard);
  useUnloadGuard(unload);
  return null;
}

function mount(guard: LeaveGuard | null, unload = false) {
  const ui = (g: LeaveGuard | null, u: boolean) => (
    <Router>
      <Probe guard={g} unload={u} />
    </Router>
  );
  const view = render(ui(guard, unload));
  return { rerender: (g: LeaveGuard | null, u = false) => view.rerender(ui(g, u)) };
}

/**
 * Waits until the page shows `hash` and the history agrees, however many
 * traversals the browser queued to get there (a held move is two: the move
 * and its undo), then a little longer, so an undo that was still to come
 * would have arrived and failed the assertion that follows.
 */
async function settleOn(hash: string) {
  await waitFor(() => {
    expect(location.hash).toBe(hash);
    expect(shown).toBe(hash);
  });
  await act(() => new Promise<void>((resolve) => setTimeout(resolve, 30)));
  expect(location.hash).toBe(hash);
  expect(shown).toBe(hash);
}

beforeEach(() => {
  history.replaceState(null, "", "#/org");
});

afterEach(() => {
  cleanup();
  nav = null;
  history.replaceState(null, "", "#/");
});

test("a push from code waits for the guard, and leave makes it", async () => {
  const probe = holdAll();
  mount(probe.guard);
  act(() => nav!.to(["people"]));
  expect(probe.asked).toEqual(["#/people"]);
  expect(location.hash).toBe("#/org");
  expect(shown).toBe("#/org");
  act(() => probe.leave());
  expect(location.hash).toBe("#/people");
  expect(shown).toBe("#/people");
  // A section is a push too.
  act(() => nav!.section("lens", "builder"));
  expect(probe.asked).toEqual(["#/people", "#/people?lens=builder"]);
  expect(location.hash).toBe("#/people");
});

test("a replace stays on the entry, so no guard is asked", () => {
  const probe = holdAll();
  mount(probe.guard);
  act(() => nav!.filter({ unit: "Sales" }));
  expect(probe.asked).toEqual([]);
  expect(location.hash).toBe("#/org?unit=Sales");
});

test("Back and Forward are undone while held, and leave makes each", async () => {
  const view = mount(null);
  act(() => nav!.to(["people"]));
  act(() => nav!.to(["runs"]));
  const probe = holdAll();
  view.rerender(probe.guard);

  act(() => history.back());
  await waitFor(() => expect(probe.asked).toEqual(["#/people"]));
  // Undone: the page never showed the entry it was asked about.
  await settleOn("#/runs");

  act(() => probe.leave());
  await settleOn("#/people");

  act(() => history.forward());
  await waitFor(() => expect(probe.asked).toEqual(["#/people", "#/runs"]));
  await settleOn("#/people");
  act(() => probe.leave());
  await settleOn("#/runs");
});

// A link is an entry the browser makes itself, with no place stamped on it:
// it is the one after the entry the reader was on, and undone like Back.
test("a link is held and undone, and leave follows it", async () => {
  const probe = holdAll();
  mount(probe.guard);
  const link = document.createElement("a");
  link.href = "#/integrations";
  document.body.appendChild(link);
  try {
    fireEvent.click(link);
    await waitFor(() => expect(probe.asked).toEqual(["#/integrations"]));
    await settleOn("#/org");
    act(() => probe.leave());
    await settleOn("#/integrations");
  } finally {
    link.remove();
  }
});

test("a guard that lets a move go is not in its way, and no guard holds nothing", async () => {
  const asked: string[] = [];
  const view = mount((to) => {
    asked.push(to.hash);
    return false;
  });
  act(() => nav!.to(["people"]));
  expect(asked).toEqual(["#/people"]);
  expect(shown).toBe("#/people");
  view.rerender(null);
  act(() => history.back());
  await settleOn("#/org");
  expect(asked).toEqual(["#/people"]);
});

// An editor's question is asked over the lens's: the guard that began to
// hold last is asked first, and agreeing to it asks the next one down, so an
// editor that lets its form go never also lets the lens's work go unasked.
test("the guard that began to hold last is asked first, and every guard before the move", () => {
  const outer = holdAll();
  const inner = holdAll();
  function Nested({ holding }: { holding: boolean }) {
    nav = useNavigator();
    shown = useRoute().hash;
    useLeaveGuard(outer.guard);
    return <Inner holding={holding} />;
  }
  function Inner({ holding }: { holding: boolean }) {
    useLeaveGuard(holding ? inner.guard : null);
    return null;
  }
  const ui = (holding: boolean) => (
    <Router>
      <Nested holding={holding} />
    </Router>
  );
  const view = render(ui(false));
  view.rerender(ui(true));
  act(() => nav!.to(["people"]));
  expect(inner.asked).toEqual(["#/people"]);
  expect(outer.asked).toEqual([]);
  act(() => inner.leave());
  expect(outer.asked).toEqual(["#/people"]);
  expect(shown).toBe("#/org");
  act(() => outer.leave());
  expect(shown).toBe("#/people");
});

test("a reload or a closed tab asks while a leave or an unload guard holds, and only then", () => {
  const unload = () => {
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  };
  const view = mount(null);
  expect(unload()).toBe(false);
  view.rerender(holdAll().guard);
  expect(unload()).toBe(true);
  view.rerender(null, true);
  expect(unload()).toBe(true);
  view.rerender(null, false);
  expect(unload()).toBe(false);
});

// LISTENING ONLY WHILE SOMETHING HOLDS: some browsers keep a page with a
// `beforeunload` listener out of their back-forward cache, which would have
// every dashboard screen load from scratch on a Back from another site.
test("the page listens for a reload only while something holds work", () => {
  const added = vi.spyOn(window, "addEventListener");
  const removed = vi.spyOn(window, "removeEventListener");
  const unloads = (spy: typeof added) =>
    spy.mock.calls.filter(([type]) => type === "beforeunload").length;
  try {
    const view = mount(null);
    expect(unloads(added)).toBe(0);
    view.rerender(holdAll().guard, true);
    expect(unloads(added)).toBe(1);
    view.rerender(null, false);
    expect(unloads(removed)).toBe(1);
  } finally {
    added.mockRestore();
    removed.mockRestore();
  }
});

// A SCREEN MAY NAVIGATE IN ITS OWN FIRST EFFECT, which React runs before the
// router's. After a reload the entry stands wherever the session had got to,
// so the move takes its place from the entry, not from a router that has yet
// to adopt it: stamped with another place, every Back after it would be
// undone in the wrong direction.
test("a move made before the router adopts its entry keeps the entry's place", async () => {
  // A router that has stood on the session's first entry, as a fresh page's has.
  history.replaceState(null, "", "#/");
  render(<Router>{null}</Router>);
  cleanup();

  history.replaceState({ crewletIndex: 3 }, "", "#/org");
  function Early() {
    const navigator = useNavigator();
    useEffect(() => navigator.filter({ unit: "Sales" }), [navigator]);
    return null;
  }
  render(
    <Router>
      <Early />
      <Probe guard={null} />
    </Router>,
  );
  expect(location.hash).toBe("#/org?unit=Sales");
  expect((history.state as { crewletIndex?: unknown }).crewletIndex).toBe(3);
  act(() => nav!.to(["people"]));
  expect((history.state as { crewletIndex?: unknown }).crewletIndex).toBe(4);
});
