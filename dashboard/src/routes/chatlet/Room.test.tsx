/**
 * A room, and the three things it must never get wrong.
 *
 * OPTIMISTIC IS ALLOWED AND A LIE IS NOT. A message this tab has said and the
 * company has not yet acknowledged is drawn as pending; a write whose outcome
 * nobody can vouch for is drawn as unknown, with the retry that reuses its own
 * operation key; and neither becomes "sent" until the room hands the message
 * back.
 *
 * EVERY WRITE IS A PRESS THROUGH THE ACT TRANSPORT (ADR-0024), so the engine
 * these cases talk to is `POST /operator/act/{tool}` and what they assert is
 * the tool and its arguments.
 *
 * AND A THREAD IS ONE LEVEL DEEP, which is a property of the screen as much as
 * of the engine: a reply that offered a thread of its own would promise a
 * shape the write path cannot file.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import {
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "~/app/assistant/context.ts";

import { ReadCursors } from "./cursor.ts";
import { LIVE_REFETCH_MS } from "./live.ts";
import { SHORTEN_FIRST } from "./Composer.tsx";
import { DELETE_IN_FLIGHT, EDIT_IN_FLIGHT, NOTHING_CHANGED } from "./Transcript.tsx";
import { MAX_BODY_BYTES } from "~/contract/chat.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { Store, type ChatPresencePush, type RoomMember, type QueryName } from "~/protocol/index.ts";
import { MAX_CHAT_LIVE } from "~/protocol/store.ts";
import { LoneRoom } from "./testkit.tsx";
import {
  ADA,
  actEngine,
  applied,
  mountChat,
  refused,
  refusedForNow,
  unknown,
  type ActReply,
  type Pressed,
} from "~/test/chat.tsx";
import { LinkHostsProvider } from "~/lib/linkHosts.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { withDerived } from "~/test/org.ts";
import { session } from "~/protocol/session.ts";
import { PAGE } from "./page.ts";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 40 },
};

const channel = {
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};

function message(seq: number, over: Record<string, unknown> = {}) {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: "bo",
      author_kind: "agent",
      body: `line ${seq}`,
      created_at: "2026-01-01T00:00:00Z",
      ...over,
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

/** What the fake engine answers, mutable between calls. */
let answers: Partial<Record<QueryName, unknown>>;
let presses: Pressed[];
let reply: (tool: string) => ActReply | Promise<ActReply>;

beforeEach(() => {
  reply = (tool) => applied(tool, { channel: "room-1" });
  presses = actEngine((tool) => reply(tool));
  answers = {
    chat_channel: { ...served, channel, revision: 3, message_seq: 2, member: true, members: [] },
    chat_messages: { ...served, channel_id: "room-1", messages: [message(2), message(1)] },
  };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** The room, read by [ADA] — or by `viewer`, for a case about who reads it. */
function mount(viewer: Record<string, unknown> = ADA) {
  const cursors = new ReadCursors({ flush: async () => undefined });
  return mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    { answers, viewer },
  );
}

/** [ADA], holding Admin — who may take down anybody's message. */
const ADMIN_ADA = { ...ADA, grants: ["admin"] };

/** The presses of one tool, in order. */
const pressesOf = (tool: string) => presses.filter((press) => press.tool === tool);

/** A committed record arriving on the socket, which is what makes a room
 *  re-read. It carries no body — that is the engine's own rule. */
function frame(store: Store, seq: number, over: Record<string, unknown> = {}) {
  store.applyChatChange({
    channel_id: "room-1",
    op: "post",
    op_id: `op-${seq}`,
    message_id: `m-${seq}`,
    channel_seq: seq,
    at: "2026-01-01T00:01:00Z",
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
    ...over,
  } as never);
}

async function say(text: string) {
  const box = (await screen.findByLabelText(/^Message/)) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: text } });
  // SEND OPENS ONCE THE VIEWER READ SAYS WHO IS WRITING: a message is a
  // change made as the person, and nobody is a person until then.
  const send = screen.getByRole("button", { name: /^send$/i });
  await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.submit(box.closest("form")!);
}

/** The room's own refusal, as the callout above the transcript says it. */
function callout(text: RegExp): HTMLElement | null {
  return (
    [...document.querySelectorAll<HTMLElement>(".crewlet-callout")].find((el) =>
      text.test(el.textContent ?? ""),
    ) ?? null
  );
}

test("a message this tab has said is pending until the room hands it back", async () => {
  // OPTIMISTIC, AND HONEST ABOUT IT. The record applied — the engine said so —
  // and what is on screen is still this tab's draft: the message the company
  // holds carries the BROKER's instant and its place in the room, neither of
  // which exists until the transcript brings it back.
  reply = (tool) => applied(tool, { channel: "room-1", message_id: "m-3" });
  const { store } = mount();
  await screen.findByText("line 2");

  await say("shipping it");
  await waitFor(() => expect(screen.getByText("shipping it")).toBeTruthy());
  expect(document.querySelector(".chat-message.pending")).toBeTruthy();

  // The record lands: a frame says so, the room re-reads, and the message is
  // in the answer.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [
      message(3, { id: "m-3", author: "ada", author_kind: "human", body: "shipping it" }),
      message(2),
      message(1),
    ],
  };
  frame(store, 3);

  await waitFor(() => expect(document.querySelector(".chat-message.pending")).toBeNull());
  // AND EXACTLY ONCE. The pending row leaves by being superseded, so the two
  // are never both on screen.
  expect(screen.getAllByText("shipping it")).toHaveLength(1);
});

test("a write nobody can vouch for is never drawn as sent", async () => {
  // An outcome of `unknown`: the record may be on the log and may never be.
  // The only honest rendering says so and offers the retry.
  reply = (tool) => unknown(tool);
  mount();
  await screen.findByText("line 2");

  await say("did that land?");
  await waitFor(() => expect(screen.getByText(/couldn’t confirm this was sent/i)).toBeTruthy());
  expect(document.querySelector('.chat-message[data-state="unknown"]')).toBeTruthy();
  expect(screen.queryByText(/^Sending/)).toBeNull();
});

test("retrying an unknown write reuses its operation key, because a fresh one would say it twice", async () => {
  // A post arbitrates nothing at the broker, so the operation key is the
  // ONLY thing that can tell a resubmission from a second remark — the engine
  // derives the message's own id from it.
  reply = (tool) => unknown(tool);
  mount();
  await screen.findByText("line 2");
  await say("did that land?");

  const first = await screen.findByRole("button", { name: /send again/i });
  fireEvent.click(first);
  await waitFor(() => expect(pressesOf("post_message")).toHaveLength(2));
  const [sent, again] = pressesOf("post_message");
  expect(sent!.opId).toBeTruthy();
  expect(again!.opId).toBe(sent!.opId);
  // AND IT IS THE MESSAGE AS TYPED: what it names is the engine's reading.
  expect(sent!.args).toEqual({ channel: "room-1", body: "did that land?" });
});

test("a retry offline is held with why, in the room", async () => {
  // A RETRY IS A PRESS LIKE ANY OTHER: offline it is held with the sentence
  // every control is held with, rather than drawn pressable and sending
  // nothing — the row would flicker to "Sending…" and back, which reads as a
  // second attempt that failed.
  reply = (tool) => unknown(tool);
  const { store } = mount();
  await screen.findByText("line 2");
  await say("did that land?");
  const retry = await screen.findByRole("button", { name: /send again/i });
  act(() => store.setConnected(false));
  await waitFor(() => expect(retry.getAttribute("title")).toBe(WRITE_REASONS.offline));
  expect(retry.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(retry);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("post_message")).toHaveLength(1);
});

test("a reply's retry offline is held with why, in a thread as in the room", async () => {
  // THE THREAD'S PENDING ROW IS HELD AS THE ROOM'S IS: offline, its "Send
  // again" is held with the sentence every control is held with, rather than
  // drawn pressable and sending nothing.
  reply = (tool) => unknown(tool);
  answers.chat_thread = { ...served, channel_id: "room-1", root: message(2), replies: [] };
  const { store } = mount();
  const open = await screen.findAllByRole("button", { name: /reply in thread/i });
  fireEvent.click(open[open.length - 1]!);
  const pane = await waitFor(() => {
    const found = document.querySelector<HTMLElement>(".chat-thread");
    if (!found) throw new Error("no thread pane");
    return found;
  });
  const box = within(pane).getByLabelText(/^Reply/) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "in the thread?" } });
  const send = within(pane).getByRole("button", { name: /^reply$/i });
  await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.submit(box.closest("form")!);
  const retry = await within(pane).findByRole("button", { name: /send again/i });
  expect(pressesOf("reply_in_thread")[0]!.args).toEqual({
    channel: "room-1",
    reply_to: "m-2",
    body: "in the thread?",
  });

  act(() => store.setConnected(false));
  await waitFor(() => expect(retry.getAttribute("title")).toBe(WRITE_REASONS.offline));
  expect(retry.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(retry);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("reply_in_thread")).toHaveLength(1);
});

test("a refusal is shown as the engine's own sentence rather than as a failure", async () => {
  reply = (tool) =>
    refused(
      tool,
      "invalid",
      "post_message refused that: `body`: a message is 40960 bytes against a 32768 cap",
      422,
    );
  mount();
  await screen.findByText("line 2");
  await say("anyone there?");
  await waitFor(() => expect(screen.getByText(/40960 bytes/)).toBeTruthy());
  // A REFUSAL OF THE MESSAGE IS NOT A BUTTON TO PRESS AGAIN: it is refused
  // the same way however often it is sent.
  expect(screen.queryByRole("button", { name: /try again/i })).toBeNull();
});

