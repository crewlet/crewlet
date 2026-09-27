/**
 * What each write control sends, and when it declines to.
 *
 * `app/writeGate.test.tsx` holds every control's GATING; this holds the
 * change it makes — the arguments the engine receives are the only thing the
 * press is for, and a control that sent the right tool with the wrong
 * argument would be refused on every press.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { AssignButton, PinButton, RestoreButton } from "./writes.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { pick } from "~/testing.tsx";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

const JANE: ViewerState = {
  operatorID: "founder",
  operator: true,
  handle: "jane",
  name: "Jane Founder",
  acts: ["update_work_item", "restore_work_item", "set_pins", "mark_inbox"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
};

let sent: { tool: string; body: { request_id: string; args: Record<string, unknown> } }[];

beforeEach(() => {
  sent = [];
  vi.mocked(useViewer).mockReturnValue(JANE);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Nimbus",
    roles: [
      { name: "Ada Okonkwo", handle: "ada", kind: "agent" },
      { name: "Jane Founder", handle: "jane", kind: "human" },
    ],
  } as never);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({
        tool: new URL(url).pathname.split("/").pop()!,
        body: JSON.parse(init.body as string) as never,
      });
      return new Response(
        JSON.stringify({ outcome: "applied", position: "CREWLET_TRACKER_LOG@1:3" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const mount = (node: React.ReactNode) =>
  render(
    <LayerHost>
      <ToastProvider>{node}</ToastProvider>
    </LayerHost>,
  );

// CONDITIONAL ON WHAT THE PERSON LOOKED AT: the version on screen goes as
// `if_match`, so a colleague's change in between is refused rather than
// overwritten — and the reason goes with an assignee, never without one.
test("assigning names the task, the seat, the version on screen and the reason", async () => {
  mount(<AssignButton item="ENG-42" version={7} assignee="" />);
  fireEvent.click(screen.getByRole("button", { name: "Assign" }));
  const dialog = await screen.findByRole("dialog");
  pick(within(dialog).getByLabelText("Assignee"), /Ada Okonkwo/);
  fireEvent.change(within(dialog).getByLabelText(/Why it is theirs now/), {
    target: { value: "  You wrote the login flow  " },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Assign" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]!.tool).toBe("update_work_item");
  expect(sent[0]!.body.args).toEqual({
    item: "ENG-42",
    assignee: "ada",
    if_match: 7,
    reason: "You wrote the login flow",
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("assigning to whoever holds it already is declined, and says so", async () => {
  mount(<AssignButton item="ENG-42" version={7} assignee="ada" />);
  fireEvent.click(screen.getByRole("button", { name: "Reassign" }));
  const dialog = await screen.findByRole("dialog");
  const confirm = within(dialog).getByRole("button", { name: "Assign" });
  expect(confirm.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(confirm);
  await new Promise((r) => setTimeout(r, 10));
  expect(sent).toHaveLength(0);
  expect(dialog.textContent).toContain("Choose somebody other than who holds it now.");
});

// A GRID DRAWS ONE PER ROW: each is named for its task, so a screen reader's
// list of the page's buttons says which task each brings back.
test("restoring names the task, in the button's name and in what it sends", async () => {
  mount(<RestoreButton item="ENG-9" />);
  const button = screen.getByRole("button", { name: "Restore ENG-9" });
  expect(button.textContent).toContain("Restore");
  fireEvent.click(button);
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({ tool: "restore_work_item", body: { args: { item: "ENG-9" } } });
});

// A GESTURE, NEVER THE WHOLE STRIP: `set` would replace every pin, and a
// strip read in another tab before this one moved would drop that tab's.
test("pinning adds the one view and unpinning removes it, never replacing the strip", async () => {
  mount(<PinButton view="v-1" name="Triage" pinned={false} />);
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]!.body.args).toEqual({ views: { add: ["v-1"] } });
  cleanup();
  mount(<PinButton view="v-1" name="Triage" pinned />);
  fireEvent.click(screen.getByRole("button", { name: "Unpin" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1]!.body.args).toEqual({ views: { remove: ["v-1"] } });
});
