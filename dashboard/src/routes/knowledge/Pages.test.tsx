/**
 * What a page's header says about where the page sits.
 *
 * The trail above the header and the `Container` fact inside it are two
 * answers to one question, and only one of them is ever worth drawing twice:
 * the fact is a link to the container, and the trail is the path THROUGH the
 * container's tree. On a page filed directly in its container — which is most
 * of them — the trail has nothing the fact does not, and it rendered anyway:
 * one accent word alone in an otherwise empty band above the header, reading
 * as a stray button over a header that says the same thing three lines down.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { PageView } from "./Pages.tsx";
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

/** One socket answering each question with a fixture. */
function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({ name: "Acme", roles: [] } as never);
  return query;
}

const page = {
  id: "p-1",
  container: "LEAD",
  title: "Test Sample Page",
  status: "published",
  version: 1,
  author: "agent-ceo",
  updated_at: "2026-09-19T12:00:00Z",
  body: "",
};

function mount() {
  return render(
    <Router>
      <PageView container="LEAD" title="Test Sample Page" />
    </Router>,
  );
}

/** Every link on screen whose text is exactly the container's name. */
function containerLinks() {
  return screen.queryAllByRole("link", { name: "LEAD" });
}

test("a page filed straight in its container names it once, as the fact", async () => {
  serving({ page: { page, history: [], ancestors: [], children: [] } });
  mount();
  // The fact is the one that survives: it is inside the header, under a label
  // saying which question it answers.
  await waitFor(() => expect(screen.getByText("Container")).toBeTruthy());
  expect(containerLinks()).toHaveLength(1);
});

test("a page under an ancestor draws the path, because the fact cannot", async () => {
  serving({
    page: {
      page,
      history: [],
      // The half a `Container` fact has no room for: which page inside the
      // tree this one hangs off.
      ancestors: [{ id: "p-0", container: "LEAD", title: "Runbooks" }],
      children: [],
    },
  });
  mount();
  await waitFor(() => expect(screen.getByRole("link", { name: "Runbooks" })).toBeTruthy());
  // Two now, and they are not a duplicate: one opens the container, the other
  // is the first step of a path that continues past it.
  expect(containerLinks()).toHaveLength(2);
});