test("a post the room's state refuses is drawn as a conflict, never as a retry", async () => {
  // AN ARCHIVED ROOM IS THE ROOM'S STATE (`409 conflict`, the one rule every
  // domain's refusals are classed by): the same post lands once somebody
  // reopens the room and not before, so the row says it met the room as it
  // is now, in the dashboard's own words for the class, and offers nothing
  // to press again.
  reply = (tool) =>
    refused(
      tool,
      "conflict",
      "post_message refused that: room-1 was archived on 2026-02-01T00:00:00Z",
      409,
    );
  mount();
  await screen.findByText("line 2");
  await say("anyone there?");
  await waitFor(() => expect(screen.getByText(ACT_ERRORS.conflict)).toBeTruthy());
  expect(screen.queryByRole("button", { name: /try again/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /send again/i })).toBeNull();
});

test("a refusal the node made can be tried again, under the same operation key", async () => {
  // THE NODE SAID WAITING CHANGES IT — its `Retry-After` — which is the only
  // refusal a press offers again: a node's 503 without one (a full log, a
  // record it cannot decode) is refused the same way however often it is sent.
  reply = (tool) => refusedForNow(tool, "post_message did not land: nothing was written");
  mount();
  await screen.findByText("line 2");
  await say("anyone there?");
  fireEvent.click(await screen.findByRole("button", { name: /try again/i }));
  await waitFor(() => expect(pressesOf("post_message")).toHaveLength(2));
  expect(pressesOf("post_message")[1]!.opId).toBe(pressesOf("post_message")[0]!.opId);
});

test("offline, nothing can be said, and Send says why", async () => {
  // OFFLINE IS DISABLED, NEVER QUEUED: a message sent "later" is one the
  // person has walked away from believing it was said. The BOX stays open —
  // it is the browser that cannot send, not the room that takes nothing —
  // and Send is held with the sentence every control is held with offline.
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    { answers, connected: false },
  );
  const box = (await screen.findByLabelText(/^Message/)) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "later" } });
  const send = screen.getByRole("button", { name: /^send$/i });
  await waitFor(() => expect(send.getAttribute("title")).toBe(WRITE_REASONS.offline));
  expect(box.disabled).toBe(false);
  fireEvent.submit(box.closest("form")!);
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
  expect(box.value).toBe("later");
});

test("a reply in the transcript says which thread it is in and opens no new one", async () => {
  // THE OTHER HALF OF ONE LEVEL DEEP, in the room rather than in the pane: a
  // reply is a message like any other and appears in the transcript, and the
  // only thread it can ever belong to is the one it already has.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(4, { id: "m-4", thread_root: "m-2", body: "answered" }), message(2)],
  };
  mount();
  await screen.findByText("answered");
  const row = document.querySelector('[data-message-id="m-4"]')!;
  expect(row.textContent).toContain("In thread");
  expect([...row.querySelectorAll("button")].map((b) => b.textContent)).not.toContain(
    "Reply in thread",
  );
});

test("a thread pane shows one level: a reply offers no thread of its own", async () => {
  // A reply to a reply carries the same root, so an affordance on a reply
  // would promise a shape the write path cannot file. The reply says which
  // thread it is IN instead.
  answers.chat_thread = {
    ...served,
    channel_id: "room-1",
    root: message(2),
    replies: [message(5, { id: "m-5", thread_root: "m-2", body: "first answer" })],
    participants: ["bo"],
  };
  mount();
  const open = await screen.findAllByRole("button", { name: /reply in thread/i });
  fireEvent.click(open[0]!);

  await waitFor(() => expect(screen.getByText("first answer")).toBeTruthy());
  const pane = document.querySelector(".chat-thread")!;
  expect(pane.querySelectorAll("button")).toBeTruthy();
  // Nothing inside the pane opens another thread.
  expect([...pane.querySelectorAll("button")].map((b) => b.textContent)).not.toContain(
    "Reply in thread",
  );
});

// A MESSAGE'S LINKS ARE DRAWN BY THE DASHBOARD'S LINK RULES, read `checked`
// whoever wrote the message: an address on a host the company configured opens
// as the dashboard rebuilds it, in a new tab, a GitHub or GitLab address only
// under an owner that host's `link_hosts` row names; any other is its words,
// whole, and never a control; and a reply's links are drawn in the thread pane
// as in the room.
describe("a message's links", () => {
  /** The company's code host, as the frame's `link_hosts` names it. */
  const HOSTS = { hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }] };

  /** The room inside the frame's reading of the company's hosts. */
  function mountLinked(hosts: unknown = HOSTS) {
    answers.link_hosts = hosts;
    const cursors = new ReadCursors({ flush: async () => undefined });
    return mountChat(
      <LinkHostsProvider>
        <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />
      </LinkHostsProvider>,
      { answers },
    );
  }

  /** The room's transcript holding one message carrying `links`. */
  function saying(links: string[], over: Record<string, unknown> = {}) {
    answers.chat_messages = {
      ...served,
      channel_id: "room-1",
      messages: [message(3, { id: "m-3", body: "the links", links, ...over })],
    };
  }

  const row = async () => {
    await screen.findByText("the links");
    return document.querySelector<HTMLElement>('[data-message-id="m-3"]')!;
  };

  test("an address on the company's code host opens as rebuilt, in a new tab", async () => {
    saying(["https://github.com/gophrase/backend/pull/14?ref=x#discussion_r1"]);
    mountLinked();
    const link = await waitFor(() => {
      const found = document.querySelector<HTMLAnchorElement>('[data-message-id="m-3"] a');
      if (!found) throw new Error("no link in the row yet");
      return found;
    });
    expect(link.getAttribute("href")).toBe("https://github.com/gophrase/backend/pull/14");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link.textContent).toBe("GitHub: gophrase/backend#14 (opens in a new tab)");
  });

  // Mutation: draw a link as written and each address here is a control.
  test.each([
    ["an agent's", "agent"],
    ["a person's", "human"],
  ])("%s address on a host the company did not configure is its words, whole", async (_, kind) => {
    const address = "https://notes.example.net/c?q=the%20plan";
    saying([address], { author_kind: kind });
    mountLinked();
    const found = await row();
    await waitFor(() => expect(found.textContent).toContain(address));
    expect(found.querySelector("a")).toBeNull();
    expect(found.querySelector(`[title="${address}"]`)).not.toBeNull();
  });

  /** The addresses the row links, once the frame has read the company's hosts. */
  const linked = async () => {
    await row();
    return waitFor(() => {
      const anchors = [...document.querySelectorAll('[data-message-id="m-3"] a')];
      if (anchors.length === 0) throw new Error("no link in the row yet");
      return anchors.map((a) => a.getAttribute("href"));
    });
  };

  // AN OWNER THE CODE HOST DOES NOT NAME IS SOMEBODY ELSE'S, on the company's
  // own host too: its address is drawn whole and inert beside the one the
  // dashboard rebuilt under the company's owner.
  //
  // Mutation: drop `ownsOwner` from describeGitHub, and the address under
  // other-org is a control.
  test("an address on the company's code host under an owner it does not name is its words, whole", async () => {
    const foreign = "https://github.com/other-org/tool/pull/3";
    saying([foreign, "https://github.com/gophrase/backend/pull/14"]);
    mountLinked();
    expect(await linked()).toEqual(["https://github.com/gophrase/backend/pull/14"]);
    const found = await row();
    expect(found.textContent).toContain(foreign);
    expect(found.querySelector(`[title="${foreign}"]`)).not.toBeNull();
  });

  // A CODE HOST WHOSE ROW NAMES NO OWNERS VOUCHES FOR NONE, so an address under
  // the company's own organization opens nothing there either, while one on
  // the company's Jira opens as rebuilt.
  //
  // Mutation: drop `ownsOwner` from describeGitHub, and the pull request is a
  // control.
  test("a code host whose row names no owners opens no address on it", async () => {
    const pull = "https://github.com/gophrase/backend/pull/14";
    saying([pull, "https://gophrase.atlassian.net/browse/GP-1?focus=1"]);
    mountLinked({
      hosts: [
        { kind: "github", url: "https://github.com" },
        { kind: "jira", url: "https://gophrase.atlassian.net" },
      ],
    });
    expect(await linked()).toEqual(["https://gophrase.atlassian.net/browse/GP-1"]);
    const found = await row();
    expect(found.textContent).toContain(pull);
    expect(found.querySelector(`[title="${pull}"]`)).not.toBeNull();
  });

  test.each([
    "//notes.example.net/x",
    "#/settings/iam",
    "javascript:alert(1)",
    "ftp://files.example.net/a",
    "data:text/html,hi",
  ])("%s is never a control", async (link) => {
    saying([link]);
    mountLinked();
    const found = await row();
    expect(found.querySelector("a")).toBeNull();
    expect(found.textContent).toContain(link);
  });

  test("a reply's links are drawn in the thread pane", async () => {
    answers.chat_thread = {
      ...served,
      channel_id: "room-1",
      root: message(2),
      replies: [
        message(5, {
          id: "m-5",
          thread_root: "m-2",
          body: "the fix",
          links: ["https://github.com/gophrase/backend/pull/15"],
        }),
      ],
      participants: ["bo"],
    };
    mountLinked();
    const open = await screen.findAllByRole("button", { name: /reply in thread/i });
    fireEvent.click(open[0]!);
    await waitFor(() => expect(screen.getByText("the fix")).toBeTruthy());
    const pane = document.querySelector<HTMLElement>(".chat-thread")!;
    await waitFor(() =>
      expect(pane.querySelector("a")?.getAttribute("href")).toBe(
        "https://github.com/gophrase/backend/pull/15",
      ),
    );
  });
});

