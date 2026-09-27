/**
 * "3 of 18" on a task page, in the list the task was opened from.
 *
 * The failure it exists to prevent is a count of whatever page the list had
 * loaded: a lane of four hundred stopped at fifty and called the fiftieth task
 * the last one. The page carries the list's QUESTION, and the engine places
 * the task in the whole answer.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { ListPosition } from "./ListPosition.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  location.hash = "";
});

function mount(hash: string, around: unknown) {
  location.hash = hash;
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    if (what === "work_items") asked.push(params ?? {});
    return Promise.resolve(what === "work_items" ? { items: [], around, complete: true } : {});
  }) as typeof socket.query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ListPosition itemKey="ENG-4" />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

const LIST = encodeURIComponent("container=project:ENG&group_by=status&closed_since=sow");

test("3 of 18 is the engine's placement in the whole list, with a way to either side", async () => {
  const asked = mount(`#/work/ENG-4?list=${LIST}`, {
    position: 3,
    prev: "ENG-2",
    next: "ENG-7",
    total_hint: 18,
  });
  await settle();
  // THE LIST'S OWN QUESTION, asked with `around=` and one card per lane.
  expect(asked.at(-1)).toEqual({
    container: "project:ENG",
    group_by: "status",
    closed_since: "sow",
    around: "ENG-4",
    group_limit: 1,
  });
  expect(screen.getByText("3 of 18")).toBeTruthy();
  const prev = screen.getByRole("link", { name: "Previous task: ENG-2" });
  // AND THE NEIGHBOUR CARRIES THE SAME LIST, so stepping keeps counting.
  expect(prev.getAttribute("href")).toBe(`#/work/ENG-2?list=${LIST}`);
  expect(screen.getByRole("link", { name: "Next task: ENG-7" })).toBeTruthy();
  // UP AND DOWN, THEN THE PLACE, as the approved Issue page draws it.
  const nav = document.querySelector(".list-position")!;
  expect(nav.lastElementChild?.textContent).toBe("3 of 18");
});

// `j` AND `k` STEP THROUGH THE LIST, as they step through its rows: on a task
// opened from a list, the task is that list's row. Each keeps the list, so the
// next task still knows where it sits.
test("j and k open the next and the previous task in the list", async () => {
  mount(`#/work/ENG-4?list=${LIST}`, { position: 3, prev: "ENG-2", next: "ENG-7", total_hint: 18 });
  await settle();
  fireEvent.keyDown(window, { key: "j" });
  expect(location.hash).toBe(`#/work/ENG-7?list=${LIST}`);
  fireEvent.keyDown(window, { key: "k" });
  expect(location.hash).toBe(`#/work/ENG-2?list=${LIST}`);
});

// PAST THE ENGINE'S COUNTING CEILING there is a count and no place in it.
test("a position the engine could not count to reads as one of a capped total", async () => {
  mount(`#/work/ENG-4?list=${LIST}`, {
    position: null,
    prev: null,
    next: "ENG-7",
    total_hint: 10000,
    total_capped: true,
  });
  await settle();
  expect(screen.getByText("one of 10,000+")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /Previous/ })).toBeNull();
  // AND `k` HAS NOWHERE TO GO, so it goes nowhere.
  fireEvent.keyDown(window, { key: "k" });
  expect(location.hash).toBe(`#/work/ENG-4?list=${LIST}`);
});

// NOTHING TRUE TO SAY, NOTHING SAID: a pasted link carries no list, and a task
// that has moved off its list since is not "0 of 18".
test("no list, or a task no longer on it, draws nothing", async () => {
  const asked = mount("#/work/ENG-4", null);
  await settle();
  expect(asked).toEqual([]);
  expect(document.querySelector(".list-position")).toBeNull();
  cleanup();
  mount(`#/work/ENG-4?list=${LIST}`, null);
  await settle();
  expect(document.querySelector(".list-position")).toBeNull();
});
