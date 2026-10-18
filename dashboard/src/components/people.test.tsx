/**
 * Where a person comes from: a vacant human seat, and the three things that
 * can be said about one — held, invited, vacant — with the writes that move it
 * between them.
 *
 * The invariants, in the order they cost when they go: a person is invited or
 * created ON A SEAT, which the dialog states rather than offers, so no write
 * leaves without one; a person's create is a KEYED create whose retry is the
 * first attempt's, because the engine derives the person and their first
 * password link from the operation and only the same key hands the same link
 * back; a value the engine shows once is shown once, and a link the engine no
 * longer has is said in its own words rather than drawn as nothing; a seat an
 * open invitation holds is never drawn as vacant; a read that failed is said;
 * every gesture is drawn for an admin alone; and the access a write confers is
 * picked as Admin — one switch that includes every app — and one box per app.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { useState } from "react";
import {
  AccessPicker,
  AccessTags,
  AppReachNote,
  canManagePeople,
  CancelInvitation,
  CreatePersonDialog,
  holdingLine,
  InviteDialog,
  NO_SEAT,
  seatGestures,
  SeatHolding,
  seatOptions,
  useHumanSeats,
} from "./people.tsx";
import type { HumanSeat } from "~/contract/identity.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const UUID7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** An admin: every app is included. */
const ADMIN = ["admin", "worklet", "doclet", "artifacts"];
/** A member holding every app: reads the directory and changes none of it. */
const MEMBER = ["worklet", "doclet", "artifacts"];

const SAM = { handle: "sam", name: "Sam Support" };

/** The listing: Ana's seat held, Lee's invited, Sam's vacant. */
const SEATS: HumanSeat[] = [
  {
    handle: "ana",
    name: "Ana",
    holder: { person: "p-ana", kind: "person", login: "ana.lee", stage: "suspended" },
  },
  {
    handle: "ops",
    name: "Ops Desk",
    holder: { person: "m-ci", kind: "machine", login: "ci:release", stage: "active" },
  },
  {
    handle: "lee",
    name: "Lee Ops",
    invitation: {
      id: "inv-7",
      email: "lee@example.com",
      invited_by: "ana",
      expires_at: "2026-10-14T09:00:00Z",
    },
  },
  SAM,
];

interface Sent {
  method: string;
  path: string;
  key: string | null;
  body: unknown;
}