/**
 * An open thread under m-2, paged to its second page, over a fake engine whose
 * pages the case moves: what each page answers, every `chat_thread` asked (by
 * the cursor it named, "" for the first page), and how many times the room's
 * own newest page was read — the read every frame causes, which is what says
 * a frame has been handled.
 */
async function threadPagedForward() {
  // THE ROOT IS WHICHEVER MESSAGE THE READER OPENED, as the pane asked for it.
  const opened = { root: "" };
  const reply = (seq: number, body: string, over: Record<string, unknown> = {}) =>
    message(seq, { id: `m-${seq}`, thread_root: opened.root, body, ...over });
  const pages = {
    first: [reply(5, "first answer"), reply(6, "second answer")],
    later: [reply(7, "third answer")],
    // WHERE A CASE HOLDS THE LATER PAGE'S READS, each waiting for the
    // replies it is to answer with.
    holding: null as ((replies: ReturnType<typeof reply>[]) => void)[] | null,
  };
  const cursors = new ReadCursors({ flush: async () => undefined });
  const { store, socket } = mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      answer: async (what, params) => {
        if (what === "chat_thread") {
          opened.root = String(params?.root_id);
          const root = message(Number(opened.root.slice(2)));
          const holding = pages.holding;
          if (holding && params?.cursor === "c-6") {
            return new Promise((resolve) =>
              holding.push((replies) =>
                resolve({ ...served, channel_id: "room-1", root, replies }),
              ),
            );
          }
          const page =
            params?.cursor === "c-6"
              ? { replies: pages.later }
              : { replies: pages.first, next_cursor: "c-6" };
          return {
            ...served,
            channel_id: "room-1",
            root: message(Number(opened.root.slice(2))),
            ...page,
          };
        }
        return answers[what as QueryName] ?? {};
      },
    },
  );
  const open = await screen.findAllByRole("button", { name: /reply in thread/i });
  fireEvent.click(open[0]!);
  await screen.findByText("second answer");
  fireEvent.click(screen.getByRole("button", { name: /more replies/i }));
  await screen.findByText("third answer");
  const asked = () =>
    socket.query.mock.calls
      .filter(([what]) => what === "chat_thread")
      .map(([, params]) => String((params as Record<string, unknown> | undefined)?.cursor ?? ""));
  const roomReads = () =>
    socket.query.mock.calls.filter(([what]) => what === "chat_messages").length;
  const pane = document.querySelector<HTMLElement>(".chat-thread")!;
  return { store, pages, reply, asked, roomReads, pane, root: opened.root };
}

/** Apply one frame and wait until the room has re-read for it — the same
 *  run of the live signal every view of the room answers on. */
async function handled(store: Store, roomReads: () => number, seq: number, over = {}) {
  const before = roomReads();
  act(() => frame(store, seq, over));
  await waitFor(() => expect(roomReads()).toBeGreaterThan(before));
  // THE NEXT FRAME IS NOT COALESCED INTO THIS ONE.
  await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 20));
}

test("a frame in another thread asks nothing of an open one", async () => {
  // A THREAD IS BOUNDED IN WHO TAKES PART, NOT IN HOW MUCH THEY SAY. Every
  // frame in the room used to re-ask every page the pane held — the root's
  // and each one paged to — against the socket's four in-flight queries.
  const { store, asked, roomReads } = await threadPagedForward();
  const before = asked().length;
  await handled(store, roomReads, 9, { thread_root: "m-77", channel_kind: "public" });
  await handled(store, roomReads, 10, { op: "delete", message_id: "m-30" });
  await handled(store, roomReads, 11, { op: "react", message_id: "m-31" });
  expect(asked()).toHaveLength(before);
});

test("a reply taken down on a later page re-reads that page and no other", async () => {
  const { store, pages, reply, asked, roomReads, pane } = await threadPagedForward();
  const before = asked().length;
  pages.later = [reply(7, "", { deleted_at: "2026-01-01T00:02:00Z", deleted_by: "bo" })];
  await handled(store, roomReads, 9, { op: "delete", message_id: "m-7" });
  await waitFor(() => expect(within(pane).getByText(/Deleted by bo/)).toBeTruthy());
  expect(asked().slice(before)).toEqual(["c-6"]);
});

test("a re-read of a thread page asked while one is out is made after it, and drawn", async () => {
  // THE EARLIER-ASKED ANSWER IS THE OLDER WORD: drawn last, it would put back
  // a reply as it was before the change the later read was asked for. So the
  // later read waits for the earlier one to land, and what it reads stays.
  const { store, pages, reply, roomReads, pane } = await threadPagedForward();
  const held: ((replies: ReturnType<typeof reply>[]) => void)[] = [];
  pages.holding = held;
  await handled(store, roomReads, 9, { op: "edit", message_id: "m-7" });
  await handled(store, roomReads, 10, { op: "delete", message_id: "m-7" });
  expect(held).toHaveLength(1);
  await act(async () => held[0]!([reply(7, "third answer, edited")]));
  await waitFor(() => expect(held).toHaveLength(2));
  await act(async () =>
    held[1]!([reply(7, "", { deleted_at: "2026-01-01T00:02:00Z", deleted_by: "bo" })]),
  );
  expect(within(pane).getByText(/Deleted by bo/)).toBeTruthy();
  expect(within(pane).queryByText("third answer, edited")).toBeNull();
});

test("a new reply in the thread re-reads its last page, where it lands", async () => {
  const { store, pages, reply, asked, roomReads, pane, root } = await threadPagedForward();
  const before = asked().length;
  pages.later = [reply(7, "third answer"), reply(8, "fourth answer")];
  await handled(store, roomReads, 8, { thread_root: root, channel_kind: "public" });
  await waitFor(() => expect(within(pane).getByText("fourth answer")).toBeTruthy());
  expect(asked().slice(before)).toEqual(["c-6"]);
});

test("an erase re-reads every page of an open thread, the first one included", async () => {
  // EACH PAGE IS THE TRUTH WITHIN ITS RANGE: the cursor used to be a
  // parameter of the one question, so after "More replies" a re-read asked
  // only from there on — a tombstone on the first page was never drawn and
  // an erased reply stayed on screen. An erase names no message, so it
  // reaches every page.
  const { store, pages, reply, asked, roomReads, pane } = await threadPagedForward();
  const before = asked().length;
  pages.first = [reply(5, "", { deleted_at: "2026-01-01T00:02:00Z", deleted_by: "bo" })];
  pages.later = [reply(7, "third answer, fixed", { edited_at: "2026-01-01T00:02:00Z" })];
  await handled(store, roomReads, 9, { op: "erase", message_id: undefined });
  await waitFor(() => expect(within(pane).getByText(/Deleted by bo/)).toBeTruthy());
  await waitFor(() => expect(within(pane).getByText("third answer, fixed")).toBeTruthy());
  expect(within(pane).queryByText("first answer")).toBeNull();
  expect(within(pane).queryByText("second answer")).toBeNull();
  expect(asked().slice(before).sort()).toEqual(["", "c-6"]);
});

test("more replies asked while an erase moves the last page's end are still drawn", async () => {
  // A PAGE ASKED FROM THE OLD END OF ONE A RE-READ MOVED was dropped by the
  // pane's own pager: "More replies" pressed, nothing added, and the button
  // back. The engine here pages two replies at a time, forwards, on a cursor
  // naming the last reply a page holds.
  const thread = { replies: [5, 6, 7, 8, 9, 10] };
  const held: (() => void)[] = [];
  const pageFrom = (root: string, after: number) => {
    const rest = thread.replies.filter((seq) => seq > after);
    const replies = rest.slice(0, 2);
    return {
      ...served,
      channel_id: "room-1",
      root: message(Number(root.slice(2))),
      replies: replies.map((seq) =>
        message(seq, { id: `m-${seq}`, thread_root: root, body: `reply ${seq}` }),
      ),
      ...(rest.length > 2 ? { next_cursor: `c-${replies[replies.length - 1]}` } : {}),
    };
  };
  const cursors = new ReadCursors({ flush: async () => undefined });
  const { store, socket } = mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      answer: async (what, params) => {
        if (what !== "chat_thread") return answers[what as QueryName] ?? {};
        const root = String(params?.root_id);
        const cursor = String(params?.cursor ?? "");
        if (cursor === "c-8" && held.length === 0) {
          // THE THIRD PAGE IS ON ITS WAY while the erase lands.
          return new Promise((resolve) => held.push(() => resolve(pageFrom(root, 8))));
        }
        return pageFrom(root, cursor ? Number(cursor.slice(2)) : 0);
      },
    },
  );
  const open = await screen.findAllByRole("button", { name: /reply in thread/i });
  fireEvent.click(open[0]!);
  await screen.findByText("reply 6");
  fireEvent.click(screen.getByRole("button", { name: /more replies/i }));
  await screen.findByText("reply 8");
  fireEvent.click(screen.getByRole("button", { name: /more replies/i }));
  await waitFor(() => expect(held).toHaveLength(1));
  const roomReads = () =>
    socket.query.mock.calls.filter(([what]) => what === "chat_messages").length;
  thread.replies = [5, 6, 8, 9, 10];
  await handled(store, roomReads, 11, { op: "erase", message_id: undefined });
  await act(async () => held[0]!());
  const pane = document.querySelector<HTMLElement>(".chat-thread")!;
  await waitFor(() => expect(within(pane).getByText("reply 10")).toBeTruthy());
  await waitFor(() => expect(within(pane).queryByText("reply 7")).toBeNull());
  for (const seq of [5, 6, 8, 9, 10]) {
    expect(within(pane).getAllByText(`reply ${seq}`)).toHaveLength(1);
  }
  expect(within(pane).queryByRole("button", { name: /more replies/i })).toBeNull();
});

