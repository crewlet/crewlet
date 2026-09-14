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

import { act, cleanup, fireEvent, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
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

/** Lets the history traversals the page queued run, and their events arrive. */
const settle = () => act(() => new Promise<void>((resolve) => setTimeout(resolve, 20)));

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
  await settle();
  expect(probe.asked).toEqual(["#/people"]);
  // Undone: the page never showed the entry it was asked about.
  expect(location.hash).toBe("#/runs");
  expect(shown).toBe("#/runs");

  act(() => probe.leave());
  await settle();
  expect(location.hash).toBe("#/people");
  expect(shown).toBe("#/people");

  act(() => history.forward());
  await settle();
  expect(probe.asked).toEqual(["#/people", "#/runs"]);
  expect(location.hash).toBe("#/people");
  act(() => probe.leave());
  await settle();
  expect(shown).toBe("#/runs");
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
    await settle();
    expect(probe.asked).toEqual(["#/integrations"]);
    expect(location.hash).toBe("#/org");
    expect(shown).toBe("#/org");
    act(() => probe.leave());
    await settle();
    expect(location.hash).toBe("#/integrations");
    expect(shown).toBe("#/integrations");
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
  await settle();
  expect(asked).toEqual(["#/people"]);
  expect(shown).toBe("#/org");
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
