/**
 * Whose day this screen is showing, and how it says so.
 *
 * The screen is read two ways — a person reading their own day, and an
 * operator reading a report's — and one wording cannot serve both. "Nothing
 * has reached them" on your own inbox reads as a screen describing somebody
 * else, which is precisely the confusion the `viewer` question exists to end.
 *
 * It matters more here than anywhere: this screen used to fall back to the
 * ALPHABETICALLY FIRST SEAT, so a page titled "My work" showed every reader a
 * stranger's day with no indication that it had guessed.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { MyWork } from "./MyWork.tsx";
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
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [
      { name: "Ada Okonkwo", handle: "ada", kind: "human", contact: { slack_user_id: "U0A" } },
      { name: "Rui Santos", handle: "rui", kind: "human", contact: { slack_user_id: "U0R" } },
    ],
  } as never);
  return query;
}

const emptyDay = {
  handle: "ada",
  priorities: [],
  assigned: [],
  asked_of_me: [],
  checklist_items: [],
  collaborating: [],
  watching_recent: [],
  unblocked_recent: [],
  complete: true,
};

const emptyInbox = { handle: "ada", notices: [], primary_reasons: [], unread: 0, primary: 0 };

function mount() {
  return render(
    <Router>
      <MyWork />
    </Router>,
  );
}

// THE VIEWER DECIDES WHOSE DAY IT IS, with no handle in the URL. Before the
// `viewer` question existed there was nothing for this to resolve from, and
// the screen picked the first seat in the roster.
test("with no handle it shows the viewer's own day, and says it is theirs", async () => {
  serving({
    viewer: {
      operator_id: "ops-1",
      operator: true,
      handle: "ada",
      name: "Ada Okonkwo",
      kind: "human",
    },
    work_my_work: emptyDay,
    work_inbox: emptyInbox,
  });
  mount();
  await waitFor(() => expect(screen.getByText("yours")).toBeTruthy());
  // SECOND PERSON on your own day. Awaited separately: the viewer resolves
  // first and the day is a second round trip, so the badge is on screen a
  // render before the panels are.
  await waitFor(() => expect(screen.getByText("Nothing has reached you")).toBeTruthy());
});

// AN OPERATOR READING SOMEBODY ELSE'S DAY is a real thing to do, and the
// screen has to say so: a page called "My work" showing a colleague's without
// naming them is how a reader acts on work that is not theirs.
test("an explicit handle names whose day it is, in the third person", async () => {
  location.hash = "#/me?handle=rui";
  serving({
    viewer: {
      operator_id: "ops-1",
      operator: true,
      handle: "ada",
      name: "Ada Okonkwo",
      kind: "human",
    },
    work_my_work: { ...emptyDay, handle: "rui" },
    work_inbox: { ...emptyInbox, handle: "rui" },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Rui Santos’s day")).toBeTruthy());
  await waitFor(() => expect(screen.getByText("Nothing has reached them")).toBeTruthy());
  expect(screen.queryByText("yours")).toBeNull();
});

// AND THE THREE VIEWER STATES TAKE THREE SENTENCES. Only one of them is
// anybody's fault, and the other two have different remedies: a credential,
// and a line of company configuration.
test("an unbound token says what to bind, not that something is broken", async () => {
  serving({
    viewer: { operator_id: "ops-7", operator: true, handle: "", name: "", kind: "" },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/not bound to a person/)).toBeTruthy());
  // The id is NAMED, because it is the value that goes in the config.
  expect(screen.getByText(/ops-7/)).toBeTruthy();
});

test("no credential at all is a different sentence from an unbound one", async () => {
  serving({ viewer: { operator_id: "", operator: false, handle: "", name: "", kind: "" } });
  mount();
  await waitFor(() => expect(screen.getByText(/No credential is presented/)).toBeTruthy());
  expect(screen.queryByText(/not bound to a person/)).toBeNull();
});