test("opening another thread over an open one draws only the new thread", async () => {
  // ONE PANE PER ROOT: the pane used to be reused across roots, so what it
  // had read and paged for one thread was merged into the next.
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      answer: async (what, params) => {
        if (what === "chat_thread") {
          const root = String(params?.root_id);
          const seq = root === "m-2" ? 5 : 6;
          // THE FIRST THREAD HAS A SECOND PAGE, which the reader pages to.
          if (params?.cursor) {
            return {
              ...served,
              channel_id: "room-1",
              root: message(1),
              replies: [message(8, { id: "m-8", thread_root: root, body: "more under m-1" })],
            };
          }
          return {
            ...served,
            channel_id: "room-1",
            root: message(root === "m-2" ? 2 : 1),
            replies: [
              message(seq, { id: `m-${seq}`, thread_root: root, body: `answer under ${root}` }),
            ],
            ...(root === "m-1" ? { next_cursor: "c-6" } : {}),
          };
        }
        return answers[what as QueryName] ?? {};
      },
    },
  );
  const open = await screen.findAllByRole("button", { name: /reply in thread/i });
  fireEvent.click(open[0]!);
  await screen.findByText("answer under m-1");
  fireEvent.click(screen.getByRole("button", { name: /more replies/i }));
  await screen.findByText("more under m-1");
  fireEvent.click(screen.getAllByRole("button", { name: /reply in thread/i })[0]!);
  await screen.findByText("answer under m-2");
  expect(screen.queryByText("answer under m-1")).toBeNull();
  expect(screen.queryByText("more under m-1")).toBeNull();
});

test("an archived room says why it takes no messages rather than greying a box", async () => {
  answers.chat_channel = {
    ...served,
    channel: { ...channel, archived_at: "2026-02-01T00:00:00Z" },
    revision: 4,
    message_seq: 2,
    member: true,
    members: [],
  };
  mount();
  await waitFor(() => expect(callout(/archived/i)).not.toBeNull());
  expect(callout(/can still read it/i)).not.toBeNull();
  // THE ROOM CLOSES THE BOX, in its own words.
  const box = screen.getByLabelText(/^Message/) as HTMLTextAreaElement;
  expect(box.disabled).toBe(true);
  expect(box.placeholder).toMatch(/archived/i);
  // AND ITS SETTINGS STAY OPEN: the engine takes a patch on an archived room,
  // so the opener is not held by the archive.
  const topic = screen.getByRole("button", { name: /^topic$/i });
  await waitFor(() => expect(topic.getAttribute("aria-disabled")).not.toBe("true"));
});

test("a private room the reader is not in refuses every write, a reaction included", async () => {
  answers.chat_channel = {
    ...served,
    channel: { ...channel, kind: "private", name: "board" },
    revision: 4,
    message_seq: 2,
    member: false,
    members: [],
  };
  mount();
  await waitFor(() => expect(callout(/private room you’re not in/i)).not.toBeNull());
  // A PATCH FROM OUTSIDE IS REFUSED TOO, so the settings opener is held with
  // the same sentence rather than opening a dialog whose Save cannot land.
  const topic = screen.getByRole("button", { name: /^topic$/i });
  expect(topic.getAttribute("aria-disabled")).toBe("true");
  expect(topic.getAttribute("title")).toMatch(/private room you’re not in/i);
});

test("a room this build cannot classify is read-only rather than guessed at", async () => {
  // An open enum read two-valued falls out as "not private", which is how a
  // newer peer's room would become writable by anyone. The engine refuses
  // every write to one; the screen offers none.
  answers.chat_channel = {
    ...served,
    channel: { ...channel, kind: "broadcast" },
    revision: 4,
    message_seq: 2,
    member: true,
    members: [],
  };
  mount();
  await waitFor(() => expect(callout(/newer version of the engine/i)).not.toBeNull());
  const topic = screen.getByRole("button", { name: /^topic$/i });
  expect(topic.getAttribute("title")).toMatch(/newer version of the engine/i);
});

/** A `chat_presence` roster for room-1, as the presence hub hands a joining socket one. */
function present(
  ...members: (Partial<RoomMember> & Pick<RoomMember, "seat" | "kind" | "activity">)[]
): ChatPresencePush {
  return {
    channel_id: "room-1",
    roster: true,
    upsert: members.map((m, i) => ({
      session: `s-${i}`,
      focus: {},
      at: "2026-01-01T00:00:00Z",
      ...m,
    })),
    leave: [],
    truncated: false,
  };
}

test("a seat working on an answer is drawn under the message it is answering", async () => {
  // The frame says WHICH message the reply will land under, and that is the
  // whole point of the field: an indicator at the top of the room would put
  // it somewhere the answer is not going to appear.
  const { store } = mount();
  await screen.findByText("line 2");
  store.applyChatPresence(
    present(
      { seat: "bo", kind: "human", activity: "viewing" },
      {
        seat: "bo",
        kind: "agent",
        activity: "editing",
        focus: { thread: "m-2" },
        status: "is drafting a reply",
      },
    ),
  );
  await waitFor(() => expect(screen.getByText(/bo is drafting a reply/)).toBeTruthy());
  const row = document.querySelector('[data-message-id="m-2"]')!;
  expect(row.textContent).toContain("is drafting a reply");
  // THE KIT'S WORKING MARK — the info dot, pulsing, beside its word — as the
  // sidebar and the org chart draw a working seat, rather than a still dot of
  // the screen's own that said "working" in a mark no other screen uses.
  expect(row.querySelector(".crewlet-status .crewlet-status-dot--info.is-pulsing")).not.toBeNull();
});

test("a seat working on the room rather than a message is drawn in the presence band", async () => {
  const { store } = mount();
  await screen.findByText("line 2");
  store.applyChatPresence(
    present({ seat: "bo", kind: "agent", activity: "editing", status: "is reading the room" }),
  );
  const said = await screen.findByText(/bo is reading the room/);
  const mark = said.closest(".crewlet-status");
  expect(mark?.closest(".chat-presence")).not.toBeNull();
  expect(mark?.querySelector(".crewlet-status-dot--info.is-pulsing")).not.toBeNull();
  // NOT UNDER A MESSAGE: an empty `thread` is a seat working on the room.
  expect(document.querySelector("[data-message-id] .crewlet-status")).toBeNull();
});

test("who is here is drawn as the chart draws them: a person in a circle, an agent as itself", async () => {
  // A CHIP HANDED ONLY A NAME takes the kit's default, the agent's squircle,
  // which drew every person looking at a room in an agent's outline. The
  // chart says what each seat is and how an agent chose to look.
  const { store } = mount();
  store.applyOrg(
    withDerived({
      roles: [
        { name: "Cy Person", handle: "cy", kind: "human" },
        { name: "Bo Agent", handle: "bo", avatar: { character: "hexlet", color: "rose" } },
      ],
    }) as never,
  );
  await screen.findByText("line 2");
  store.applyChatPresence(
    present(
      { seat: "cy", kind: "human", activity: "viewing" },
      { seat: "bo", kind: "human", activity: "viewing" },
    ),
  );
  const band = await waitFor(() => {
    const found = document.querySelector(".chat-presence");
    expect(found).not.toBeNull();
    return found!;
  });
  const chip = (handle: string) =>
    band.querySelector(`a.seat-chip[href$="/${handle}"] .crewlet-avatar`) as HTMLElement | null;
  expect(chip("cy")?.classList.contains("crewlet-avatar--human")).toBe(true);
  expect(chip("bo")?.classList.contains("crewlet-avatar--agent")).toBe(true);
  expect(chip("bo")?.classList.contains("crewlet-avatar--hue-rose")).toBe(true);
  expect(chip("bo")?.querySelector('[data-character="hexlet"]')).not.toBeNull();
});

test("a person's message names the person, never the kind it was stored under", async () => {
  // EVERY MESSAGE A PERSON WRITES is stored under the seat they hold with the
  // kind `human` (`iam.ActorFor`), so a tag for that kind would put "human"
  // on every person's message in every room — and a row a newer peer wrote
  // may carry `operator`, which is drawn as a person too, never as an agent.
  // An agent's is still said.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [
      message(5, { id: "m-5", author: "ada", author_kind: "human", body: "from a person" }),
      message(4, { id: "m-4", author: "bo", author_kind: "agent", body: "from a seat" }),
    ],
  };
  const answer = async (what: string) =>
    what === "chat_thread"
      ? {
          ...served,
          channel_id: "room-1",
          root: message(4, { id: "m-4", author: "bo", author_kind: "agent", body: "from a seat" }),
          replies: [
            message(6, {
              id: "m-6",
              author: "ada",
              author_kind: "operator",
              thread_root: "m-4",
              body: "a person, in a thread",
            }),
          ],
        }
      : (answers[what as QueryName] ?? {});
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom
      channelID="room-1"
      viewer="ada"
      nameOf={(handle) => (handle === "ada" ? "Ada Lovelace" : handle)}
      cursors={cursors}
    />,
    { answer },
  );
  await screen.findByText("from a person");
  const mine = document.querySelector<HTMLElement>('[data-message-id="m-5"]')!;
  expect(within(mine).getByText("Ada Lovelace")).toBeTruthy();
  expect(within(mine).queryByText("human")).toBeNull();
  const seat = document.querySelector<HTMLElement>('[data-message-id="m-4"]')!;
  expect(within(seat).getByText("agent")).toBeTruthy();

  // AND THE THREAD SAYS IT THE SAME WAY, through the same line.
  fireEvent.click(within(seat).getByRole("button", { name: /reply in thread/i }));
  const pane = await waitFor(() => {
    const found = document.querySelector<HTMLElement>(".chat-thread");
    if (!found || !within(found).queryByText("a person, in a thread")) throw new Error("no thread");
    return found;
  });
  expect(within(pane).getByText("Ada Lovelace")).toBeTruthy();
  expect(within(pane).queryByText("operator")).toBeNull();
  expect(within(pane).queryByText("human")).toBeNull();
  expect(within(pane).getByText("agent")).toBeTruthy();
});

