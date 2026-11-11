/**
 * Reviewing a server's changed tools: what moved, the suspicious direction
 * said first, and each decision sent by the digest of the definition reviewed.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { RestResult } from "~/lib/useRest.ts";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { reviewChanges, type ServerDetail } from "../api.ts";
import {
  LINEAR_CHANGES,
  LINEAR_DETAIL,
  LINEAR_READ_TOOLS,
  longDescription,
  mcpData,
  wordingChange,
} from "../fixtures.ts";
import { DriftDialog } from "./DriftDialog.tsx";

let detail: ServerDetail = { ...LINEAR_DETAIL, changes: LINEAR_CHANGES };
const reload = vi.fn(async () => {});
vi.mock("../data.ts", async (actual) => ({
  ...(await actual<typeof import("../data.ts")>()),
  useServerDetail: (): RestResult<ServerDetail> => ({
    data: detail,
    error: null,
    code: null,
    refusal: null,
    loading: false,
    reload,
  }),
}));
vi.mock("../api.ts", async (actual) => ({
  ...(await actual<typeof import("../api.ts")>()),
  reviewChanges: vi.fn(),
}));

const onDone = vi.fn();
const onClose = vi.fn();

function tree() {
  const server = mcpData("admin").server("linear")!;
  return (
    <TestClient>
      <ToastProvider>
        <LayerHost>
          <DriftDialog server={server} onClose={onClose} onDone={onDone} />
        </LayerHost>
      </ToastProvider>
    </TestClient>
  );
}

function mount() {
  return render(tree());
}

beforeEach(() => {
  detail = { ...LINEAR_DETAIL, changes: LINEAR_CHANGES };
  vi.mocked(reviewChanges).mockReset();
  onDone.mockReset();
  onClose.mockReset();
  reload.mockClear();
});

afterEach(() => cleanup());

// THE SUSPICIOUS DIRECTION IS SAID: a change that claims less risk is shown
// first, flagged, with its hints before and after.
test("a change that claims less risk is shown first, flagged, with what moved", () => {
  mount();
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("It now claims to be less risky")).toBeDefined();
  expect(within(dialog).getByText("Description")).toBeDefined();
  expect(within(dialog).getByText("Hints")).toBeDefined();
  expect(within(dialog).getAllByText("Destructive").length).toBeGreaterThan(0);
  expect(within(dialog).getByText(/the tool it holds asks for approval/)).toBeDefined();
});

// A DECISION NAMES THE DEFINITION REVIEWED: accepting sends the change's
// `after.digest` by the server's own name for the tool, keeping it blocked
// the same, and a removal is accepted by null. Mutation: send the accepted
// digest, or the prefixed name, and this is red.
test("each decision is sent by the digest of the definition reviewed", async () => {
  vi.mocked(reviewChanges).mockResolvedValue({
    kind: "ok",
    status: 200,
    value: { server: "linear", accepted: ["delete_issue"], blocked: [], moved: [], revision: null },
  });
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept change" }));
  await afterMicrotasks();
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { accept: { delete_issue: "c".repeat(64) } },
    "Accepted the change to delete_issue on linear",
  );
  expect(onDone).toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Keep blocked" }));
  await afterMicrotasks();
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { block: { delete_issue: "c".repeat(64) } },
    "Kept delete_issue on linear blocked",
  );
  fireEvent.click(screen.getByRole("button", { name: /export_board/ }));
  expect(screen.queryByRole("button", { name: "Keep blocked" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Accept that it went" }));
  await afterMicrotasks();
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { accept: { export_board: null } },
    "Accepted the change to export_board on linear",
  );
});

// A DEFINITION SWAPPED SINCE THE REVIEW is refused, said, and read again.
test("a decision refused as stale says so and reads the changes again", async () => {
  vi.mocked(reviewChanges).mockResolvedValue({
    kind: "refused",
    status: 409,
    code: "stale_version",
    sentence: "delete_issue changed again since it was reviewed; nothing was written.",
    body: { tools: ["delete_issue"] },
  });
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept change" }));
  await afterMicrotasks();
  expect(screen.getAllByText(/changed again since it was reviewed/).length).toBeGreaterThan(0);
  expect(reload).toHaveBeenCalled();
  expect(onDone).not.toHaveBeenCalled();
});

test("a server with nothing changed says so", () => {
  detail = { ...LINEAR_DETAIL, changes: [] };
  mount();
  expect(screen.getByText("No changes to review")).toBeDefined();
});

// A DESCRIPTION CHANGED PAST WHAT A LIST KEEPS IS SHOWN: the engine serves both
// descriptions whole, and the diff draws the sentence the server changed at
// their end. Mutation: diff the first KiB of each, or drop the description
// part, and this is red.
test("a description changed past its first KiB shows the late change", () => {
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_READ_TOOLS,
    changes: [
      wordingChange(
        "list_issues",
        longDescription("It never closes an issue."),
        longDescription("It closes the stale ones it finds."),
        ["k", "m"],
      ),
    ],
  };
  mount();
  const dialog = screen.getByRole("dialog");
  const diff = within(dialog).getByRole("group", {
    name: "list_issues description, before and after",
  });
  expect(diff.textContent).toContain("It closes the stale ones it finds.");
  expect(diff.textContent).toContain("It never closes an issue.");
  expect(dialog.textContent).not.toContain("Nothing changed");
});

// A PART THAT MOVED IS NEVER DRAWN AS UNCHANGED: one the engine says it cannot
// show is said to have changed and not to be shown, and one whose two texts
// read alike although it moved says the same rather than "Nothing changed".
// Mutation: drop the `unshown` branch or a diff's `empty`, and this is red.
test("a part that moved and cannot be shown is said, never drawn as unchanged", () => {
  const cut = wordingChange("list_issues", "List the issues", "List the issues", ["k", "m"]);
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_READ_TOOLS,
    changes: [
      {
        ...cut,
        unshown: ["description"],
        before: { ...cut.before!, whole: false },
        after: { ...cut.after!, whole: false },
      },
    ],
  };
  mount();
  let dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("the engine keeps no whole copy of it");
  expect(dialog.textContent).not.toContain("Nothing changed");
  cleanup();

  detail = { ...LINEAR_DETAIL, tools: LINEAR_READ_TOOLS, changes: [cut] };
  mount();
  dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("The description changed, but this review cannot show how.");
  expect(dialog.textContent).not.toContain("Nothing changed");
});

// WORDING ON READ-ONLY TOOLS IS ACCEPTED TOGETHER, ONCE READ: only the changes
// that reword a read-only tool and move nothing else are offered; the first
// press sends nothing and lists each tool with the words it changed, and the
// second sends each tool's `after.digest` in one decision. A loosened change, a
// removal and a part the review cannot show are left to be decided one by one.
// Mutation: send on the first press, send the accepted digests, list a change
// that is not wording on a read-only tool, or drop its changed words, and
// this is red.
test("wording changes on read-only tools are listed, then accepted together by each digest", async () => {
  vi.mocked(reviewChanges).mockResolvedValue({
    kind: "ok",
    status: 200,
    value: {
      server: "linear",
      accepted: ["list_issues", "list_teams"],
      blocked: [],
      moved: [],
      revision: null,
    },
  });
  const hidden = wordingChange("get_issue", "Read an issue", "Read an issue", ["p", "q"]);
  detail = {
    ...LINEAR_DETAIL,
    tools: [...LINEAR_READ_TOOLS, LINEAR_DETAIL.tools[1]!],
    changes: [
      ...LINEAR_CHANGES,
      wordingChange(
        "list_issues",
        longDescription("It never closes an issue."),
        longDescription("It closes the stale ones it finds."),
        ["k", "m"],
      ),
      wordingChange("list_teams", "List teams", "List every team", ["n", "o"]),
      { ...hidden, unshown: ["description"] },
    ],
  };
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept 2 wording changes" }));
  await afterMicrotasks();
  expect(reviewChanges).not.toHaveBeenCalled();
  const listed = screen.getByRole("dialog");
  expect(within(listed).getByText("Accept 2 wording changes on Linear?")).toBeDefined();
  const terms = within(listed)
    .getAllByRole("term")
    .map((t) => t.textContent);
  expect(terms).toEqual(["list_issues", "list_teams"]);
  const words = within(listed)
    .getAllByRole("definition")
    .map((d) => d.textContent);
  expect(words).toEqual([
    "\u201cnever closes an issue\u201d \u2192 \u201ccloses the stale ones it finds\u201d",
    "\u201cteams\u201d \u2192 \u201cevery team\u201d",
  ]);

  fireEvent.click(within(listed).getByRole("button", { name: "Accept 2 changes" }));
  await afterMicrotasks();
  expect(reviewChanges).toHaveBeenCalledTimes(1);
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { accept: { list_issues: "m".repeat(64), list_teams: "o".repeat(64) } },
    "Accepted 2 wording changes on linear",
  );
  expect(onDone).toHaveBeenCalled();
});

// CANCEL RETURNS TO THE REVIEW, having sent nothing. Mutation: send on Cancel,
// or close the dialog, and this is red.
test("cancelling the wording list returns to the review and sends nothing", async () => {
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_READ_TOOLS,
    changes: [
      wordingChange("list_issues", "List issues", "List the issues in a team", ["k", "m"]),
      wordingChange("list_teams", "List teams", "List every team", ["n", "o"]),
    ],
  };
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept 2 wording changes" }));
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await afterMicrotasks();
  expect(reviewChanges).not.toHaveBeenCalled();
  expect(onClose).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Accept 2 wording changes" })).toBeDefined();
  expect(screen.getByRole("button", { name: "Accept change" })).toBeDefined();
});

// ONE WORDING CHANGE IS DECIDED ON ITS OWN, and so is one on a tool that runs
// as more than read-only here, whatever its server declares. Mutation: offer
// the action for one change, or judge the risk by the declared hints alone,
// and this is red.
test("the wording action is offered only for two or more qualifying changes", () => {
  const teams = LINEAR_READ_TOOLS.map((t) =>
    t.raw === "list_teams" ? { ...t, risk: "additive" as const } : t,
  );
  detail = {
    ...LINEAR_DETAIL,
    tools: teams,
    changes: [
      wordingChange("list_issues", "List issues", "List the issues in a team", ["k", "m"]),
      wordingChange("list_teams", "List teams", "List every team", ["n", "o"]),
    ],
  };
  mount();
  expect(screen.queryByRole("button", { name: /wording change/ })).toBeNull();
  expect(screen.getByRole("button", { name: "Accept change" })).toBeDefined();
});

// A SCHEMA CHANGE IS SHOWN WHOLE, whatever else moved: an argument added beside
// another argument's rewritten description, and an output schema that moved,
// are each drawn, the argument lines above as the summary. Mutation: draw the
// whole schema only where the argument lines read alike, and this is red.
test("a schema change shows the schemas whole beside an added argument", () => {
  const base = wordingChange("list_issues", "List issues", "List issues", ["k", "m"]);
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_READ_TOOLS,
    changes: [
      {
        ...base,
        kinds: ["schema"],
        before: {
          ...base.before!,
          input_schema: {
            type: "object",
            properties: { team: { type: "string", description: "The team to list." } },
          },
          output_schema: { type: "object", properties: { issues: { type: "array" } } },
        },
        after: {
          ...base.after!,
          input_schema: {
            type: "object",
            properties: {
              team: { type: "string", description: "The team to list, and close what is stale." },
              limit: { type: "integer" },
            },
          },
          output_schema: { type: "object", properties: { closed: { type: "array" } } },
        },
      },
    ],
  };
  mount();
  const dialog = screen.getByRole("dialog");
  const lines = within(dialog).getByRole("group", {
    name: "list_issues arguments, before and after",
  });
  expect(lines.textContent).toContain("limit: integer");
  const whole = within(dialog).getByRole("group", { name: "list_issues schema, before and after" });
  expect(whole.textContent).toContain("and close what is stale.");
  expect(whole.textContent).toContain("closed");
});

// THE WORDING LIST IS FROZEN AT THE FIRST PRESS: a reading that brings another
// definition of a listed tool, or a third qualifying change, between the two
// presses changes neither what is listed nor what is sent. Mutation: derive
// the list or the digests again at the second press, and this is red.
test("the wording list sends what the first press listed, whatever is read meanwhile", async () => {
  vi.mocked(reviewChanges).mockResolvedValue({
    kind: "ok",
    status: 200,
    value: {
      server: "linear",
      accepted: ["list_issues", "list_teams"],
      blocked: [],
      moved: [],
      revision: null,
    },
  });
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_READ_TOOLS,
    changes: [
      wordingChange("list_issues", "List issues", "List the issues in a team", ["k", "m"]),
      wordingChange("list_teams", "List teams", "List every team", ["n", "o"]),
    ],
  };
  const { rerender } = mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept 2 wording changes" }));
  const listedWords = () =>
    within(screen.getByRole("dialog"))
      .getAllByRole("definition")
      .map((d) => d.textContent);
  const first = listedWords();
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_READ_TOOLS,
    changes: [
      wordingChange("list_issues", "List issues", "List the issues in a team", ["k", "m"]),
      wordingChange("list_teams", "List teams", "Delete every team", ["n", "z"]),
      wordingChange("get_issue", "Read an issue", "Read one issue", ["p", "q"]),
    ],
  };
  rerender(tree());
  const listed = screen.getByRole("dialog");
  expect(within(listed).getByText("Accept 2 wording changes on Linear?")).toBeDefined();
  expect(
    within(listed)
      .getAllByRole("term")
      .map((t) => t.textContent),
  ).toEqual(["list_issues", "list_teams"]);
  expect(listedWords()).toEqual(first);
  expect(listed.textContent).not.toContain("Delete");
  fireEvent.click(within(listed).getByRole("button", { name: "Accept 2 changes" }));
  await afterMicrotasks();
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { accept: { list_issues: "m".repeat(64), list_teams: "o".repeat(64) } },
    "Accepted 2 wording changes on linear",
  );
});

// A DECISION WRITTEN IN PART SAYS WHICH: the engine's 503 names what it decided
// and what it did not, and the dialog says exactly that, never "Nothing was
// changed", and reads the changes again. Mutation: word every 503 as nothing
// changed, or skip the reload, and this is red.
test("a decision written in part says what was decided and what was not", async () => {
  vi.mocked(reviewChanges).mockResolvedValue({
    kind: "refused",
    status: 503,
    code: "unavailable",
    sentence: "The decision about list_teams could not be written.",
    body: { accepted: ["list_issues"], blocked: [], moved: [], undecided: ["list_teams"] },
  });
  detail = {
    ...LINEAR_DETAIL,
    tools: LINEAR_READ_TOOLS,
    changes: [
      wordingChange("list_issues", "List issues", "List the issues in a team", ["k", "m"]),
      wordingChange("list_teams", "List teams", "List every team", ["n", "o"]),
    ],
  };
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept 2 wording changes" }));
  fireEvent.click(screen.getByRole("button", { name: "Accept 2 changes" }));
  await afterMicrotasks();
  expect(screen.getAllByText("1 change decided, 1 change not").length).toBeGreaterThan(0);
  expect(
    screen.getAllByText(/Accepted: list_issues\. Not decided: list_teams\./).length,
  ).toBeGreaterThan(0);
  expect(screen.queryByText("Nothing was changed")).toBeNull();
  expect(reload).toHaveBeenCalled();
  expect(onDone).toHaveBeenCalled();
});
