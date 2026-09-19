/**
 * What the page for one saved view CLAIMS about who owns it.
 *
 * A view's owner is the one value on this screen that is stored as a slug and
 * read as a person, and it is printed twice — once in the note and once in the
 * facts. Two renderings of one value is exactly where a screen comes to name
 * one person two ways, which is the failure this case exists to hold shut.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";
import { SavedViews } from "./SavedViews.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [{ name: "Ada Okonkwo", handle: "ada-okonkwo", kind: "agent" }],
  } as never);
  return query;
}

const view = (over: Record<string, unknown> = {}) => ({
  id: "v-1",
  key: "my-queue",
  name: "My queue",
  type: "list",
  container: { kind: "workspace", id: "" },
  builtin: false,
  ...over,
});

const mount = (id?: string) =>
  render(
    <Router>
      <SavedViews id={id} />
    </Router>,
  );

// AN OWNER IS A PERSON, and the chart is what turns their handle into the name
// they are actually called. The note printed the raw slug while the Owner fact
// two lines below it resolved the same value — so `ada-okonkwo` and "Ada
// Okonkwo" appeared on one page as though they were two people.
test("a personal view names its owner the way the rest of the screen does", async () => {
  serving({ work_views: { views: [view({ owner: "ada-okonkwo" })], complete: true } });
  const { container } = mount("v-1");
  await waitFor(() => expect(screen.getByText(/A personal view owned by/)).toBeTruthy());
  expect(container.textContent).toContain("A personal view owned by Ada Okonkwo.");
  expect(container.textContent).not.toContain("ada-okonkwo");
});

// A HANDLE THE CHART DOES NOT HOLD IS STILL THE THING TO SAY. An operator
// writing through the MCP surface owns views and holds no seat, so the
// fallback is the handle itself rather than an empty sentence.
test("an owner outside the chart is named by their handle", async () => {
  serving({ work_views: { views: [view({ owner: "ops-rota" })], complete: true } });
  const { container } = mount("v-1");
  await waitFor(() => expect(screen.getByText(/A personal view owned by/)).toBeTruthy());
  expect(container.textContent).toContain("A personal view owned by ops-rota.");
});

// A SHARED VIEW IS NOT AN UNOWNED ONE — empty is a setting somebody chose, and
// it is a sentence rather than a blank.
test("a shared view says what shared means", async () => {
  serving({ work_views: { views: [view()], complete: true } });
  const { container } = mount("v-1");
  await waitFor(() => expect(screen.getByText(/everybody in this company/)).toBeTruthy());
  expect(container.textContent).not.toContain("A personal view owned by");
});

/** The explanation each mark carries, in the order they are drawn. */
function markTitles(root: HTMLElement): (string | null)[] {
  return [...root.querySelectorAll(".crewlet-tag")]
    .filter((t) => ["default", "protected", "pinned"].includes(t.textContent ?? ""))
    .map((t) => t.getAttribute("title"));
}

// A HEADED COLUMN ANSWERS ON EVERY ROW.
//
// None of the three marks is set on a view somebody just saved, so the cell drew
// an empty span for those rows — and in a company where nobody has pinned or
// protected anything, a column headed "Marks" was blank the whole way down,
// which reads as a screen that failed to load rather than as three settings
// nobody turned on.
test("a view carrying no marks says which absence that is", async () => {
  serving({
    work_views: {
      views: [view(), view({ id: "v-2", key: "team-board", name: "Team board", pinned: true })],
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Team board")).toBeTruthy());

  const dashes = container.querySelectorAll(".crewlet-empty-value");
  expect(dashes.length).toBe(1);
  // READ, not hovered: the sentence naming which absence this is rides in the
  // accessible name now rather than in a `title` only a mouse could reach.
  expect(dashes[0]!.textContent).toBe(
    `${EMPTY_VALUE}Not the default, not protected, not pinned by you`,
  );
  // THE CONTROL: the marked row draws its mark and NO dash, or this would pass
  // just as well on a grid that dashed every row in the column.
  expect(screen.getByText("pinned")).toBeTruthy();
});

// ONE SPELLING OF A MARK. The grid's tags carried the sentence saying what each
// mark means and the facts block's did not, so `protected` explained itself on
// the inventory and explained nothing on the view's own page.
test("a mark says what it means wherever it is drawn", async () => {
  const marked = { default: true, protected: true, pinned: true };
  serving({ work_views: { views: [view(marked)], complete: true } });
  const list = mount();
  await waitFor(() => expect(screen.getByText("My queue")).toBeTruthy());
  const onTheGrid = markTitles(list.container);
  cleanup();

  serving({ work_views: { views: [view(marked)], complete: true } });
  const page = mount("v-1");
  await waitFor(() => expect(screen.getByText("Saved view")).toBeTruthy());
  expect(markTitles(page.container)).toEqual(onTheGrid);
  expect(onTheGrid).toEqual([
    "the container's landing tab",
    "only its owner may change it",
    "pinned by you — pins are per reader",
  ]);
});