test("the author is offered the edit and the delete, an admin the delete, and nobody else either", async () => {
  // AN EDIT IS THE AUTHOR'S ALONE, not even an admin's: a remark somebody
  // else can rewrite is a remark attributed to a person who did not make it.
  // A DELETE IS THE AUTHOR'S OR AN ADMIN'S, which is the engine's own rule
  // for `delete_message` — the tombstone records who took it down — so a
  // reader who is neither is offered no control the engine would refuse.
  // The fixtures are authored by `bo` and the reader is `ada`.
  mount();
  await screen.findByText("line 2");
  await new Promise((r) => setTimeout(r, 20));
  expect(screen.queryByRole("button", { name: /^delete$/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /^edit$/i })).toBeNull();

  cleanup();
  mount(ADMIN_ADA);
  await screen.findByText("line 2");
  await waitFor(() => expect(screen.getAllByRole("button", { name: /^delete$/i }).length).toBe(2));
  expect(screen.queryByRole("button", { name: /^edit$/i })).toBeNull();

  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  cleanup();
  mount();
  await screen.findByText("mine");
  await waitFor(() => expect(screen.getByRole("button", { name: /^edit$/i })).toBeTruthy());
  expect(screen.getByRole("button", { name: /^delete$/i })).toBeTruthy();
});

test("a delete asks once before it writes, because a tombstone is not undone", async () => {
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  mount();
  await screen.findByText("mine");

  fireEvent.click(await screen.findByRole("button", { name: /^delete$/i }));
  expect(presses).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: /^really delete\?$/i }));
  await waitFor(() => expect(pressesOf("delete_message")).toHaveLength(1));
  expect(pressesOf("delete_message")[0]!.args).toEqual({ channel: "room-1", message_id: "m-5" });
});

test("an edit sends the new words, and what they name is the engine's reading", async () => {
  // THE TOOL READS THE BODY AGAIN, as it does for a post: the mentions are
  // re-derived from the new words on the engine's side, so the edit carries
  // the words and nothing about them.
  answers.chat_channel = {
    ...served,
    channel,
    revision: 3,
    message_seq: 2,
    member: true,
    members: [{ handle: "bo", joined_at: "2026-01-01T00:00:00Z" }],
  };
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  mount();
  await screen.findByText("mine");

  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  const box = screen.getByLabelText("Rewrite this message");
  fireEvent.change(box, { target: { value: "over to you @bo" } });
  fireEvent.submit(box.closest("form")!);

  await waitFor(() => expect(pressesOf("edit_message")).toHaveLength(1));
  expect(pressesOf("edit_message")[0]!.args).toEqual({
    channel: "room-1",
    message_id: "m-5",
    body: "over to you @bo",
  });
});

test("a rewrite the engine refused stays in front of the person who typed it", async () => {
  // CONFIRMED, NOT OPTIMISTIC: the editor closes on an answer that took the
  // edit, and on a refusal it keeps the words — the refusal names what was
  // wrong with them, above the transcript, and a person who has to type them
  // again to fix them has been punished for the engine's answer.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  reply = (tool) =>
    tool === "edit_message"
      ? refused(tool, "bad_params", "`body` is longer than a message may be")
      : applied(tool);
  mount();
  await screen.findByText("mine");

  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  const box = screen.getByLabelText("Rewrite this message") as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "a very long rewrite" } });
  fireEvent.submit(box.closest("form")!);
  await screen.findByText(/longer than a message may be/);
  expect((screen.getByLabelText("Rewrite this message") as HTMLTextAreaElement).value).toBe(
    "a very long rewrite",
  );

  // AND TAKEN, IT CLOSES: the row is read back at the write's floor.
  reply = (tool) => applied(tool);
  fireEvent.submit(screen.getByLabelText("Rewrite this message").closest("form")!);
  await waitFor(() => expect(screen.queryByLabelText("Rewrite this message")).toBeNull());
  expect(pressesOf("edit_message")).toHaveLength(2);
});

test("an empty rewrite is held rather than sent", async () => {
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  mount();
  await screen.findByText("mine");
  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  const box = screen.getByLabelText("Rewrite this message");
  fireEvent.change(box, { target: { value: "   " } });
  const save = screen.getByRole("button", { name: /^save$/i });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  expect(save.getAttribute("title")).toMatch(/^Write the message first/);
  fireEvent.submit(box.closest("form")!);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("edit_message")).toHaveLength(0);
});

test("a rewrite near the cap counts as the composer and every field do", async () => {
  // ONE MEASURE AND ONE LINE for a message body (`bodyBudget`), the editor's
  // as the composer's, and both as every capped field in the dashboard says
  // it: from the last fifth, in `textBudget`'s words.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  mount();
  await screen.findByText("mine");
  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  const box = screen.getByLabelText("Rewrite this message");
  const near = Math.ceil(MAX_BODY_BYTES * 0.8);
  fireEvent.change(box, { target: { value: "x".repeat(near) } });
  const count = screen.getByText(`${near} of ${MAX_BODY_BYTES} bytes`);
  expect(count.id).not.toBe("");
  expect(box.getAttribute("aria-describedby") ?? "").toContain(count.id);
});

test("a rewrite past the cap is held with why, and sends nothing", async () => {
  // THE EDITOR MEASURES AS THE COMPOSER DOES (`bodyBudget`): it used to send
  // a rewrite past the engine's cap and learn of it after the round trip,
  // while the room's own composer held the same body before a press.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  mount();
  await screen.findByText("mine");
  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  const box = screen.getByLabelText("Rewrite this message");
  fireEvent.change(box, { target: { value: "x".repeat(MAX_BODY_BYTES + 1) } });
  const save = screen.getByRole("button", { name: /^save$/i });
  expect(holdOf(save)).toBe(SHORTEN_FIRST);
  expect(screen.getByText(/Nothing is cut to fit/)).toBeTruthy();
  fireEvent.submit(box.closest("form")!);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("edit_message")).toHaveLength(0);

  // AT THE CAP, with whitespace the engine never receives, it is a rewrite.
  fireEvent.change(box, { target: { value: `${"x".repeat(MAX_BODY_BYTES)}  ` } });
  await waitFor(() => expect(holdOf(save)).toBeNull());
});

// ---------------------------------------------------------------------------
// Each change to a message, held by its own tool's rule
// ---------------------------------------------------------------------------

/** A message of the reader's own, with a reaction on it. */
function ownWithReaction(over: Record<string, unknown> = {}) {
  return {
    ...message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine", ...over }),
    reactions: [{ emoji: ":tada:", count: 2 }],
  };
}

/** The room's detail, as the engine answers it, over a channel shape. */
function detailOf(over: Record<string, unknown>, member = true) {
  return {
    ...served,
    channel: { ...channel, ...over },
    revision: 4,
    message_seq: 5,
    member,
    members: [],
  };
}

/** Whether a control is held, and with which sentence. */
function holdOf(control: HTMLElement): string | null {
  return control.getAttribute("aria-disabled") === "true" ? control.getAttribute("title") : null;
}

test("an archived room still takes a change to what was said in it", async () => {
  // AN ARCHIVE CLOSES A ROOM TO NEW MESSAGES AND TO NOTHING ELSE: the engine
  // takes an author's edit, a delete and a reaction there, so the screen holds
  // none of them — moderating a closed room is not an un-archive.
  answers.chat_channel = detailOf({ archived_at: "2026-02-01T00:00:00Z" });
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  mount();
  const chip = await screen.findByRole("button", { name: /:tada:/ });
  await waitFor(() => expect(callout(/archived/i)).not.toBeNull());
  expect((screen.getByLabelText(/^Message/) as HTMLTextAreaElement).disabled).toBe(true);
  await waitFor(() => expect(holdOf(chip)).toBeNull());
  expect(holdOf(screen.getByRole("button", { name: /^edit$/i }))).toBeNull();
  expect(holdOf(screen.getByRole("button", { name: /^delete$/i }))).toBeNull();
  fireEvent.click(chip);
  await waitFor(() => expect(pressesOf("react_to_message")).toHaveLength(1));
});

test("a private room the reader is outside holds a reaction and still offers moderation", async () => {
  // A REACTION TAKES THE ROOM'S MEMBERSHIP RULE, as a post does; a delete is
  // the author's or an operator's and reaches past membership, and an edit is
  // the author's wherever they stand.
  answers.chat_channel = detailOf({ kind: "private", name: "board" }, false);
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  mount();
  const chip = await screen.findByRole("button", { name: /:tada:/ });
  await waitFor(() => expect(holdOf(chip)).toMatch(/private room you’re not in/i));
  expect(holdOf(screen.getByRole("button", { name: /^delete$/i }))).toBeNull();
  expect(holdOf(screen.getByRole("button", { name: /^edit$/i }))).toBeNull();
  fireEvent.click(chip);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("react_to_message")).toHaveLength(0);
});

