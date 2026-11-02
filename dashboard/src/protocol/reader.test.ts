/**
 * Every request a tab makes for its reader names them, and the one refusal
 * that says the browser is somebody else's hands the tab over.
 *
 * The case it exists for: somebody signs in as somebody else in another tab,
 * the close of the session this tab was opened with does not land, and this
 * tab goes on drawing the last person's screen while the cookie under it is
 * the new person's. The engine can refuse what this tab sends only if the
 * request says who it was sent for; and the tab can be handed over only if a
 * refusal of that kind reaches whoever hands tabs over. Each case below is one
 * of the two halves, or one of the two requests that must name nobody because
 * they are how a tab learns or changes who it is.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { READER_HEADER } from "../contract/identity.ts";
import { ACT_ERRORS } from "../contract/errors.ts";
import { act } from "./act.ts";
import { api } from "./api.ts";
import { auth } from "./auth.ts";
import { rest, RestError } from "./rest.ts";
import { SessionFloors } from "./session.ts";
import { setTabReader } from "./signin.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

/** The person this tab was read by, and the one the browser's cookie is now. */
const READER = "018f3a9c-0000-7000-8000-0000000000aa";
const HOLDER = "018f3a9c-0000-7000-8000-0000000000bb";

interface Sent {
  path: string;
  method: string;
  reader: string | null;
}

let sent: Sent[];
let moved: number;
let uninstall: () => void;
let reader: string | null;

/**
 * An engine whose cookie is [HOLDER]'s, refusing what its guard refuses: a
 * request that names a reader who is not the holder, `409 reader_changed`,
 * before anything runs. Everything else is answered `answer`.
 */
function engine(answer: () => Response = () => json({ person: HOLDER }, 200)): void {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const headers = new Headers(init.headers);
      const named = headers.get(READER_HEADER);
      sent.push({
        path: new URL(String(input), "http://engine.test").pathname,
        method: init.method ?? "GET",
        reader: named,
      });
      if (named !== null && named !== HOLDER) {
        return json({ error: "reader_changed", message: "the engine's sentence" }, 409);
      }
      return answer();
    }),
  );
}

const json = (payload: unknown, status: number) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
  });

beforeEach(() => {
  sent = [];
  moved = 0;
  reader = READER;
  uninstall = setTabReader({ current: () => reader, moved: () => moved++ });
});

afterEach(() => {
  uninstall();
  vi.unstubAllGlobals();
});

describe("a request made for the tab's reader", () => {
  // Mutation: send `readerHeaders()` nowhere in `attempt`, or in `stream`,
  // and these go out naming nobody, served as the new person.
  test("names them, on a read, a write, a download and a stream alike", async () => {
    engine(() => json({}, 200));
    reader = HOLDER;
    await rest.get("/agents");
    await rest.post("/iam/tokens", {});
    await rest.blob("/worklet/files/ENG/report.md").catch(() => undefined);
    await rest.stream("/assistant/conversations/c/stream").catch(() => undefined);
    expect(sent.map((s) => s.reader)).toEqual([HOLDER, HOLDER, HOLDER, HOLDER]);
  });

  // A CALLER'S OWN HEADER CANNOT NAME SOMEBODY ELSE in the tab's place: the
  // reader is the tab's, read where every request is built.
  test("names the tab's reader whatever header the caller passed", async () => {
    engine(() => json({}, 200));
    await rest.post("/iam/tokens", {}, { [READER_HEADER]: HOLDER }).catch(() => undefined);
    expect(sent[0]!.reader).toBe(READER);
  });

  test("names nobody where the tab has learned nobody", async () => {
    engine(() => json({}, 200));
    reader = null;
    await rest.get("/agents");
    expect(sent[0]!.reader).toBeNull();
  });

  test("names nobody where nothing is installed — the e2e replay, a bare suite", async () => {
    engine(() => json({}, 200));
    uninstall();
    await rest.get("/agents");
    expect(sent[0]!.reader).toBeNull();
  });
});

