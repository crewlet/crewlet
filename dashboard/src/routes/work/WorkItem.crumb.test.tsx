/**
 * What the trail calls a task's project.
 *
 * ONE PROJECT, ONE NAME. The project's own page titles its crumb with the
 * project's name, and the task page one click below it named the same
 * project by its key — `Work › Core` on one page and `Work › ENG › ENG-1` on
 * the next, which reads as two places. The task page reads the project anyway
 * (for its statuses, types and fields), so the name is in hand.
 *
 * THE KEY STAYS UNTIL THE NAME ARRIVES: an address the reader can paste is a
 * better crumb than nothing, and a placeholder would be worse than either.
 */

import { act, cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { WorkItem } from "./WorkItem.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { resetForTest } from "~/lib/recents.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  localStorage.clear();
  resetForTest();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
  document.title = "";
});

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

function mount(answers: Record<string, unknown>) {
  location.hash = "#/work/ENG-1";
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [], units: [] });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    Promise.resolve(answers[what] ?? {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <WorkItem id="ENG-1" />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });
const parts = () =>
  [...trail().querySelectorAll(".crumb-part")].map((p) => p.textContent?.trim() ?? "");

const item = {
  task: { id: "t-1", key: "ENG-1", project: "ENG", title: "Fix the login race", version: 1 },
  complete: true,
};

test("a task's trail names its project as the project's page does", async () => {
  mount({
    work_item: item,
    work_project: {
      key: "ENG",
      name: "Core",
      task_counts: { todo: 1, active: 0, done: 0, closed: 0 },
      version: 1,
    },
  });
  await settle();

  // THE NAME WITH ITS KEY AS A CHIP, the way every row and rail draws a
  // project ("ENG Core platform" in the artboard's bar).
  expect(parts()).toEqual(["Work", "ENGCore", "ENG-1"]);
  // Still the way to it: the crumb links to the project's page.
  const link = trail().querySelector<HTMLAnchorElement>("a[href='#/work/ENG']");
  expect(link?.querySelector(".crumb-tag")?.textContent).toBe("ENG");
  expect(link?.querySelector(".crumb-text")?.textContent).toBe("Core");
});

test("the key is the crumb until the project has answered", async () => {
  mount({ work_item: item });
  await settle();

  expect(parts()).toEqual(["Work", "ENG", "ENG-1"]);
});
