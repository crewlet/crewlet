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
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Backups } from "./Backups.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { BackupPointRow, BackupsAnswer } from "~/contract/backups.ts";
import type { RetentionReport } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { engineAlarms } from "~/test/retentionAlarms.ts";

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
      // A PRINCIPAL BOUND TO NO SEAT, under its own login (`iam.ActorFor`).
      id: "b-failed",
      at: hoursAgo(1),
      node: "node-b",
      actor: "ops-cron",
      actor_kind: "operator",
      dir: "/full/disk",
      outcome: "failed",
      summary: "ops-cron: backup to /full/disk failed",
    },
    {
      // A PERSON BOUND TO A SEAT asks AS the seat, and the credential they
      // asked through rides beside them.
      id: "b-landed",
      at: hoursAgo(2),
      node: "node-a",
      actor: "jane",
      actor_kind: "human",
      operator_id: "session:s-1",
      dir: "/var/backups/crewlet-20260929-0200",
      outcome: "applied",
      summary: "jane backed up",
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

/** Who is reading: somebody holding the grant a backup is taken under. */
const OPERATOR = { login: "ops", owner: "ops", grants: ["admin"] };

function mount(
  over: Partial<BackupsAnswer> = {},
  {
    strict = false,
    historySeconds = (30 * 86_400) as number | null,
    viewer = OPERATOR as unknown,
    retention = null as RetentionReport | null,
  } = {},
) {
  const store = new Store();
  store.applyOrg(withDerived(org) as never);
  // A NULL FLOOR IS NO HEALTH READ YET: the socket is open and no frame has
  // arrived, so how far back the log goes is not known.
  if (historySeconds !== null) {
    store.applyHealth(healthFrame({ node: "node-a", event_history_seconds: historySeconds }));
  }
  store.setConnected(true);
  const socket = new LiveSocket(store);
  const query = vi.fn((what: string) => {
    if (what === "backups") return Promise.resolve({ ...answer(), ...over });
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "retention" && retention) return Promise.resolve(retention);
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
  await afterMicrotasks();
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
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

// THE ROW MARKED NEWEST IS THE ENGINE'S. node-c's copy is newer, and was never
// verified: the screen must not promote it because it is the latest date.
test("the newest counted point is the engine's, and the others say why they are not", async () => {
  mount();
  await settle();
  const points = screen
    .getByText("Newest per node and log")
    .closest(".crewlet-card") as HTMLElement;
  const row = (text: string) =>
    within(points).getByText(text).closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(row("node-a")).getByText("counted · newest")).toBeTruthy();
  expect(within(row("node-c")).getByText("not verified")).toBeTruthy();
  expect(within(row("Operator acknowledgement")).getByText("counted")).toBeTruthy();
  // AN ACKNOWLEDGEMENT HAS NO SIZE, and says so rather than printing zero.
  expect(
    within(row("Operator acknowledgement")).getByText("The engine never saw this copy"),
  ).toBeTruthy();
  // The tile names the same point.
  expect(
    screen.getByText("Oldest backup a trim reads").closest(".crewlet-statcard")?.textContent,
  ).toMatch(/node-a/);
});

// EACH LOG'S ACKNOWLEDGEMENT IS ITS OWN ROW, AND EACH IS READ. Under `operator`
// an acknowledgement names one log and the register files it under that log,
// so two acknowledgements share an owner: both rows are drawn, both are what a
// trim reads, and the tile names the older — the log the alarm names first.
test("two logs acknowledged in turn are two rows, both read, the older on the tile", async () => {
  const ack = (stream: string, hours: number): BackupPointRow => ({
    owner: "operator",
    kind: "operator",
    taken_at: hoursAgo(hours),
    verified: true,
    covers: [{ stream, generation: 1, seq: 100 }],
    counted: true,
    newest: true,
  });
  // ONE OWNER, TWO ROWS: a row keyed by its owner alone is two rows React
  // reconciles as one, and it says so here before it draws the wrong one.
  const errors = vi.spyOn(console, "error").mockImplementation(() => {});
  mount({
    policy: "operator",
    points: [ack("CREWLET_PAGES_LOG", 2), ack("CREWLET_TRACKER_LOG", 50)],
  });
  await settle();
  expect(errors.mock.calls.map((c) => String(c[0])).filter((m) => /same key/.test(m))).toEqual([]);
  errors.mockRestore();
  const points = screen
    .getByText("Newest per node and log")
    .closest(".crewlet-card") as HTMLElement;
  expect(within(points).getAllByText("Operator acknowledgement")).toHaveLength(2);
  expect(within(points).getAllByText("counted · newest")).toHaveLength(2);
  const tile =
    screen.getByText("Oldest backup a trim reads").closest(".crewlet-statcard")?.textContent ?? "";
  expect(tile).toMatch(/2d ago/);
  expect(tile).toMatch(/oldest of 2/);
});

// EVERY REQUESTED BACKUP, WITH THE HOST THAT HOLDS IT, a failure drawn as one
// and a person drawn as the seat they asked as, with the credential beside.
test("the history lists each backup with its node, its person and its outcome", async () => {
  mount();
  await settle();
  const history = screen.getByText("Backup history").closest(".crewlet-card") as HTMLElement;
  const failed = within(history)
    .getByText("/full/disk")
    .closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(failed).getByText("Failed")).toBeTruthy();
  expect(within(failed).getByText("node-b")).toBeTruthy();
  expect(within(failed).getByText("ops-cron")).toBeTruthy();
  const landed = within(history)
    .getByText("/var/backups/crewlet-20260929-0200")
    .closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(landed).getByText("Written")).toBeTruthy();
  expect(within(landed).getByRole("link", { name: /Jane Founder/ })).toBeTruthy();
  expect(within(landed).getByText("session:s-1")).toBeTruthy();
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
  expect(screen.getByText("Older backups still count above.")).toBeTruthy();
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
  expect(screen.getAllByText(/\/var\/backups\/tonight: 2\.0 KiB in 4s/).length).toBeGreaterThan(0);
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
  expect(screen.getByText("Use an absolute path on the engine’s host.")).toBeTruthy();
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
  const field = screen.getByLabelText(/Directory/).closest(".crewlet-form-field") as HTMLElement;
  expect(field?.textContent ?? "").toMatch(/mkdir \/etc\/passwd: not a directory/);
  expect(within(dialog).queryByRole("alert")?.textContent ?? "").not.toMatch(/log/);
  expect(screen.queryByText(/api_backup_failed/)).toBeNull();
});