describe("the two requests that name nobody", () => {
  // THE READ A HAND-OVER TAKES ITS ANSWER FROM. Naming the last reader, it is
  // refused like everything else and the tab has nobody to be handed to.
  // Mutation: drop `forReader: false` from `auth.session` and this is a 409.
  test("the read of who the session is answers for whoever holds the cookie", async () => {
    engine();
    const session = await auth.session();
    expect(session.person).toBe(HOLDER);
    expect(sent[0]!.reader).toBeNull();
    expect(moved).toBe(0);
  });

  // A TOKEN EXCHANGE SIGNS SOMEBODY IN, who may be anybody: refused for the
  // last reader, a person could never sign in as anybody else from this tab.
  test("the token exchange signs in whoever the token is", async () => {
    engine(() => json({ person: HOLDER, status: "signed_in" }, 200));
    await auth.exchangeToken("a-tier-a-token");
    expect(sent[0]).toMatchObject({ path: "/auth/token", reader: null });
  });

  // AND THE KEEPALIVE IS NOT ONE OF THEM: it is how a tab nobody is using
  // learns the browser changed hands under it.
  test("the keepalive's read names the reader, and hands the tab over", async () => {
    engine();
    await auth.keepAlive().catch(() => undefined);
    expect(sent[0]).toMatchObject({ path: "/auth/session", reader: READER });
    expect(moved).toBe(1);
  });
});

describe("the refusal that says the browser is somebody else's", () => {
  // Mutation: drop the `reader_changed` arm from `noteSession` and nothing is
  // handed over — the tab keeps the last person's screen and is refused for
  // ever.
  test("hands the tab over, once per refusal, from a request, a download and a stream", async () => {
    engine();
    const err = await rest.get("/agents").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestError);
    expect((err as RestError).code).toBe("reader_changed");
    await rest.blob("/worklet/files/ENG/report.md").catch(() => undefined);
    await rest.stream("/assistant/conversations/c/stream").catch(() => undefined);
    expect(moved).toBe(3);
  });

  // THE CONTROL: a 409 that is about the request — a version that moved, a
  // name somebody took — says nothing about whose the browser is.
  test("is that refusal alone", async () => {
    reader = HOLDER;
    engine(() => json({ error: "conflict" }, 409));
    await rest.get("/agents").catch(() => undefined);
    engine(() => json({ error: "unauthorized", grants: ["admin"] }, 403));
    await rest.get("/agents").catch(() => undefined);
    expect(moved).toBe(0);
  });

  // THE RETRY THE ENGINE MUST NOT TAKE AS THE NEW PERSON'S: the toast's Retry
  // of a change whose outcome nobody knew sends the same operation again,
  // and under the new cookie the operation key would be scoped to them. It
  // names the tab's reader, so the engine refuses it unrun; nothing is sent
  // again, no floor moves, and the tab is handed over.
  test("a retried change goes out naming the reader, and is refused, not resent", async () => {
    engine();
    const floors = new SessionFloors();
    const opId = "01927c3a-4e5b-7000-8000-000000000001";
    const result = await act("set_pins", { views: { add: ["v1"] } }, { opId, floors });
    expect(sent).toHaveLength(1);
    expect(sent[0]).toMatchObject({ path: "/operator/act/set_pins", reader: READER });
    expect(result).toMatchObject({
      kind: "refused",
      code: "reader_changed",
      opId,
      sentence: ACT_ERRORS.reader_changed,
      retryable: false,
    });
    expect(floors.floor("tracker")).toBeNull();
    expect(moved).toBe(1);
  });
});

describe("the requests the socket makes beside its own", () => {
  // THE DEGRADED SNAPSHOT is the company drawn while the socket is down:
  // somebody else's is not one to draw on this tab.
  test("the degraded snapshot names the reader, and hands the tab over", async () => {
    engine();
    const read = await api.snapshot();
    expect(sent[0]).toMatchObject({ path: "/stream/snapshot", reader: READER });
    expect(read).toEqual({ state: "unread", retryAfter: null });
    expect(moved).toBe(1);
  });
});
