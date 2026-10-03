/**
 * Backups & retention draws what the fleet has backed up, as `backups`
 * answers it, and takes a backup through `POST /backup`.
 *
 * The invariants: the point marked newest is the ENGINE's — the one the trim
 * reads — and a point the policy does not count says so; the history is every
 * requested backup with the host that holds it, a failure drawn as one; and a
 * backup taken here goes to the directory typed, on the node serving the page,
 * with the engine's own outcome said in a toast whether or not the dialog is
 * still open, and a refusal about the directory said on the field.
 */

import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Backups } from "./Backups.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { clearToken, storeToken } from "~/protocol/authToken.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { BackupsAnswer } from "~/contract/backups.ts";

const hoursAgo = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString();

const answer = (): BackupsAnswer => ({
  policy: "engine",
  points: [
    {
      owner: "node-c",
      kind: "node",
      taken_at: hoursAgo(1),
      dir: "/var/backups/c",
      verified: false,
      bytes: 3 * 1024 * 1024,
      covers: [{ stream: "CREWLET_TRACKER_LOG", generation: 2, seq: 950 }],
      counted: false,
      newest: false,
    },
    {
      owner: "node-a",
      kind: "node",
      taken_at: hoursAgo(2),
      dir: "/var/backups/crewlet-20260929-0200",
      verified: true,
      bytes: 2 * 1024 * 1024,
      covers: [
        { stream: "CREWLET_PAGES_LOG", generation: 1, seq: 450 },
        { stream: "CREWLET_TRACKER_LOG", generation: 2, seq: 900 },
      ],
      counted: true,
      newest: true,
    },
    {
      owner: "operator",
      kind: "operator",
      taken_at: hoursAgo(30),
      verified: true,
      covers: [{ stream: "CREWLET_TRACKER_LOG", generation: 2, seq: 600 }],
      counted: true,
      newest: false,
    },
  ],
  history: [
    {
      id: "b-failed",
      at: hoursAgo(1),
      node: "node-b",
      operator: "ops-cron",
      dir: "/full/disk",
      outcome: "failed",
      summary: "ops-cron: backup to /full/disk failed",
    },
    {
      id: "b-landed",
      at: hoursAgo(2),
      node: "node-a",
      operator: "U0FOUNDER",
      actor_seat: "jane",
      dir: "/var/backups/crewlet-20260929-0200",
      outcome: "applied",
      summary: "U0FOUNDER (jane) backed up",
    },
  ],
  more: false,
  coverage: { nodes: [{ id: "node-a", answered: true, error: "" }], complete: true },
});

const org = {
  name: "Acme",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount(
  over: Partial<BackupsAnswer> = {},
  { strict = false, historySeconds = (30 * 86_400) as number | null } = {},
) {
  const store = new Store();
  store.applyOrg(org as never);
  store.applyHealth({
    status: "ok",
    node: "node-a",
    event_history_seconds: historySeconds ?? undefined,
  } as never);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  const query = vi.fn((what: string) => {
    if (what === "backups") return Promise.resolve({ ...answer(), ...over });
    return new Promise(() => {});
  });
  (socket as unknown as { query: typeof query }).query = query;
  const tree = (
    <ClientContext.Provider value={{ store, socket }}>
      <ToastProvider>
        <LayerHost>
          <FrameReadings>
            <Router>
              <Backups />
            </Router>
          </FrameReadings>
        </LayerHost>
      </ToastProvider>
    </ClientContext.Provider>
  );
  // StrictMode is how main.tsx mounts the app: every effect runs, is cleaned
  // up and runs again, which is what a flag only ever cleared does not survive.
  const view = render(strict ? <StrictMode>{tree}</StrictMode> : tree);
  return { view, query };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  });
}