function json(status: number, payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/**
 * The engine: `GET /iam/seats` answering `seats` (or `failure`), every write
 * recorded and answered from `writes` — a queue per `METHOD /path`, the last
 * answer repeating.
 */
let seats: HumanSeat[] = SEATS;
let failure: Response | null = null;

function engine(writes: Record<string, Response[]> = {}) {
  const sent: Sent[] = [];
  const queues = new Map<string, Response[]>(Object.entries(writes).map(([k, v]) => [k, [...v]]));
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://engine.test");
    const method = (init?.method ?? "GET").toUpperCase();
    const headers = (init?.headers ?? {}) as Record<string, string>;
    sent.push({
      method,
      path: url.pathname,
      key: headers["Idempotency-Key"] ?? null,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    if (method !== "GET") {
      const queue = queues.get(`${method} ${url.pathname}`);
      const answer = queue && (queue.length > 1 ? queue.shift()! : queue[0]!);
      return Promise.resolve(answer ? answer.clone() : json(404, { error: "no_route" }));
    }
    if (url.pathname === "/iam/seats") {
      return Promise.resolve(failure ? failure.clone() : json(200, { seats }));
    }
    return Promise.resolve(json(404, { error: "no_route" }));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return {
    writes: () => sent.filter((s) => s.method !== "GET"),
    reads: (path: string) => sent.filter((s) => s.method === "GET" && s.path === path).length,
  };
}

function mount(node: React.ReactNode) {
  const store = new Store();
  const socket = new LiveSocket(store);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>{node}</Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

/**
 * The reach note as an admin granting access reads it, on a company whose
 * chat is `chat_backend` — the one fact about the reader it reads.
 */
async function reach(node: React.ReactNode, chatBackend: string) {
  const viewer = {
    login: "ada.admin",
    owner: "ada",
    handle: "ada",
    name: "Ada",
    kind: "human",
    grants: ["admin"],
    acts: [],
    project: "",
    config_writer: true,
    config_managed_by: [],
    chat_backend: chatBackend,
    chat_vendors: [],
  };
  const store = new Store();
  store.setConnected(true);
  const socket = { query: vi.fn(async (what: string) => (what === "viewer" ? viewer : {})) };
  render(
    <ClientContext.Provider value={{ store, socket } as never}>
      <ViewerProvider>{node}</ViewerProvider>
    </ClientContext.Provider>,
  );
  await settle();
}

/** A seat's holding as its page draws it, over the real listing hook — which
 *  every signed-in reader asks, the engine masking what each may see. */
function Holding({ seat, grants }: { seat: { handle: string; name: string }; grants: string[] }) {
  const listing = useHumanSeats(true);
  return (
    <SeatHolding seat={seat} seats={listing} manages={canManagePeople(grants)} held={grants} />
  );
}

/** Every answer in flight lands. */
async function settle() {
  await act(async () => {});
  await act(async () => {});
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  seats = SEATS;
  failure = null;
});

afterEach(() => {
  cleanup();
  location.hash = "";
  vi.restoreAllMocks();
});

describe("what a seat says about what holds it", () => {
  // A CARD HAS ROOM FOR A FEW WORDS, and an INVITED seat is not a vacant one:
  // drawn as "Open seat", an administrator invited a second person onto a seat
  // the first invitation already holds. Mutation: drop the invitation arm and
  // the invited row reads "Open seat".
  test("a card's line names the holder, the invitation or the vacancy", () => {
    expect(SEATS.map(holdingLine)).toEqual([
      "Held by ana.lee",
      "Held by ci:release",
      "Invited · lee@example.com",
      "Open seat",
    ]);
    // An address this node cannot open is never drawn as one.
    expect(
      holdingLine({
        handle: "x",
        name: "X",
        invitation: { id: "i", sealed: true, expires_at: "2026-10-14T09:00:00Z" },
      }),
    ).toBe("Invited");
  });

  // WHAT A SEAT OFFERS FOLLOWS WHAT HOLDS IT, and nothing is offered on a
  // seat the listing did not answer for — a read that failed says nothing
  // about whether the seat is free.
  test("a vacant seat offers Invite and Create, an invited one its cancellation, a held one nothing", () => {
    expect(SEATS.map(seatGestures)).toEqual([[], [], ["cancel"], ["invite", "create"]]);
    expect(seatGestures(undefined)).toEqual([]);
  });

  // NO SEAT IS A SERVICE ACCOUNT'S ANSWER ONLY: offered to a person it was a
  // choice every save came back refused (`seat_required`). Mutation: prepend
  // it whatever the caller asks and the person's options carry it.
  test("a seat select offers nobody only where the caller asks for it", () => {
    expect(seatOptions([SAM]).map((o) => o.value)).toEqual(["sam"]);
    expect(seatOptions([SAM], { none: true }).map((o) => o.value)).toEqual([NO_SEAT, "sam"]);
  });

  // ADMIN WRITES THE DIRECTORY, however it was stored — a row of `[admin]`
  // alone included — and no app does.
  test("the directory is written by an admin alone", () => {
    expect(canManagePeople(ADMIN)).toBe(true);
    expect(canManagePeople(["admin"])).toBe(true);
    expect(canManagePeople(MEMBER)).toBe(false);
    expect(canManagePeople([])).toBe(false);
  });
});

describe("a seat's holding", () => {
  test("a held seat names its holder and their stage, and offers nothing", async () => {
    engine();
    mount(<Holding seat={{ handle: "ana", name: "Ana" }} grants={ADMIN} />);
    const note = (await screen.findByText("ana.lee")).closest(".crewlet-callout") as HTMLElement;
    expect(note.textContent).toMatch(/^Held by ana\.lee \(suspended\)\./);
    expect(within(note).queryByRole("button")).toBeNull();
  });

  // A SERVICE ACCOUNT ON A SEAT IS SAID TO BE ONE: its login read as a
  // person's would name somebody who does not exist.
  test("a seat a service account holds says so", async () => {
    engine();
    mount(<Holding seat={{ handle: "ops", name: "Ops Desk" }} grants={ADMIN} />);
    const note = (await screen.findByText("ci:release")).closest(".crewlet-callout") as HTMLElement;
    expect(note.textContent).toMatch(/^Held by the service account ci:release\./);
  });

  // AN INVITED SEAT NAMES ITS INVITATION and offers its cancellation, which
  // frees the seat as well as the address. Mutation: draw an invited row as
  // vacant and Invite is offered onto a seat the engine refuses.
  test("an invited seat names its invitation and cancels it", async () => {
    const eng = engine({
      "DELETE /iam/invitations/inv-7": [json(200, { id: "inv-7", outcome: "applied" })],
    });
    mount(<Holding seat={{ handle: "lee", name: "Lee Ops" }} grants={ADMIN} />);
    const note = (await screen.findByText("lee@example.com")).closest(
      ".crewlet-callout",
    ) as HTMLElement;
    expect(note.textContent).toMatch(/^Invited lee@example\.com\. Expires /);
    expect(within(note).queryByRole("button", { name: "Invite" })).toBeNull();
    expect(within(note).queryByRole("button", { name: "Add person" })).toBeNull();
    fireEvent.click(within(note).getByRole("button", { name: "Cancel invitation" }));
    const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
    expect(dialog.textContent).toMatch(/The link stops working\./);
    const before = eng.reads("/iam/seats");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel invitation" }));
    await settle();
    expect(eng.writes().map((w) => [w.method, w.path])).toEqual([
      ["DELETE", "/iam/invitations/inv-7"],
    ]);
    // EVERY LISTING ON THE PAGE HEARS IT, this one included.
    expect(eng.reads("/iam/seats")).toBeGreaterThan(before);
  });

  test("a vacant seat offers an admin Invite and Create, and anybody else neither", async () => {
    engine();
    mount(<Holding seat={SAM} grants={ADMIN} />);
    const note = (await screen.findByText("This seat is open.")).closest(
      ".crewlet-callout",
    ) as HTMLElement;
    expect(within(note).getByRole("button", { name: "Invite" })).toBeTruthy();
    expect(within(note).getByRole("button", { name: "Add person" })).toBeTruthy();
    cleanup();
    engine();
    mount(<Holding seat={SAM} grants={MEMBER} />);
    const read = (await screen.findByText("This seat is open.")).closest(
      ".crewlet-callout",
    ) as HTMLElement;
    expect(within(read).queryByRole("button")).toBeNull();
  });

  // A READ THAT FAILED IS SAID: drawn as nothing, a refused or unreachable
  // listing removed the only way to fill the seat with no word why. The
  // CONTROL is the vacant seat above, which offers the gestures. Mutation:
  // draw nothing on an error and the banner is gone.
  test("a listing that failed is said, and offers nothing", async () => {
    failure = json(500, { error: "internal", message: "boom" });
    engine();
    mount(<Holding seat={SAM} grants={ADMIN} />);
    expect(await screen.findByText(/The engine failed to answer/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Invite" })).toBeNull();
  });

  // THE DIALOG IS NOT THE LISTING'S: the write's announcement reads the
  // listing again, and a re-read refused (a node behind its identity log)
  // leaves no data. The link the engine shows once must survive it. The
  // CONTROL is the same create with the re-read answering 200. Mutation: draw
  // the dialog inside the branch that needs the listing and the dialog — and
  // the link — are gone, leaving the QueryState sentence alone.
  test("a refused re-read after a create keeps the link the engine showed once", async () => {
    const CREATED = {
      id: "p-new",
      kind: "person",
      login: "dee.ray",
      seat: "sam",
      credential: "c-first",
      url: "https://crewlet.example.com/dashboard#/reset/c-first.s3cret",
      expires_at: "2026-10-14T09:00:00Z",
      outcome: "applied",
    };
    for (const refused of [false, true]) {
      failure = null;
      const eng = engine({ "POST /iam/people": [json(201, CREATED)] });
      mount(<Holding seat={SAM} grants={ADMIN} />);
      fireEvent.click(await screen.findByRole("button", { name: "Add person" }));
      const dialog = screen.getByRole("dialog", { name: "Add someone as Sam Support" });
      fireEvent.change(within(dialog).getByLabelText("Email address"), {
        target: { value: "dee.ray@example.com" },
      });
      const before = eng.reads("/iam/seats");
      if (refused) failure = json(503, { error: "unavailable", message: "behind its log" });
      fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
      await settle();
      await settle();
      expect(eng.reads("/iam/seats")).toBeGreaterThan(before);
      if (refused)
        expect(screen.getByText("Couldn’t load this on this node. Try another node.")).toBeTruthy();
      expect(screen.getAllByRole("dialog")).toHaveLength(1);
      expect(
        screen.getByText("https://crewlet.example.com/dashboard#/reset/c-first.s3cret"),
      ).toBeTruthy();
      cleanup();
    }
  });

  // THE LISTING IS THE RUNNING COMPANY'S: a seat it does not name is one this
  // node has not applied, never one nobody holds.
  test("a seat the listing does not name is not called vacant", async () => {
    seats = SEATS.filter((s) => s.handle !== "sam");
    engine();
    mount(<Holding seat={SAM} grants={ADMIN} />);
    expect(
      await screen.findByText("This seat isn’t ready yet. Try again in a moment."),
    ).toBeTruthy();
    expect(screen.queryByText("This seat is open.")).toBeNull();
    expect(screen.queryByRole("button", { name: "Invite" })).toBeNull();
  });
});

describe("an invitation onto a seat", () => {
  function invite() {
    mount(<InviteDialog seat={SAM} held={ADMIN} onClose={() => {}} onDone={() => {}} />);
    return screen.getByRole("dialog", { name: "Invite someone as Sam Support" });
  }

  // THE SEAT IS STATED, NOT OFFERED, and always sent: an invitation names the
  // seat its redemption binds. Mutation: leave the seat out of the body and
  // the engine refuses `seat_required`.
  test("an invitation is sent keyed, for its seat, and its link is shown once", async () => {
    const eng = engine({
      "POST /iam/invitations": [
        json(201, {
          id: "inv-2",
          seat: "sam",
          url: "https://crewlet.example.com/dashboard#/invite/inv-2.s3cret",
          expires_at: "2026-10-14T09:00:00Z",
          outcome: "applied",
        }),
      ],
    });
    const dialog = invite();
    expect(within(dialog).queryByRole("combobox")).toBeNull();
    expect(within(dialog).queryByLabelText("Seat")).toBeNull();
    expect(dialog.textContent).toMatch(/The seat is held until they join\./);
    // EVERY APP IS PRE-TICKED AND ADMIN IS NOT: a person invited works in
    // every app unless somebody says otherwise.
    for (const app of ["Worklet", "Doclet", "Artifacts", "Chatlet"]) {
      expect(within(dialog).getByRole("checkbox", { name: app })).toHaveProperty("checked", true);
    }
    expect(within(dialog).getByRole("checkbox", { name: "Admin" })).toHaveProperty(
      "checked",
      false,
    );
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Doclet" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
    await settle();
    const [sent] = eng.writes();
    expect(sent?.path).toBe("/iam/invitations");
    expect(sent?.key).toMatch(UUID7);
    expect(sent?.body).toEqual({
      email: "dee@example.com",
      grants: ["worklet", "artifacts", "chatlet"],
      seat: "sam",
    });
    expect(
      within(dialog).getByText("https://crewlet.example.com/dashboard#/invite/inv-2.s3cret"),
    ).toBeTruthy();
    expect(within(dialog).getByText(/Copy it now\. You won’t see it again\. Expires/)).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Done" })).toBeTruthy();
  });

  // WHAT A PERSON WILL FIND IS SAID BEFORE THE INVITATION IS SENT: without
  // Worklet the work agents send to people never reaches them, and with no
  // app at all they read the overview only. The CONTROL is the dialog as it
  // opens, every app ticked, which says nothing.
  test("an invitation without Worklet, or with no app, says what its person will miss", () => {
    engine();
    const dialog = invite();
    expect(within(dialog).queryByText(/Without Worklet/)).toBeNull();
    expect(within(dialog).queryByText(/^Overview only:/)).toBeNull();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Worklet" }));
    expect(within(dialog).getByText(/Without Worklet/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Doclet" }));
    expect(within(dialog).queryByText(/^Overview only:/)).toBeNull();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Artifacts" }));
    expect(within(dialog).queryByText(/^Overview only:/)).toBeNull();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Chatlet" }));
    expect(within(dialog).getByText(/^Overview only:/)).toBeTruthy();
    expect(within(dialog).queryByText(/Without Worklet/)).toBeNull();
  });

  // AN UNKNOWN ANSWER IS RETRIED UNDER THE SAME KEY — the one the engine
  // handed back — so the retry is the first attempt's write. Mutation: mint
  // a key per press and the second request carries another.
  test("an unknown invitation is retried under the key the engine handed back", async () => {
    const eng = engine({
      "POST /iam/invitations": [
        json(503, {
          error: "unavailable",
          outcome: "unknown",
          op_id: "0192f4c8-0000-7000-8000-000000000001",
          message: "This node cannot answer that right now.",
        }),
        json(201, { id: "inv-2", url: "https://x/dashboard#/invite/inv-2.s", expires_at: "" }),
      ],
    });
    const dialog = invite();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
    await settle();
    expect(within(dialog).getByText(/Try again\. It won’t happen twice\./)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
    await settle();
    expect(eng.writes().map((w) => w.key)).toEqual([
      expect.stringMatching(UUID7),
      "0192f4c8-0000-7000-8000-000000000001",
    ]);
    expect(within(dialog).getByText("https://x/dashboard#/invite/inv-2.s")).toBeTruthy();
  });

  // AN UNKNOWN ANSWER'S KEY IS FOR THE REQUEST IT WAS SENT WITH: an address
  // corrected before the retry is a new invitation, under a new key.
  test("an unknown invitation corrected before its retry goes under a new key", async () => {
    const eng = engine({
      "POST /iam/invitations": [
        json(503, {
          error: "unavailable",
          outcome: "unknown",
          op_id: "0192f4c8-0000-7000-8000-000000000001",
          message: "This node cannot answer that right now.",
        }),
        json(201, { id: "inv-2", url: "https://x/dashboard#/invite/inv-2.s", expires_at: "" }),
      ],
    });
    const dialog = invite();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "jan@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
    await settle();
    expect(within(dialog).getByText(/Try again\. It won’t happen twice\./)).toBeTruthy();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "jane@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
    await settle();
    const [first, second] = eng.writes();
    expect(second?.body).toEqual({
      email: "jane@example.com",
      grants: ["worklet", "doclet", "artifacts", "chatlet"],
      seat: "sam",
    });
    expect(second?.key).toMatch(UUID7);
    expect(second?.key).not.toBe(first?.key);
    expect(second?.key).not.toBe("0192f4c8-0000-7000-8000-000000000001");
  });

  // A REFUSAL IS THE ENGINE'S SENTENCE, with the grants that would admit; and
  // a refused create's key is not sent again with the next attempt.
  test("a refusal is the engine's sentence and the grants that would admit", async () => {
    const eng = engine({
      "POST /iam/invitations": [
        json(403, {
          error: "unauthorized",
          message:
            "You don’t have the access this needs (Admin, Worklet, Doclet or Chatlet). Ask an admin for it.",
          reason: "directory",
          grants: ["admin"],
        }),
      ],
    });
    const dialog = invite();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
    await settle();
    const alert = within(dialog).getByRole("alert");
    expect(alert.textContent).not.toMatch(/Admin, Worklet, Doclet or Chatlet/);
    expect(alert.textContent).toMatch(/You need Admin access for this\./);
    fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
    await settle();
    const [first, second] = eng.writes();
    expect(second?.key).not.toBe(first?.key);
  });
});

describe("a person created on a seat", () => {
  function create() {
    mount(<CreatePersonDialog seat={SAM} held={ADMIN} onClose={() => {}} onDone={() => {}} />);
    return screen.getByRole("dialog", { name: "Add someone as Sam Support" });
  }

  const CREATED = {
    id: "p-new",
    kind: "person",
    login: "dee.ray",
    seat: "sam",
    credential: "c-first",
    url: "https://crewlet.example.com/dashboard#/reset/c-first.s3cret",
    expires_at: "2026-10-14T09:00:00Z",
    outcome: "applied",
  };

  // THE LOGIN IS THE ENGINE'S TO PROPOSE: left empty it is not sent, and the
  // done screen names the login the engine took. The seat is always sent.
  // The first password link is shown once. Mutation: send an empty login and
  // the engine refuses it as one outside the grammar.
  test("a person is created keyed on their seat, and their first password link is shown once", async () => {
    const eng = engine({ "POST /iam/people": [json(201, CREATED)] });
    const dialog = create();
    expect(within(dialog).queryByRole("combobox")).toBeNull();
    expect(dialog.textContent).toMatch(/They sign in once they set a password\./);
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee.ray@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    const [sent] = eng.writes();
    expect(sent?.path).toBe("/iam/people");
    expect(sent?.key).toMatch(UUID7);
    // EVERY APP, AS AN INVITATION PRE-TICKS IT.
    expect(sent?.body).toEqual({
      kind: "person",
      seat: "sam",
      email: "dee.ray@example.com",
      grants: ["worklet", "doclet", "artifacts", "chatlet"],
    });
    expect(within(dialog).getByText("dee.ray")).toBeTruthy();
    expect(dialog.textContent).toMatch(/dee\.ray is now Sam Support/);
    expect(
      within(dialog).getByText("https://crewlet.example.com/dashboard#/reset/c-first.s3cret"),
    ).toBeTruthy();
    expect(within(dialog).getByText(/Copy it now\. You won’t see it again\. Expires/)).toBeTruthy();
    // DONE ALONE: the person exists, and a Cancel read as a way to undo it.
    expect(within(dialog).queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Done" })).toBeTruthy();
  });

  // A LOGIN TYPED IS HELD TO THE PERSON GRAMMAR before anything is posted,
  // and one that fits is sent with the rest. The CONTROL is the case above,
  // which typed none and posted.
  test("a typed login is checked under the field, and sent when it fits", async () => {
    const eng = engine({ "POST /iam/people": [json(201, CREATED)] });
    const dialog = create();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee.ray@example.com" },
    });
    fireEvent.change(within(dialog).getByLabelText(/^Login/), { target: { value: "ci:dee" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    expect(within(dialog).getByText("Use lowercase words with dots, like jane.doe.")).toBeTruthy();
    expect(eng.writes()).toEqual([]);
    fireEvent.change(within(dialog).getByLabelText(/^Login/), { target: { value: "dee.ray" } });
    fireEvent.change(within(dialog).getByLabelText(/^Name/), { target: { value: "Dee Ray" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    expect(eng.writes().map((w) => w.body)).toEqual([
      {
        kind: "person",
        seat: "sam",
        email: "dee.ray@example.com",
        login: "dee.ray",
        name: "Dee Ray",
        grants: ["worklet", "doclet", "artifacts", "chatlet"],
      },
    ]);
  });

  // THE KEY IS WHAT MAKES THE LINK SURVIVE A DROPPED ANSWER: the engine
  // derives the person and their link from the operation, so the retry under
  // the same key hands back the SAME link rather than a second person.
  // Mutation: mint a key per press and the retry is a new create.
  test("an unknown create is retried under the engine's key and shows the link it hands back", async () => {
    const eng = engine({
      "POST /iam/people": [
        json(503, {
          error: "unavailable",
          outcome: "unknown",
          op_id: "0192f4c8-0000-7000-8000-000000000009",
          message: "This node cannot answer that right now.",
        }),
        json(201, CREATED),
      ],
    });
    const dialog = create();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee.ray@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    expect(within(dialog).getByText(/Try again\. It won’t happen twice\./)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
    await settle();
    expect(eng.writes().map((w) => w.key)).toEqual([
      expect.stringMatching(UUID7),
      "0192f4c8-0000-7000-8000-000000000009",
    ]);
    expect(
      within(dialog).getByText("https://crewlet.example.com/dashboard#/reset/c-first.s3cret"),
    ).toBeTruthy();
  });

  // RECORDED AND NOT YET APPLIED HERE (202) is still a create the engine
  // answered with its link, which is shown — it is the only time it is.
  test("a create recorded but not yet applied here still shows its link", async () => {
    engine({ "POST /iam/people": [json(202, { ...CREATED, outcome: "pending" })] });
    const dialog = create();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee.ray@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    expect(
      within(dialog)
        .getAllByRole("status")
        .some((s) => /^Saved\. The list will update in a moment\./.test(s.textContent ?? "")),
    ).toBe(true);
    expect(
      within(dialog).getByText("https://crewlet.example.com/dashboard#/reset/c-first.s3cret"),
    ).toBeTruthy();
  });

  // A RETRY THAT FINDS THE LINK CLOSED — spent, revoked or lapsed since the
  // first answer — carries no link, and the engine's own words say where a
  // new one comes from. Drawn as nothing, the done screen said the person
  // existed and gave nobody a way in. Mutation: draw only a url and the
  // detail is nowhere.
  test("a create whose link no longer opens says so in the engine's words", async () => {
    engine({
      "POST /iam/people": [
        json(201, {
          id: "p-new",
          kind: "person",
          login: "dee.ray",
          seat: "sam",
          outcome: "applied",
          detail:
            "the first password link no longer opens — issue a password reset link (POST /iam/people/{id}/password-reset)",
        }),
      ],
    });
    const dialog = create();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee.ray@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    expect(within(dialog).queryByText(/Copy it now/)).toBeNull();
    expect(within(dialog).getByText(/^The first password link no longer opens/)).toBeTruthy();
  });

  test("a refused create is the engine's sentence, and the next press a new key", async () => {
    const eng = engine({
      "POST /iam/people": [
        json(400, {
          error: "seat_required",
          message: "Name the seat this is for.",
          detail:
            "A person needs a human seat. Pick an empty one (GET /iam/seats?unheld=true lists them).",
        }),
      ],
    });
    const dialog = create();
    fireEvent.change(within(dialog).getByLabelText("Email address"), {
      target: { value: "dee.ray@example.com" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    expect(within(dialog).getByRole("alert").textContent).toMatch(/human seat/);
    fireEvent.click(within(dialog).getByRole("button", { name: "Add person" }));
    await settle();
    const [first, second] = eng.writes();
    expect(second?.key).not.toBe(first?.key);
  });
});

describe("cancelling an invitation", () => {
  // NEVER A BUTTON CALLED "CANCEL" BESIDE ANOTHER: the dismissal read "Cancel"
  // next to "Cancel invitation", and somebody meaning to cancel the
  // invitation pressed it and kept it. And a legacy invitation that names no
  // seat frees only its address. Mutation: leave the dismissal's word at its
  // default.
  test("the dismissal is not a second Cancel, and what is freed is said", () => {
    engine();
    mount(
      <CancelInvitation
        row={{ id: "inv-1", email: "old@example.com" }}
        onClose={() => {}}
        onChanged={() => {}}
      />,
    );
    const dialog = screen.getByRole("dialog", { name: "Cancel this invitation?" });
    expect(within(dialog).getByRole("button", { name: "Keep it" })).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(dialog.textContent).toMatch(/The link stops working\./);
  });
});

// ACCESS IS SAID AS WHAT IT IS: Admin alone where it is held (every app is
// included, and naming them beside it read as if they were separate), the
// apps otherwise, "Overview only" for none — and a name this build has no
// word for is drawn as itself rather than dropped.
describe("access drawn as words", () => {
  const tags = (grants: string[]) => {
    const { container, unmount } = render(<AccessTags grants={grants} />);
    const words = [...container.querySelectorAll(".crewlet-tag")].map((n) => n.textContent);
    unmount();
    return words;
  };

  test("admin is one tag, apps one each, nothing the overview", () => {
    expect(tags(["admin", "worklet", "doclet"])).toEqual(["Admin"]);
    expect(tags(["worklet", "doclet"])).toEqual(["Worklet", "Doclet"]);
    expect(tags([])).toEqual(["Overview only"]);
    expect(tags(["worklet", "later"])).toEqual(["Worklet", "later"]);
  });
});

// ADMIN IS ONE SWITCH: ticking it ticks every app and locks them as
// "Included with Admin", and stores them, so taking Admin away later leaves
// the apps rather than nothing. A permission the writer does not hold is
// offered disabled, with why. Mutation: leave the apps unlocked under Admin
// and the first assertion on `disabled` fails; drop the stored apps and the
// value after unticking Admin is empty.
describe("the access picker", () => {
  function Picker({ initial, held }: { initial: string[]; held: string[] }) {
    const [value, setValue] = useState<string[]>(initial);
    return (
      <>
        <AccessPicker value={value} onChange={setValue} held={held} />
        <output data-testid="value">{value.join(",")}</output>
      </>
    );
  }
  const box = (name: string) => screen.getByRole("checkbox", { name }) as HTMLInputElement;
  const value = () => screen.getByTestId("value").textContent;

  test("Admin locks the apps in the picker", () => {
    render(<Picker initial={[]} held={ADMIN} />);
    fireEvent.click(box("Admin"));
    expect(box("Worklet").checked).toBe(true);
    expect(box("Worklet").disabled).toBe(true);
    expect(box("Doclet").checked).toBe(true);
    expect(box("Doclet").disabled).toBe(true);
    expect(box("Artifacts").checked).toBe(true);
    expect(box("Artifacts").disabled).toBe(true);
    expect(box("Chatlet").checked).toBe(true);
    expect(box("Chatlet").disabled).toBe(true);
    expect(screen.getAllByText("Included with Admin.")).toHaveLength(4);
    expect(value()).toBe("admin,worklet,doclet,artifacts,chatlet");
    fireEvent.click(box("Admin"));
    expect(box("Worklet").disabled).toBe(false);
    expect(box("Worklet").checked).toBe(true);
    expect(value()).toBe("worklet,doclet,artifacts,chatlet");
  });

  // A ROW STORED AS ADMIN ALONE keeps its apps when Admin is taken away.
  test("a row stored as admin alone keeps every app when Admin is unticked", () => {
    render(<Picker initial={["admin"]} held={ADMIN} />);
    expect(box("Worklet").checked).toBe(true);
    fireEvent.click(box("Admin"));
    expect(value()).toBe("worklet,doclet,artifacts,chatlet");
  });

  test("a permission the writer does not hold is offered disabled, with why", () => {
    render(<Picker initial={[]} held={["worklet"]} />);
    expect(box("Admin").disabled).toBe(true);
    expect(box("Doclet").disabled).toBe(true);
    expect(box("Worklet").disabled).toBe(false);
    expect(screen.getByText("You can’t give Admin because you don’t have it.")).toBeTruthy();
  });
});

// WHAT A PERSON WILL MISS, said to the reader about themselves as well.
test("the reach note speaks to the reader about their own access", async () => {
  await reach(<AppReachNote grants={["doclet"]} you />, "chatlet");
  expect(screen.getByText(/agents can’t send you work/)).toBeTruthy();
  cleanup();
  await reach(<AppReachNote grants={["admin"]} you />, "chatlet");
  expect(document.body.textContent).toBe("");
});

// WITHOUT CHATLET, WHAT AGENTS SAY NEVER REACHES THEM — on a company running the
// engine's own chat, and only there: on a vendor's the conversation is the
// vendor's, and a reader the viewer names no backend to is told nothing it
// cannot stand behind. The tracker's note comes first where both are missing.
// Mutation: drop the backend check and the vendor row goes red; drop the
// chat note and the chatlet row does.
test.each([
  ["chatlet", true],
  ["vendor", false],
  ["none", false],
  ["", false],
])(
  "on a company whose chat is %j, a person without Chatlet is told so: %s",
  async (backend, said) => {
    await reach(<AppReachNote grants={["worklet"]} />, backend);
    expect(/Without Chatlet, what agents say to them/.test(document.body.textContent ?? "")).toBe(
      said,
    );
    cleanup();
    await reach(<AppReachNote grants={["doclet"]} />, backend);
    expect(document.body.textContent).toMatch(/Without Worklet/);
  },
);