test("a room this build cannot classify draws every change to a message, held with why", async () => {
  // DRAWN AND HELD, never hidden: a control that vanished from a room reads as
  // a permission this reader lacks, and the remedy here is the node's upgrade.
  answers.chat_channel = detailOf({ kind: "broadcast" });
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  mount();
  const chip = await screen.findByRole("button", { name: /:tada:/ });
  await waitFor(() => expect(holdOf(chip)).toMatch(/newer version of the engine/));
  const edit = screen.getByRole("button", { name: /^edit$/i });
  const remove = screen.getByRole("button", { name: /^delete$/i });
  expect(holdOf(edit)).toMatch(/newer version of the engine/);
  expect(holdOf(remove)).toMatch(/newer version of the engine/);
  fireEvent.click(remove);
  fireEvent.click(chip);
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
  expect(screen.queryByRole("button", { name: /really delete/i })).toBeNull();
});

test("a room not yet read offers nothing on a guess, and draws every control held", async () => {
  // WHICH OF THE THREE REFUSALS A ROOM MAKES IS ITS OWN ANSWER, so until it
  // has come nothing is offered — and nothing is hidden either.
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      answer: (what) =>
        what === "chat_channel"
          ? new Promise(() => undefined)
          : Promise.resolve({ ...served, channel_id: "room-1", messages: [ownWithReaction()] }),
    },
  );
  const chip = await screen.findByRole("button", { name: /:tada:/ });
  const box = screen.getByLabelText(/^Message/) as HTMLTextAreaElement;
  expect(box.disabled).toBe(true);
  expect(box.placeholder).toMatch(/reading the room/i);
  expect(holdOf(chip)).toMatch(/reading the room/i);
  expect(holdOf(screen.getByRole("button", { name: /^edit$/i }))).toMatch(/reading the room/i);
  expect(holdOf(screen.getByRole("button", { name: /^delete$/i }))).toMatch(/reading the room/i);
  expect(holdOf(screen.getByRole("button", { name: /^topic$/i }))).toMatch(/reading the room/i);
});

test("a room whose read failed says so rather than reading for ever", async () => {
  // `useQuery` asks again only after `unavailable`, so any other failure held
  // every control "reading the room" for the life of the screen while the
  // header counted "0 members" nobody had stated.
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      answer: async (what) => {
        if (what === "chat_channel") throw new Error("not_found");
        return { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
      },
    },
  );
  await screen.findByText(/not found\. it may have been removed/i);
  await screen.findByText("mine");
  const box = screen.getByLabelText(/^Message/) as HTMLTextAreaElement;
  expect(box.placeholder).toMatch(/couldn’t be read/i);
  expect(box.placeholder).not.toMatch(/reading the room/i);
  expect(holdOf(screen.getByRole("button", { name: /^delete$/i }))).toMatch(/couldn’t be read/i);
  expect(screen.queryByText(/0 members/)).toBeNull();
});

test("an edit submitted twice while the first is out is one press", async () => {
  // THE BUTTON'S OWN GATE, BECAUSE A FORM SUBMITS ON ITS OWN: a second submit
  // while the first answer was out rewrote the message again under a new
  // operation key.
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  let release: () => void = () => undefined;
  reply = (tool) =>
    tool === "edit_message"
      ? (new Promise<ActReply>((resolve) => {
          release = () => resolve(applied(tool));
        }) as never)
      : applied(tool);
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  const box = screen.getByLabelText("Rewrite this message");
  fireEvent.change(box, { target: { value: "mine, fixed" } });
  await waitFor(() => expect(holdOf(screen.getByRole("button", { name: /^save$/i }))).toBeNull());
  fireEvent.submit(box.closest("form")!);
  await waitFor(() => expect(pressesOf("edit_message")).toHaveLength(1));
  fireEvent.submit(box.closest("form")!);
  fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("edit_message")).toHaveLength(1);
  act(() => release());
  await waitFor(() => expect(screen.queryByLabelText("Rewrite this message")).toBeNull());
  expect(pressesOf("edit_message")).toHaveLength(1);
});

test("a second delete while the first is out is held with why, not dropped", async () => {
  // ONE DELETE OUT AT A TIME, and every other row's Delete SAYS so: a press
  // that silently did nothing is a button somebody presses twice. Read by an
  // admin, who may take down both of `bo`'s messages.
  let release: () => void = () => undefined;
  reply = (tool) =>
    tool === "delete_message"
      ? (new Promise<ActReply>((resolve) => {
          release = () => resolve(applied(tool));
        }) as never)
      : applied(tool);
  mount(ADMIN_ADA);
  await screen.findByText("line 2");
  const rowOf = (id: string) => document.querySelector<HTMLElement>(`[data-message-id="${id}"]`)!;
  const deleteIn = (id: string) =>
    [...rowOf(id).querySelectorAll<HTMLElement>("button")].find((b) =>
      /delete/i.test(b.textContent ?? ""),
    )!;
  await waitFor(() => expect(holdOf(deleteIn("m-2"))).toBeNull());
  fireEvent.click(deleteIn("m-2"));
  fireEvent.click(deleteIn("m-2"));
  await waitFor(() => expect(pressesOf("delete_message")).toHaveLength(1));

  await waitFor(() => expect(holdOf(deleteIn("m-1"))).toBe(DELETE_IN_FLIGHT));
  fireEvent.click(deleteIn("m-1"));
  await new Promise((r) => setTimeout(r, 20));
  expect(deleteIn("m-1").textContent).toBe("Delete");
  expect(pressesOf("delete_message")).toHaveLength(1);

  act(() => release());
  await waitFor(() => expect(holdOf(deleteIn("m-1"))).toBeNull());
});

test("a second rewrite while the first is out is held with why, not dropped", async () => {
  // ONE REWRITE OUT AT A TIME, the edit twin of the delete case above: every
  // other editor's Save SAYS so, because the press is the hook's and it takes
  // one at a time — a Save that silently did nothing is pressed twice.
  let release: () => void = () => undefined;
  reply = (tool) =>
    tool === "edit_message"
      ? (new Promise<ActReply>((resolve) => {
          release = () => resolve(applied(tool));
        }) as never)
      : applied(tool);
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [ownWithReaction({ id: "m-6", body: "second" }), ownWithReaction()],
  };
  mount();
  await screen.findByText("second");
  const rowOf = (id: string) => document.querySelector<HTMLElement>(`[data-message-id="${id}"]`)!;
  const buttonIn = (id: string, name: RegExp) =>
    [...rowOf(id).querySelectorAll<HTMLElement>("button")].find((b) =>
      name.test(b.textContent ?? ""),
    )!;
  const boxIn = (id: string) =>
    rowOf(id).querySelector<HTMLTextAreaElement>('[aria-label="Rewrite this message"]')!;

  fireEvent.click(buttonIn("m-6", /^edit$/i));
  fireEvent.change(boxIn("m-6"), { target: { value: "second, fixed" } });
  fireEvent.submit(boxIn("m-6").closest("form")!);
  await waitFor(() => expect(pressesOf("edit_message")).toHaveLength(1));

  fireEvent.click(buttonIn("m-5", /^edit$/i));
  fireEvent.change(boxIn("m-5"), { target: { value: "mine, fixed" } });
  await waitFor(() => expect(holdOf(buttonIn("m-5", /^save$/i))).toBe(EDIT_IN_FLIGHT));
  fireEvent.submit(boxIn("m-5").closest("form")!);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("edit_message")).toHaveLength(1);

  act(() => release());
  await waitFor(() => expect(holdOf(buttonIn("m-5", /^save$/i))).toBeNull());
});

test("a rewrite that changes nothing is held rather than sent", async () => {
  // THE ENGINE HAS NO SAME-CONTENT NO-OP: a Save of the words as they stand
  // would write a record and draw "edited" on a message nobody changed.
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  const box = screen.getByLabelText("Rewrite this message");
  await waitFor(() =>
    expect(holdOf(screen.getByRole("button", { name: /^save$/i }))).toBe(NOTHING_CHANGED),
  );
  fireEvent.change(box, { target: { value: "  mine  " } });
  expect(holdOf(screen.getByRole("button", { name: /^save$/i }))).toBe(NOTHING_CHANGED);
  fireEvent.submit(box.closest("form")!);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("edit_message")).toHaveLength(0);
});

test("clearing the rewrite keeps the editor open", async () => {
  // AN EMPTY BOX IS AN EMPTY REWRITE, NOT A CLOSED EDITOR: selecting the
  // words to type them again used to throw the editor away under the person.
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
  fireEvent.change(screen.getByLabelText("Rewrite this message"), { target: { value: "" } });
  expect(screen.getByLabelText("Rewrite this message")).toBeTruthy();
  expect(holdOf(screen.getByRole("button", { name: /^save$/i }))).toMatch(
    /write the message first/i,
  );
});

test("a pending message's retry is held once its room stops taking messages", async () => {
  // THE ROOM'S OWN REFUSAL HOLDS A RETRY as it closes the box: a retry into
  // a room archived since the first attempt is refused however often it goes.
  reply = (tool) => unknown(tool);
  const { store } = mount();
  await screen.findByText("line 2");
  await say("did that land?");
  const retry = await screen.findByRole("button", { name: /send again/i });
  expect(holdOf(retry)).toBeNull();
  answers.chat_channel = detailOf({ archived_at: "2026-02-01T00:00:00Z" });
  act(() => frame(store, 3, { op: "patch" }));
  await waitFor(() => expect(holdOf(retry)).toMatch(/archived/i));
  fireEvent.click(retry);
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("post_message")).toHaveLength(1);
});