/** `fetch`, answering `POST /backup` with the given status and body. */
function backupRoute(status: number, body: unknown) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      calls.push(`${init?.method ?? "GET"} ${new URL(url).pathname}${new URL(url).search}`);
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      );
    }),
  );
  return calls;
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  storeToken("t0ken");
});

afterEach(() => {
  cleanup();
  clearToken();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

// THE ROW MARKED NEWEST IS THE ENGINE'S. node-c's copy is newer, and was never
// verified: the screen must not promote it because it is the latest date.
test("the newest counted point is the engine's, and the others say why they are not", async () => {
  mount();
  await settle();
  const points = screen.getByText("Newest per owner").closest(".crewlet-card") as HTMLElement;
  const row = (text: string) => within(points).getByText(text).closest(".grid-row") as HTMLElement;
  expect(within(row("node-a")).getByText("counted · newest")).toBeTruthy();
  expect(within(row("node-c")).getByText("not verified")).toBeTruthy();
  expect(within(row("Operator acknowledgement")).getByText("counted")).toBeTruthy();
  // AN ACKNOWLEDGEMENT HAS NO SIZE, and says so rather than printing zero.
  expect(
    within(row("Operator acknowledgement")).getByText("The engine never saw this copy"),
  ).toBeTruthy();
  // The tile names the same point.
  expect(
    screen.getByText("Newest counted backup").closest(".crewlet-statcard")?.textContent,
  ).toMatch(/node-a/);
});

// EVERY REQUESTED BACKUP, WITH THE HOST THAT HOLDS IT, a failure drawn as one
// and a bound token drawn as its person.
test("the history lists each backup with its node, its person and its outcome", async () => {
  mount();
  await settle();
  const history = screen.getByText("Backup history").closest(".crewlet-card") as HTMLElement;
  const failed = within(history).getByText("/full/disk").closest(".grid-row") as HTMLElement;
  expect(within(failed).getByText("Failed")).toBeTruthy();
  expect(within(failed).getByText("node-b")).toBeTruthy();
  expect(within(failed).getByText("ops-cron")).toBeTruthy();
  const landed = within(history)
    .getByText("/var/backups/crewlet-20260929-0200")
    .closest(".grid-row") as HTMLElement;
  expect(within(landed).getByText("Written")).toBeTruthy();
  expect(within(landed).getByRole("link", { name: /Jane Founder/ })).toBeTruthy();
  expect(screen.getByText("Requested").closest(".crewlet-statcard")?.textContent).toMatch(
    /1 failed/,
  );
});

// THE COUNT SAYS THE SPAN IT COVERS. A history that filled its page stops
// short of the event window, so its failures are not thirty days' worth.
test("a history that filled its page counts failures in that page, not thirty days", async () => {
  mount({ more: true });
  await settle();
  const tile = screen.getByText("Requested").closest(".crewlet-statcard")?.textContent ?? "";
  expect(tile).toMatch(/2\+/);
  expect(tile).toMatch(/1 failed · in the newest 2/);
  expect(tile).not.toMatch(/30 days/);
});

// THE SPAN IS THE ENGINE'S. The history is an event-log read, floored where
// the log is, so the count and the empty state name the floor the engine
// REPORTED — a literal here would go on saying thirty days after the retention
// moved — and an engine that reported none is not given a number.
test("the history's span is the one the engine reports", async () => {
  mount({}, { historySeconds: 7 * 86_400 });
  await settle();
  expect(screen.getByText("Requested").closest(".crewlet-statcard")?.textContent).toMatch(
    /1 failed · in the last 7 days/,
  );
  cleanup();

  mount({ history: [] }, { historySeconds: 7 * 86_400 });
  await settle();
  expect(screen.getByText("No backup was asked for in the last 7 days")).toBeTruthy();
  expect(screen.getByText(/the store keeps 7 days/)).toBeTruthy();
  cleanup();

  mount({ history: [] }, { historySeconds: null });
  await settle();
  expect(screen.getByText("No backup was asked for in the event log's window")).toBeTruthy();
  expect(document.body.textContent).not.toMatch(/30 days/);
});

// A BACKUP TAKEN HERE goes to the directory typed — offered beside this node's
// own last copy — and the engine's outcome is said once it lands.
test("taking a backup posts the directory and says what was written", async () => {
  const calls = backupRoute(200, {
    taken_at: "2026-09-30T12:00:00Z",
    finished_at: "2026-09-30T12:00:04Z",
    node_id: "node-a",
    stores: [{ bytes: 1024 }],
    streams: [{ bytes: 1024 }],
  });
  const { query } = mount();
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Take a backup" }));
  const field = screen.getByLabelText(/Directory/) as HTMLInputElement;
  // THE OFFER IS A SIBLING OF THIS NODE'S LAST COPY, with a fresh leaf.
  expect(field.value).toMatch(/^\/var\/backups\/crewlet-\d{8}-\d{6}$/);
  fireEvent.change(field, { target: { value: "/var/backups/tonight" } });
  fireEvent.click(screen.getByRole("button", { name: "Take backup" }));
  await settle();
  expect(calls).toEqual(["POST /backup?dir=%2Fvar%2Fbackups%2Ftonight"]);
  expect((await screen.findAllByText("Backup written on node-a")).length).toBeGreaterThan(0);
  expect(screen.getAllByText(/\/var\/backups\/tonight — 2\.0 KB in 4s/).length).toBeGreaterThan(0);
  // AND THE HISTORY IS READ AGAIN, so the row it just made appears.
  expect(query.mock.calls.filter(([what]) => what === "backups").length).toBeGreaterThan(1);
});

// A RELATIVE PATH IS REFUSED BEFORE THE ROUND TRIP, and the engine's own
// refusal of a directory is said on the field, in its words.
test("a directory the engine cannot use is said on the field", async () => {
  const calls = backupRoute(400, {
    error: "backup_failed",
    detail: "backup: unusable destination: not empty",
  });
  mount();
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Take a backup" }));
  const field = screen.getByLabelText(/Directory/) as HTMLInputElement;
  fireEvent.change(field, { target: { value: "backups/tonight" } });
  expect(screen.getByText(/An absolute path/)).toBeTruthy();
  expect((screen.getByRole("button", { name: "Take backup" }) as HTMLButtonElement).disabled).toBe(
    true,
  );

  fireEvent.change(field, { target: { value: "/var/backups/full" } });
  fireEvent.click(screen.getByRole("button", { name: "Take backup" }));
  await settle();
  expect(calls).toHaveLength(1);
  expect(await screen.findByText(/unusable destination: not empty/)).toBeTruthy();
});

// A DESTINATION THE HOST CANNOT CREATE — a path through a regular file — is
// the caller's mistake: the engine answers 400 with its reason, and the reason
// is the field's, never a banner sending the reader to the engine's log.
test("a directory the host cannot create is said on the field", async () => {
  backupRoute(400, {
    error: "backup_failed",
    detail:
      "backup: unusable destination: cannot create /etc/passwd/x: mkdir /etc/passwd: not a directory",
  });
  mount();
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Take a backup" }));
  fireEvent.change(screen.getByLabelText(/Directory/), { target: { value: "/etc/passwd/x" } });
  fireEvent.click(screen.getByRole("button", { name: "Take backup" }));
  await settle();
  // THE FIELD SAYS IT: the message sits in the Directory field, whose paths
  // are drawn as chips — so its text is read whole rather than per node.
  const dialog = screen.getByRole("dialog");
  const field = screen.getByLabelText(/Directory/).closest(".field, .crewlet-field") as HTMLElement;
  expect(field?.textContent ?? "").toMatch(/mkdir \/etc\/passwd: not a directory/);
  expect(within(dialog).queryByRole("alert")?.textContent ?? "").not.toMatch(/log/);
  expect(screen.queryByText(/api_backup_failed/)).toBeNull();
});

// A READER WITH NO TOKEN is refused the section, and the button says why.
test("without an operator token the section is refused and the button says why", async () => {
  clearToken();
  const { query } = mount();
  await settle();
  expect(query.mock.calls.some(([what]) => what === "backups")).toBe(false);
  const button = screen.getByRole("button", { name: "Take a backup" });
  expect(
    button.getAttribute("aria-disabled") ?? String((button as HTMLButtonElement).disabled),
  ).toMatch(/true/);
});

// CLOSING DOES NOT LOSE THE OUTCOME. The engine finishes a copy whether or not
// anybody waits, so the dialog may be closed mid-copy — and a failure that
// arrives after it closed must still be said, not dropped with the dialog.
test("a failure that arrives after the dialog closed is said as a notice", async () => {
  let answer: (r: Response) => void = () => {};
  vi.stubGlobal(
    "fetch",
    vi.fn(() => new Promise<Response>((resolve) => (answer = resolve))),
  );
  mount();
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Take a backup" }));
  fireEvent.change(screen.getByLabelText(/Directory/), { target: { value: "/var/backups/x" } });
  fireEvent.click(screen.getByRole("button", { name: "Take backup" }));
  await settle();
  expect(screen.getByRole("button", { name: "Copying…" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Leave it copying" }));
  await settle();
  expect(screen.queryByLabelText(/Directory/)).toBeNull();

  answer(
    new Response(JSON.stringify({ error: "backup_failed" }), {
      status: 500,
      headers: { "Content-Type": "application/json" },
    }),
  );
  await settle();
  expect(
    (await screen.findAllByText("No backup was written to /var/backups/x")).length,
  ).toBeGreaterThan(0);
});

// UNDER STRICTMODE, AS THE APP IS MOUNTED, the dialog still knows it is open:
// its effects run, are cleaned up and run again, and a "still open" flag only
// ever cleared would stay cleared — the dialog never closing on success, its
// button stuck on "Copying…", and a refusal toasted instead of said on the
// field.
test("under StrictMode a taken backup closes the dialog", async () => {
  backupRoute(200, {
    taken_at: "2026-09-30T12:00:00Z",
    finished_at: "2026-09-30T12:00:04Z",
    node_id: "node-a",
    stores: [{ bytes: 1024 }],
    streams: [],
  });
  mount({}, { strict: true });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Take a backup" }));
  fireEvent.change(screen.getByLabelText(/Directory/), {
    target: { value: "/var/backups/tonight" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Take backup" }));
  await settle();
  expect(screen.queryByLabelText(/Directory/)).toBeNull();
  expect(screen.queryByRole("button", { name: "Copying…" })).toBeNull();
});

test("under StrictMode a refused directory is said on the field", async () => {
  backupRoute(400, {
    error: "backup_failed",
    detail: "backup: unusable destination: not empty",
  });
  mount({}, { strict: true });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Take a backup" }));
  fireEvent.change(screen.getByLabelText(/Directory/), {
    target: { value: "/var/backups/full" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Take backup" }));
  await settle();
  const field = screen.getByLabelText(/Directory/).closest(".field, .crewlet-field") as HTMLElement;
  expect(field?.textContent ?? "").toMatch(/unusable destination: not empty/);
  expect(screen.getByRole("button", { name: "Take backup" })).toBeTruthy();
  expect(screen.queryByText("No backup was written to /var/backups/full")).toBeNull();
});

// A COMMAND IS CODE. With nothing backed up the points table says how to take
// one, and the CLI half of that sentence is something to type, set as code the
// way the rest of Settings sets a command.
test("with nothing backed up, the command that takes one is code", async () => {
  mount({ points: [] });
  await settle();
  expect(screen.getByText("Nothing has been backed up")).toBeTruthy();
  expect(screen.getByText("crewlet backup -dir").tagName).toBe("CODE");
});
