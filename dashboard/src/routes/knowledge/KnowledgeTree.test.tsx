/**
 * The Knowledge tree: the search's mode control and the spaces' pages.
 *
 *  - A MODE THE ENGINE CANNOT SERVE IS DISABLED WITH ITS REASON, written under
 *    the control, and pressing it changes nothing; before anything has said
 *    which modes exist, nothing is disabled.
 *  - "MEANING" SENDS `semantic`: the label is the reader's, the wire value the
 *    engine's.
 *  - A LEVEL IS NEVER CUT SILENTLY: it is read in windows of 500, says how
 *    many of how many are drawn, and "Load more" continues from the answer's
 *    own cursor.
 *  - THE DEFAULT MODE IS ONE THE ENGINE SERVES: with no embeddings provider
 *    the checked segment is Keyword, never a Hybrid also drawn unavailable.
 *  - ON A PHONE THE SPACES FOLD: the column stacks above the screen there, so
 *    no tree row — and no read of one — stands between a reader and the page
 *    they followed a link to until they open the disclosure.
 *  - WHO FILES INTO A SPACE HAS FOUR STATES, and an operator whose read is in
 *    flight is never told they need a token; a project the space key does not
 *    already name is DRAWN, not only put in a tooltip.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { KnowledgeTree } from "./KnowledgeTree.tsx";
import { installWindow } from "~/testing.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
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
  try {
    sessionStorage.clear();
  } catch {
    // no storage in this environment is fine
  }
  location.hash = "#/knowledge";
});

afterEach(() => {
  cleanup();
  try {
    localStorage.clear();
  } catch {
    // no storage in this environment is fine
  }
  vi.unstubAllGlobals();
  location.hash = "#/";
});

type Answer = (params: Record<string, unknown>) => unknown;

function mount(answers: Record<string, Answer>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params: Record<string, unknown>) => {
    asked.push({ what, params });
    const answer = answers[what];
    return answer ? Promise.resolve(answer(params)) : Promise.reject(new Error("unknown_query"));
  }) as typeof socket.query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <KnowledgeTree />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

const keywordOnly = () => ({
  backend: "native",
  hits: [],
  available: true,
  reason: "",
  note: "",
  mode: "semantic",
  served_mode: "",
  modes: ["keyword"],
  degraded: "no_embeddings",
  coverage: { nodes: [], complete: false, buckets_missing: 0 },
});

const everyMode = () => ({
  ...keywordOnly(),
  modes: ["hybrid", "keyword", "semantic"],
  degraded: "",
});

const noSpaces = () => ({ containers: [] });

test("a mode the engine cannot serve is disabled, with its reason written", async () => {
  mount({ knowledge: keywordOnly, containers: noSpaces });
  const meaning = await screen.findByRole("radio", { name: /Meaning/ });
  await waitFor(() => expect(meaning.getAttribute("aria-disabled")).toBe("true"));
  const hybrid = screen.getByRole("radio", { name: /Hybrid/ });
  expect(hybrid.getAttribute("aria-disabled")).toBe("true");
  expect(screen.getByRole("radio", { name: /Keyword/ }).getAttribute("aria-disabled")).toBeNull();
  // WRITTEN, and the control points at it.
  const reason = screen.getByText(/is unavailable: this company has no embeddings provider/);
  expect(meaning.getAttribute("aria-describedby")).toBe(reason.id);
  // AND PRESSING IT DOES NOTHING.
  fireEvent.click(meaning);
  expect(meaning.getAttribute("aria-checked")).toBe("false");
});

test("with no embeddings provider the default is Keyword, never a checked disabled Hybrid", async () => {
  mount({ knowledge: keywordOnly, containers: noSpaces });
  const keyword = await screen.findByRole("radio", { name: /Keyword/ });
  await waitFor(() => expect(keyword.getAttribute("aria-checked")).toBe("true"));
  const hybrid = screen.getByRole("radio", { name: /Hybrid/ });
  expect(hybrid.getAttribute("aria-checked")).toBe("false");
  // THE CHECKED SEGMENT IS NEVER ONE THE CONTROL SAYS IT CANNOT RUN.
  for (const radio of screen.getAllByRole("radio")) {
    if (radio.getAttribute("aria-checked") === "true") {
      expect(radio.getAttribute("aria-disabled")).toBeNull();
    }
  }
  // AND A SEARCH FROM THE TREE RUNS IN IT, so the engine is not asked for a
  // mode it will only degrade.
  fireEvent.change(screen.getByRole("searchbox", { name: "Search pages" }), {
    target: { value: "runbook" },
  });
  fireEvent.submit(screen.getByRole("search"));
  await waitFor(() => expect(location.hash).toContain("mode=keyword"));
});

test("with every mode served the default stays Hybrid and the address names none", async () => {
  mount({ knowledge: everyMode, containers: noSpaces });
  const hybrid = await screen.findByRole("radio", { name: /Hybrid/ });
  await waitFor(() =>
    expect(screen.getByRole("radio", { name: /Meaning/ }).getAttribute("aria-disabled")).toBeNull(),
  );
  expect(hybrid.getAttribute("aria-checked")).toBe("true");
  fireEvent.change(screen.getByRole("searchbox", { name: "Search pages" }), {
    target: { value: "runbook" },
  });
  fireEvent.submit(screen.getByRole("search"));
  await waitFor(() => expect(location.hash).toContain("q=runbook"));
  expect(location.hash).not.toContain("mode=");
});

test("the probe asks for semantic, the mode whose reason names the configuration", async () => {
  const asked = mount({ knowledge: everyMode, containers: noSpaces });
  await waitFor(() => expect(asked.some((a) => a.what === "knowledge")).toBe(true));
  const probe = asked.find((a) => a.what === "knowledge")!;
  expect(probe.params).toMatchObject({ q: "", mode: "semantic" });
});

test("before the engine says which modes exist, no mode is disabled", () => {
  mount({ knowledge: () => new Promise(() => {}), containers: noSpaces });
  for (const name of [/Hybrid/, /Keyword/, /Meaning/]) {
    expect(screen.getByRole("radio", { name }).getAttribute("aria-disabled")).toBeNull();
  }
});

test("Meaning sends semantic", async () => {
  location.hash = "#/knowledge?q=runbook";
  mount({ knowledge: everyMode, containers: noSpaces });
  const meaning = await screen.findByRole("radio", { name: /Meaning/ });
  await waitFor(() => expect(meaning.getAttribute("aria-disabled")).toBeNull());
  fireEvent.click(meaning);
  await waitFor(() => expect(location.hash).toContain("mode=semantic"));
  expect(location.hash).toContain("q=runbook");
  expect(location.hash).not.toMatch(/meaning/i);
});

test("a search from the tree carries its phrase and mode to the search screen", async () => {
  location.hash = "#/knowledge/pages/0f0f0f0f-1111-4222-8333-444455556666";
  mount({ knowledge: everyMode, containers: noSpaces, page: () => new Promise(() => {}) });
  fireEvent.click(await screen.findByRole("radio", { name: /Keyword/ }));
  fireEvent.change(screen.getByRole("searchbox", { name: "Search pages" }), {
    target: { value: "rekick a host" },
  });
  fireEvent.submit(screen.getByRole("search"));
  await waitFor(() => expect(location.hash).toMatch(/^#\/knowledge\?/));
  expect(location.hash).toContain("mode=keyword");
  expect(location.hash).toMatch(/q=rekick(\+|%20)a(\+|%20)host/);
});

test("a level is read in windows of 500 and never cut silently", async () => {
  const asked = mount({
    knowledge: everyMode,
    containers: () => ({ containers: [{ key: "ENG", name: "Engineering", pages: 700 }] }),
    pages: (params) =>
      params.after
        ? {
            pages: [
              {
                id: "p-3",
                title: "Zeta",
                container: "ENG",
                status: "published",
                version: 1,
                updated_at: "",
                revision: 1,
              },
            ],
            limit: 500,
            total: 700,
          }
        : {
            pages: [
              {
                id: "p-1",
                title: "Runbooks",
                container: "ENG",
                status: "published",
                version: 1,
                updated_at: "",
                revision: 1,
                children: 2,
              },
              {
                id: "p-2",
                title: "Handbook",
                container: "ENG",
                status: "draft",
                version: 1,
                updated_at: "",
                revision: 1,
              },
            ],
            limit: 500,
            total: 700,
            after: "cursor-1",
          },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Pages in Engineering" }));
  await waitFor(() => expect(screen.getByText("Runbooks")).toBeTruthy());
  // THE FIRST READ OF A SPACE — the Agent skills row's count is a listing of
  // its own, beside the tree.
  const first = asked.find((a) => a.what === "pages" && a.params.container === "ENG")!;
  expect(first.params).toMatchObject({
    container: "ENG",
    roots: true,
    limit: 500,
    status: "published,draft",
    skills: false,
  });
  // A WINDOW LABELLED AS A WINDOW.
  expect(screen.getByText("2 of 700")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  await waitFor(() => expect(screen.getByText("Zeta")).toBeTruthy());
  expect(asked.find((a) => a.what === "pages" && a.params.after)?.params.after).toBe("cursor-1");
  // A PAGE WITH CHILDREN OPENS; A LEAF HAS NOTHING TO OPEN.
  expect(screen.getByRole("button", { name: "Pages in Runbooks" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Pages in Handbook" })).toBeNull();
});

test("the page being read opens its space and its ancestors", async () => {
  location.hash = "#/knowledge/pages/0f0f0f0f-1111-4222-8333-444455556666";
  const asked = mount({
    knowledge: everyMode,
    containers: () => ({ containers: [{ key: "ENG", name: "Engineering", pages: 3 }] }),
    page: () => ({
      page: { id: "0f0f0f0f-1111-4222-8333-444455556666", container: "ENG", title: "Provisioner" },
      revision: 1,
      ancestors: [{ id: "p-1", title: "Runbooks", container: "ENG" }],
    }),
    pages: (params) =>
      params.parent === "p-1"
        ? {
            pages: [
              {
                id: "0f0f0f0f-1111-4222-8333-444455556666",
                title: "Provisioner",
                container: "ENG",
                status: "published",
                version: 1,
                updated_at: "",
                revision: 1,
              },
            ],
            limit: 500,
            total: 1,
          }
        : {
            pages: [
              {
                id: "p-1",
                title: "Runbooks",
                container: "ENG",
                status: "published",
                version: 1,
                updated_at: "",
                revision: 1,
                children: 1,
              },
            ],
            limit: 500,
            total: 1,
          },
  });
  const current = await screen.findByRole("link", { name: /Provisioner/ });
  expect(current.getAttribute("aria-current")).toBe("page");
  expect(asked.some((a) => a.what === "pages" && a.params.parent === "p-1")).toBe(true);
});

test("a company on a vendor wiki is told there is no tree here, not shown an error", async () => {
  mount({ knowledge: keywordOnly });
  await waitFor(() => expect(screen.getByText(/no tree to browse here/)).toBeTruthy());
});

test("who files into a space: four states, each its own sentence", async () => {
  const eng = () => ({ containers: [{ key: "ENG", name: "Engineering", pages: 3 }] });
  const key = () => screen.getByText("ENG").closest(".ktree-key")!.textContent;

  // NO TOKEN: nothing is asked, and the sentence says why.
  let asked = mount({ knowledge: everyMode, containers: eng });
  await waitFor(() => expect(screen.getByText("ENG")).toBeTruthy());
  expect(key()).toMatch(/needs an operator token to read/);
  expect(asked.some((a) => a.what === "config")).toBe(false);

  // A TOKEN AND A READ IN FLIGHT: not "needs a token".
  cleanup();
  localStorage.setItem("crewlet_api_token", "tok");
  asked = mount({ knowledge: everyMode, containers: eng, config: () => new Promise(() => {}) });
  await waitFor(() => expect(screen.getByText("ENG")).toBeTruthy());
  expect(key()).toMatch(/still being read/);
  expect(key()).not.toMatch(/needs an operator token/);
  expect(asked.some((a) => a.what === "config")).toBe(true);

  // REFUSED.
  cleanup();
  mount({
    knowledge: everyMode,
    containers: eng,
    config: () => Promise.reject(new Error("unauthorized")),
  });
  await waitFor(() => expect(key()).toMatch(/could not be read with this token/));

  // READ: the unit, and its project DRAWN where the key does not name it.
  cleanup();
  mount({
    knowledge: everyMode,
    containers: eng,
    config: () => ({ units: [{ name: "Core platform", space: "eng", project: "PLAT" }] }),
  });
  await waitFor(() => expect(key()).toMatch(/Filed by Core platform, whose work is in PLAT/));
  const proj = document.querySelector(".ktree-proj");
  expect(proj?.textContent).toBe("tracker project PLAT");
  expect(proj?.querySelector(".sr-only")).toBeTruthy();
});

test("a project the space key already names is not drawn twice", async () => {
  localStorage.setItem("crewlet_api_token", "tok");
  mount({
    knowledge: everyMode,
    containers: () => ({ containers: [{ key: "ENG", name: "Engineering", pages: 3 }] }),
    config: () => ({ units: [{ name: "Engineering", space: "ENG", project: "ENG" }] }),
  });
  await waitFor(() =>
    expect(screen.getByText("ENG").closest(".ktree-key")!.textContent).toMatch(/Filed by/),
  );
  expect(document.querySelector(".ktree-proj")).toBeNull();
});

test("on a phone the spaces fold, so no tree stands above the page being read", async () => {
  const phone = installWindow(390);
  try {
    location.hash = "#/knowledge/pages/0f0f0f0f-1111-4222-8333-444455556666";
    const asked = mount({
      knowledge: everyMode,
      containers: () => ({ containers: [{ key: "ENG", name: "Engineering", pages: 3 }] }),
      page: () => ({
        page: {
          id: "0f0f0f0f-1111-4222-8333-444455556666",
          container: "ENG",
          title: "Provisioner",
        },
        revision: 1,
        ancestors: [],
      }),
      pages: () => ({
        pages: [
          {
            id: "0f0f0f0f-1111-4222-8333-444455556666",
            title: "Provisioner",
            container: "ENG",
            status: "published",
            version: 1,
            updated_at: "",
            revision: 1,
          },
        ],
        limit: 500,
        total: 1,
      }),
    });
    // THE SEARCH STAYS; THE SPACES ARE ONE CLOSED DISCLOSURE, with how many.
    expect(screen.getByRole("searchbox", { name: "Search pages" })).toBeTruthy();
    const fold = await screen.findByRole("button", { name: /Spaces/ });
    await waitFor(() => expect(fold.textContent).toContain("1"));
    expect(fold.getAttribute("aria-expanded")).toBe("false");
    // NOTHING UNDER IT IS DRAWN OR READ while it is closed.
    expect(screen.queryByText("Engineering")).toBeNull();
    expect(document.querySelector(".ktree-spaces .ktree-row")).toBeNull();
    // The one listing read is the Agent skills row's count, a single row.
    expect(asked.some((a) => (a.what === "pages" && !a.params.skills) || a.what === "page")).toBe(
      false,
    );

    // OPENED, it opens where the reader is.
    fireEvent.click(fold);
    expect(fold.getAttribute("aria-expanded")).toBe("true");
    const current = await screen.findByRole("link", { name: /Provisioner/ });
    expect(current.getAttribute("aria-current")).toBe("page");

    // AND A LINK FOLLOWED FROM IT LANDS ON ITS PAGE, not on the tree again.
    location.hash = "#/knowledge/ENG";
    await waitFor(() => expect(fold.getAttribute("aria-expanded")).toBe("false"));
    expect(document.querySelector(".ktree-spaces .ktree-row")).toBeNull();
  } finally {
    phone.restore();
  }
});

test("above a phone the spaces are always drawn", async () => {
  const wide = installWindow(1280);
  try {
    mount({
      knowledge: everyMode,
      containers: () => ({ containers: [{ key: "ENG", name: "Engineering", pages: 3 }] }),
    });
    await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
    expect(screen.queryByRole("button", { name: /^Spaces/ })).toBeNull();
    expect(screen.getByRole("heading", { name: "Spaces" })).toBeTruthy();
  } finally {
    wide.restore();
  }
});

// THE AGENT SKILLS ROW COUNTS WHAT THE ENGINE COUNTS: the listing's total of
// published tool skills, never the length of a window.
test("the Agent skills row carries the engine's total of tool skills", async () => {
  const asked = mount({
    knowledge: everyMode,
    containers: () => ({ containers: [] }),
    pages: (params) =>
      params.skills ? { pages: [], limit: 1, total: 42 } : { pages: [], limit: 500, total: 0 },
  });
  const row = await screen.findByRole("link", { name: /Agent skills/ });
  await waitFor(() => expect(row.textContent).toContain("42"));
  expect(row.getAttribute("href")).toBe("#/knowledge/skills");
  expect(asked.find((a) => a.what === "pages" && a.params.skills)?.params).toMatchObject({
    skills: true,
    status: "published",
    limit: 1,
  });
});