test("a refused delete and a refused reaction are said above the transcript", async () => {
  // A REACTION IS QUIET — no toast — so a refused one said nowhere would
  // simply vanish; the engine's sentence is drawn where the transcript is.
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  reply = (tool) =>
    tool === "delete_message"
      ? refused(tool, "invalid", "delete_message refused that: the room moved under it")
      : tool === "react_to_message"
        ? refused(tool, "invalid", "react_to_message refused that: not an emoji it knows")
        : applied(tool);
  mount();
  const chip = await screen.findByRole("button", { name: /:tada:/ });
  await waitFor(() => expect(holdOf(chip)).toBeNull());
  fireEvent.click(chip);
  await screen.findByText(/not an emoji it knows/);
  const remove = screen.getByRole("button", { name: /^delete$/i });
  fireEvent.click(remove);
  fireEvent.click(screen.getByRole("button", { name: /really delete/i }));
  await screen.findByText(/the room moved under it/);
});

test("an older page is asked at this tab's read floor", async () => {
  // EVERY ASK NAMES THE FLOOR (`withFloor`), the pager's included: a page
  // read from a node that has not applied this tab's own post would page back
  // through a room without it.
  session.written("chat", "CREWLET_CHAT_LOG@1:70", []);
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(2)],
    next_cursor: "2",
  };
  const { socket } = mount();
  await screen.findByText("line 2");
  socket.query.mockImplementation(async (what: string, params?: Record<string, unknown>) => {
    if (what === "chat_messages" && params?.cursor === "2") {
      return { ...served, channel_id: "room-1", messages: [message(1)] };
    }
    return what === "viewer" ? ADA : (answers[what as QueryName] ?? {});
  });
  fireEvent.click(screen.getByRole("button", { name: /older messages/i }));
  await screen.findByText("line 1");
  expect(socket.query).toHaveBeenCalledWith("chat_messages", {
    channel_id: "room-1",
    cursor: "2",
    limit: PAGE,
    read_level: "session",
    min_position: "CREWLET_CHAT_LOG@1:70",
  });
});

test("an older page that could not be read says why in every pager's words", async () => {
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(2)],
    next_cursor: "2",
  };
  const { socket } = mount();
  await screen.findByText("line 2");
  socket.query.mockImplementation(async (what: string, params?: Record<string, unknown>) => {
    if (what === "chat_messages" && params?.cursor === "2") throw new Error("unavailable");
    return what === "viewer" ? ADA : (answers[what as QueryName] ?? {});
  });
  expect(callout(/./)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: /older messages/i }));
  await waitFor(() => expect(callout(/./)).not.toBeNull());
  expect(screen.queryByText(/\(unavailable\)/)).toBeNull();
  expect(screen.getByText("line 2")).toBeTruthy();
});

/**
 * A room of two full pages, newest first, over a fake engine that pages it as
 * the engine does — every page `PAGE` long, the cursor the room's own number
 * of the last row — read back to its start. `room` is what the engine holds
 * and the case moves it; `cursors` is every transcript page asked, by the
 * cursor it named ("" for the newest).
 */
async function pagedToTheStart() {
  const room = {
    rows: Array.from({ length: 2 * PAGE }, (_, i) => message(2 * PAGE - i)),
    // A CURSOR WHOSE NEXT READ FAILS, once.
    failing: "",
  };
  const at = { seq: 40 };
  const pageOf = (cursor: string) => {
    const below = cursor ? room.rows.filter((row) => row.channel_seq < Number(cursor)) : room.rows;
    const slice = below.slice(0, PAGE);
    return {
      ...served,
      position: { ...served.position, seq: at.seq },
      channel_id: "room-1",
      messages: slice,
      ...(below.length > PAGE ? { next_cursor: String(slice[slice.length - 1]!.channel_seq) } : {}),
    };
  };
  const cursors = new ReadCursors({ flush: async () => undefined });
  const { store, socket } = mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      answer: async (what, params) => {
        if (what !== "chat_messages") return answers[what as QueryName] ?? {};
        const cursor = String(params?.cursor ?? "");
        if (cursor !== "" && cursor === room.failing) {
          room.failing = "";
          throw new Error("unavailable");
        }
        return pageOf(cursor);
      },
    },
  );
  await screen.findByText(`line ${2 * PAGE}`);
  fireEvent.click(screen.getByRole("button", { name: /older messages/i }));
  await screen.findByText("line 1");
  const asked = () =>
    socket.query.mock.calls
      .filter(([what]) => what === "chat_messages")
      .map(([, params]) => String((params as Record<string, unknown> | undefined)?.cursor ?? ""));
  const roomReads = () => asked().length;
  return { store, room, at, asked, roomReads };
}

test("a message taken down on a page the reader paged back to is drawn as a tombstone", async () => {
  // THE NEWEST PAGE DOES NOT REACH IT, so it is read again in the page that
  // ends at it. Kept as first read, a message somebody took down went on
  // showing its words for as long as the room was open.
  const { store, room, asked, roomReads } = await pagedToTheStart();
  const before = asked().length;
  room.rows = room.rows.map((row) =>
    row.channel_seq === 5
      ? message(5, { body: "", deleted_at: "2026-01-01T00:02:00Z", deleted_by: "bo" })
      : row,
  );
  await handled(store, roomReads, 30, { op: "delete", message_id: "m-5", channel_seq: 5 });
  await waitFor(() => expect(screen.getByText(/Deleted by bo/)).toBeTruthy());
  expect(screen.queryByText("line 5")).toBeNull();
  // THE NEWEST PAGE, AND THE ONE RANGE THAT HOLDS IT — never the room whole.
  expect(asked().slice(before)).toEqual(["", "6"]);
});

test("an erase takes its messages off a page the reader paged back to", async () => {
  // AN ERASE NAMES NO MESSAGE, so every held row below the newest page is
  // read again — which here is one range, the page below it.
  const { store, room, asked, roomReads } = await pagedToTheStart();
  const before = asked().length;
  room.rows = room.rows.filter((row) => row.channel_seq !== 3 && row.channel_seq !== 4);
  await handled(store, roomReads, 31, {
    op: "erase",
    message_id: undefined,
    channel_seq: undefined,
  });
  await waitFor(() => expect(screen.queryByText("line 3")).toBeNull());
  expect(screen.queryByText("line 4")).toBeNull();
  expect(screen.getByText("line 2")).toBeTruthy();
  expect(asked().slice(before)).toEqual(["", String(PAGE + 1)]);
});

test("a range that could not be read again is asked for with the next newest page", async () => {
  // NEVER DROPPED ON ONE FAILURE: the row it was asked for is still the one
  // a frame changed, and the next newest page is the next chance to say so.
  const { store, room, asked, roomReads } = await pagedToTheStart();
  const before = asked().length;
  room.rows = room.rows.map((row) =>
    row.channel_seq === 5
      ? message(5, { body: "", deleted_at: "2026-01-01T00:02:00Z", deleted_by: "bo" })
      : row,
  );
  room.failing = "6";
  await handled(store, roomReads, 30, { op: "delete", message_id: "m-5", channel_seq: 5 });
  await waitFor(() => expect(asked().slice(before)).toEqual(["", "6"]));
  expect(screen.getByText("line 5")).toBeTruthy();
  await handled(store, roomReads, 31, { op: "react", message_id: `m-${2 * PAGE}` });
  await waitFor(() => expect(screen.getByText(/Deleted by bo/)).toBeTruthy());
  expect(asked().slice(before)).toEqual(["", "6", "", "6"]);
});

test("a range is read again only once a newest page has caught up with the frame", async () => {
  // THE LOG DECIDES WHICH PAGE ANSWERS FOR A FRAME: a newest page derived
  // before the frame says nothing about what it moved, so the rows it does
  // not reach wait for one that is — rather than being judged against it.
  const { store, room, at, asked, roomReads } = await pagedToTheStart();
  const before = asked().length;
  room.rows = room.rows.map((row) =>
    row.channel_seq === 5
      ? message(5, { body: "", deleted_at: "2026-01-01T00:02:00Z", deleted_by: "bo" })
      : row,
  );
  // The frame sits at 50 on the log; this node has applied through 40.
  await handled(store, roomReads, 50, { op: "delete", message_id: "m-5", channel_seq: 5 });
  expect(asked().slice(before)).toEqual([""]);
  expect(screen.getByText("line 5")).toBeTruthy();
  // The next frame's newest page is derived through it.
  at.seq = 60;
  await handled(store, roomReads, 60, { op: "react", message_id: `m-${2 * PAGE}` });
  await waitFor(() => expect(screen.getByText(/Deleted by bo/)).toBeTruthy());
  expect(asked().slice(before)).toEqual(["", "", "6"]);
});

test("a live frame makes the room re-read rather than rendering the frame", async () => {
  // A frame carries no body on purpose: rendering one would be a second
  // rendering path for a conversation, and the two disagree the first time an
  // edit races a reload.
  const { store, socket } = mount();
  await screen.findByText("line 2");
  const before = socket.query.mock.calls.filter(([what]) => what === "chat_messages").length;

  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(3, { body: "arrived" }), message(2), message(1)],
  };
  frame(store, 3);

  await waitFor(() => expect(screen.getByText("arrived")).toBeTruthy());
  expect(
    socket.query.mock.calls.filter(([what]) => what === "chat_messages").length,
  ).toBeGreaterThan(before);
});