// A READER WITHOUT THE GRANT is told which one before pressing: the button is
// drawn, disabled with the grant a backup is taken under.
test("without Admin the button is held and names the grant", async () => {
  mount({}, { viewer: { login: "jane.doe", owner: "jane.doe", grants: [] } });
  await settle();
  const button = screen.getByRole("button", { name: "Take a backup" });
  expect(
    button.getAttribute("aria-disabled") ?? String((button as HTMLButtonElement).disabled),
  ).toMatch(/true/);
  expect(button.getAttribute("title")).toMatch(/needs Admin/);
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
  const field = screen.getByLabelText(/Directory/).closest(".crewlet-form-field") as HTMLElement;
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

// NOTHING BACKED UP IS SAID ONCE, BY THE FIGURE. The two lists each drew an
// empty state of their own as well, three ways of saying it beside the alarm
// that already does; with nothing at all to list they are not drawn.
test("with nothing at all backed up, the figure says so and the lists are not drawn", async () => {
  mount({ points: [], history: [] });
  await settle();
  expect(screen.getByText("none yet")).toBeTruthy();
  expect(screen.queryByText("Nothing has been backed up")).toBeNull();
  expect(screen.queryByText("Backup history")).toBeNull();
});

// A HISTORY A NODE COULD NOT READ IS NOT AN EMPTY ONE: the lists are drawn so
// the note naming the missing node is.
test("an empty history some node could not answer still draws its lists", async () => {
  mount({
    points: [],
    history: [],
    coverage: {
      nodes: [
        { id: "node-a", answered: true, error: "" },
        { id: "node-b", answered: false, error: "no answer within the read budget" },
      ],
      complete: false,
    },
  });
  await settle();
  expect(screen.getByText("Backup history")).toBeTruthy();
  expect(screen.getByText(/node-b/)).toBeTruthy();
});

// WHAT IS WRONG LEADS THE PAGE: the alarms come before the record of backups
// and the panels, and the backup alarm offers the backup it asks for.
test("the alarms lead the page, and the backup they ask for is the page bar's one", async () => {
  const report = {
    v: 1,
    node_id: "node-a",
    at: hoursAgo(0),
    domains: [],
    nodes: [],
    register_readable: true,
    snapshots: [],
    replica: { store_bytes: 0, projected_join_seconds: 0, rejoin_window_seconds: 0 },
    backup_owner: "platform-oncall",
    alarms: engineAlarms("young"),
  } as RetentionReport;
  mount({}, { retention: report });
  await settle();
  const alarms = document.getElementById("retention-alarms") as HTMLElement;
  const record = screen.getByText("Oldest backup a trim reads");
  expect(alarms.compareDocumentPosition(record) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  // THE OWNER IS A FIGURE OF THE RECORD, named once.
  expect(screen.getByText("platform-oncall")).toBeTruthy();
  // ONE TAKE A BACKUP ON THE SCREEN: the bar's, never a second beside the alarm.
  expect(within(alarms).queryByRole("button", { name: "Take a backup" })).toBeNull();
  const take = screen.getAllByRole("button", { name: "Take a backup" });
  expect(take).toHaveLength(1);
  fireEvent.click(take[0]!);
  expect(await screen.findByRole("dialog")).toBeTruthy();
});

// "OPEN ALARMS" LANDS ON THE ALARMS. The engine panel's way to them is
// `at=alarms`, and their list sits below the backups and the banners, so the
// screen brings it into view once the report has drawn it, and only then.
describe("arriving for the alarms", () => {
  let scrolled: Element[] = [];
  beforeEach(() => {
    scrolled = [];
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      writable: true,
      value(this: Element) {
        scrolled.push(this);
      },
    });
    // The list is below the fold, as it is under a page of backups.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const top = this.id === "retention-alarms" ? 2_000 : 0;
      return { top, bottom: top + 40, left: 0, right: 0, width: 0, height: 40 } as DOMRect;
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
  });

  const report = {
    v: 1,
    node_id: "node-a",
    at: hoursAgo(0),
    domains: [],
    nodes: [],
    register_readable: true,
    snapshots: [],
    replica: { store_bytes: 0, projected_join_seconds: 0, rejoin_window_seconds: 0 },
    // THE ENGINE'S OWN RENDERING of a fleet with no backup yet: one alarm,
    // every log named beside its one sentence.
    alarms: engineAlarms("young"),
  } as RetentionReport;
  /**
   * The alarm's headline: the measurement every log was taken alike on, as a
   * sentence, after the logs it names.
   */
  const headline =
    /: No verified backup of this log has been recorded, and the policy asks for one every 24h\.$/;

  test("the alarm list is brought into view", async () => {
    location.hash = "#/settings/backups?at=alarms";
    mount({}, { retention: report });
    await settle();
    expect(scrolled.map((el) => el.id)).toEqual(["retention-alarms"]);
    // Its rows say the kind in words and what was measured as a sentence,
    // with no dash between them.
    expect(screen.getByText("Backup age")).toBeTruthy();
    expect(screen.getByText(headline).textContent).not.toMatch(/\u2014/);
    // EVERY LOG IS NAMED, each a way to its own page.
    const alarms = document.getElementById("retention-alarms") as HTMLElement;
    for (const domain of (report.alarms[0]?.domains ?? []).flatMap((d) => d.domains)) {
      expect(within(alarms).getByRole("link", { name: domain })).toBeTruthy();
    }
  });

  test("a reader who came for the page is left at its top", async () => {
    location.hash = "#/settings/backups";
    mount({}, { retention: report });
    await settle();
    expect(screen.getByText(headline)).toBeTruthy();
    expect(scrolled).toEqual([]);
  });
});