test("a burst of frames is one re-read, not one per message", async () => {
  // The whole reason the frames are coalesced: six messages in a busy room
  // would otherwise be six queries on a socket that is already carrying the
  // conversation.
  const { store, socket } = mount();
  await screen.findByText("line 2");
  const before = socket.query.mock.calls.filter(([what]) => what === "chat_messages").length;

  for (let seq = 3; seq <= 8; seq++) frame(store, seq);
  await waitFor(() =>
    expect(
      socket.query.mock.calls.filter(([what]) => what === "chat_messages").length,
    ).toBeGreaterThan(before),
  );
  expect(socket.query.mock.calls.filter(([what]) => what === "chat_messages").length).toBeLessThan(
    before + 6,
  );
});

test("a frame past the bound on what a tab holds still makes the room re-read", async () => {
  // THE TAB KEEPS A BOUNDED NUMBER OF FRAMES PER ROOM, so past the bound a new
  // frame pushes the oldest out and the count of what is held stands still.
  // A room that re-read on that count stopped at its two-hundredth frame,
  // and from then on nobody else's message reached the screen.
  const { store, socket } = mount();
  await screen.findByText("line 2");
  const reads = () => socket.query.mock.calls.filter(([what]) => what === "chat_messages").length;

  act(() => {
    for (let seq = 3; seq < 3 + MAX_CHAT_LIVE; seq++) frame(store, seq);
  });
  expect(store.state.chat.rooms["room-1"]?.changes).toHaveLength(MAX_CHAT_LIVE);
  // The burst's trailing read has run once the count is quiet.
  await waitFor(() => expect(reads()).toBeGreaterThan(1));
  await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 50));
  const before = reads();

  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(3 + MAX_CHAT_LIVE, { body: "past the bound" }), message(2), message(1)],
  };
  act(() => frame(store, 3 + MAX_CHAT_LIVE));
  await waitFor(() => expect(screen.getByText("past the bound")).toBeTruthy());
  expect(reads()).toBe(before + 1);
});

// ---------------------------------------------------------------------------
// Standing in a room you are not in
// ---------------------------------------------------------------------------

test("a room the reader may read but is not in says so, and offers to join", async () => {
  // A public room is readable by every seat, joined or not — so arriving here
  // from a search result and standing outside the room whose whole transcript
  // is on the screen is an ordinary state. Unsaid, a rail that does not list
  // the room looks like a rail that lost it.
  answers.chat_channel = {
    ...served,
    channel,
    revision: 3,
    message_seq: 2,
    member: false,
    members: [],
  };
  mount();
  await waitFor(() => expect(screen.getByText(/you’re not in this room/i)).toBeTruthy());

  const join = screen.getByRole("button", { name: /^join$/i });
  await waitFor(() => expect(join.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(join);
  await waitFor(() => expect(pressesOf("join_channel")).toHaveLength(1));
  expect(pressesOf("join_channel")[0]!.args).toEqual({ channel: "room-1" });
});

test("leaving a room takes the reader out of it rather than leaving them in front of it", async () => {
  // A private room somebody has just left is one they can no longer read, so
  // staying on it would turn into "no such room" under them a moment later.
  mount();
  await screen.findByText("line 2");
  const leave = screen.getByRole("button", { name: /^leave$/i });
  await waitFor(() => expect(leave.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(leave);

  await waitFor(() => expect(pressesOf("leave_channel")).toHaveLength(1));
  expect(pressesOf("leave_channel")[0]!.args).toEqual({ channel: "room-1" });
  await waitFor(() => expect(location.hash).toBe("#/chatlet"));
});

test("a unit's room offers no leave, and says why there is no button", async () => {
  // That membership is the org chart's own: a leave there is undone by the
  // next apply, and a gesture that silently reverts is worse than one that is
  // refused — so the engine refuses it and the screen does not offer it.
  answers.chat_channel = {
    ...served,
    channel: { ...channel, kind: "unit", unit: "engineering" },
    revision: 3,
    message_seq: 2,
    member: true,
    members: [],
  };
  mount();
  await waitFor(() => expect(screen.getByText(/org chart/i)).toBeTruthy());
  expect(screen.queryByRole("button", { name: /^leave$/i })).toBeNull();
  expect(presses).toHaveLength(0);
});

// EVERY BOX OPEN ON THE ROOM IS ONE DRAFT FOR THE PERSON'S OWN ASSISTANT. The
// page context carries one draft, the newest declared, and the room's box,
// the thread's and a rewrite's each declared their own, so the box typed in
// last was the only one the assistant could read, and a reply it drafted for
// another landed nowhere. The room declares one draft whose fields are all of
// them, each set through its own box and refused in its own words.
describe("the room's draft, for the person's own assistant", () => {
  beforeEach(() => {
    setFramePage({
      screen: "chat-room",
      title: "Chatlet › #general",
      path: ["chatlet", "room-1"],
      params: {},
      query: {},
    });
  });
  afterEach(() => {
    resetPageContextForTest();
  });

  const context = () => pageSnapshot()!.context;
  /** Set the draft's fields by path, against the page as it is now. */
  const setDraft = (values: Record<string, string>) =>
    applyPageAction({
      correlation_id: `c-${Object.keys(values).join("-")}`,
      action: "draft.set",
      payload: { values },
      source_revision: context().action_revisions?.["draft.set"] ?? context().source_revision,
    });

  test("a thread opened beside a message being written is a second field, each set through its own box", async () => {
    answers.chat_thread = { ...served, channel_id: "room-1", root: message(2), replies: [] };
    mount();
    const roomBox = (await screen.findByLabelText(/^Message/)) as HTMLTextAreaElement;
    fireEvent.change(roomBox, { target: { value: "for the room" } });
    const open = await screen.findAllByRole("button", { name: /reply in thread/i });
    fireEvent.click(open[open.length - 1]!);
    const pane = await waitFor(() => {
      const found = document.querySelector<HTMLElement>(".chat-thread");
      if (!found) throw new Error("no thread pane");
      return found;
    });
    const replyBox = within(pane).getByLabelText(/^Reply/) as HTMLTextAreaElement;
    fireEvent.change(replyBox, { target: { value: "for the thread" } });

    await waitFor(() =>
      expect(context().draft).toMatchObject({
        kind: "client",
        step: "reply",
        fields: [
          { path: "message", label: "Message to #general", value: "for the room", editable: true },
          { path: "reply", label: "Reply in the thread", value: "for the thread", editable: true },
        ],
      }),
    );

    // EACH FIELD BY ITS OWN PATH, through its own box, and the other untouched.
    act(() => {
      setDraft({ message: "a better line for the room" });
    });
    await waitFor(() => expect(roomBox.value).toBe("a better line for the room"));
    expect(replyBox.value).toBe("for the thread");
    expect(context().draft?.step).toBe("message");
    act(() => {
      setDraft({ reply: "a better reply" });
    });
    await waitFor(() => expect(replyBox.value).toBe("a better reply"));
    expect(roomBox.value).toBe("a better line for the room");

    // REFUSED IN THE BOX'S OWN WORDS: past the cap a reply is not taken. The
    // cap is bytes, so three-byte characters pass it under the length the
    // draft takes a proposal at all.
    expect(setDraft({ reply: "\u20ac".repeat(Math.ceil((MAX_BODY_BYTES + 1) / 3)) })).toMatchObject(
      {
        outcome: "invalid",
        reason: expect.stringContaining(`but a message holds at most ${MAX_BODY_BYTES}`),
      },
    );
    // AND NOTHING IS SENT: Send and Reply stay the person's.
    expect(pressesOf("post_message")).toEqual([]);
    expect(pressesOf("reply_in_thread")).toEqual([]);
  });

  test("a message open for a rewrite is a field named by the message, refused while its save is out", async () => {
    answers.chat_messages = {
      ...served,
      channel_id: "room-1",
      messages: [
        message(5, {
          id: "m-5",
          author: "ada",
          author_kind: "human",
          body: "the plan for Tuesday",
        }),
      ],
    };
    let release: () => void = () => undefined;
    reply = (tool) =>
      tool === "edit_message"
        ? (new Promise<ActReply>((resolve) => {
            release = () => resolve(applied(tool));
          }) as never)
        : applied(tool);
    mount();
    fireEvent.click(await screen.findByRole("button", { name: /^edit$/i }));
    const box = screen.getByLabelText("Rewrite this message") as HTMLTextAreaElement;

    await waitFor(() =>
      expect(context().draft).toMatchObject({
        step: "rewrite.5",
        fields: [
          { path: "message", value: "" },
          {
            path: "rewrite.5",
            label: "Rewrite of your message “the plan for Tuesday”",
            value: "the plan for Tuesday",
            editable: true,
          },
        ],
      }),
    );
    act(() => {
      setDraft({ "rewrite.5": "the plan, moved to Wednesday" });
    });
    await waitFor(() => expect(box.value).toBe("the plan, moved to Wednesday"));

    // WHILE ITS SAVE IS OUT the rewrite takes nothing, and says so.
    fireEvent.submit(box.closest("form")!);
    await waitFor(() => expect(pressesOf("edit_message")).toHaveLength(1));
    expect(setDraft({ "rewrite.5": "again" })).toMatchObject({
      outcome: "invalid",
      reason: "That rewrite is being saved.",
    });
    await act(async () => {
      release();
    });
    await waitFor(() => expect(screen.queryByLabelText("Rewrite this message")).toBeNull());
    await waitFor(() =>
      expect(context().draft?.fields.map((field) => field.path)).toEqual(["message"]),
    );
  });
});
