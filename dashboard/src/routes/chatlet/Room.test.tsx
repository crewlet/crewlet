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
import { LIVE_REFETCH_MS } from "~/lib/chatLive.ts";
import { SHORTEN_FIRST } from "./Composer.tsx";
import { EDIT_IN_FLIGHT, NOTHING_CHANGED } from "./MessageRow.tsx";
import { MAX_BODY_BYTES, PENDING_REASK_MS } from "~/contract/chat.ts";
import { Store, type ChatPresencePush, type RoomMember, type QueryName } from "~/protocol/index.ts";
import { MAX_CHAT_LIVE } from "~/protocol/store.ts";
import { LoneRoom } from "./testkit.tsx";
import {
  ADA,
  actEngine,
  applied,
  mountChat,
  pending,
  refused,
  refusedForNow,
  unknown,
  type ActReply,
  type Pressed,
} from "~/test/chat.tsx";
import { LinkHostsProvider } from "~/lib/linkHosts.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
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

/**
 * A message's More menu, opened from its row: an edit and a delete live
 * there, held with their sentence where the room or the browser refuses them.
 */
function menuOf(row: HTMLElement): HTMLElement {
  fireEvent.click(within(row).getByRole("button", { name: "More actions" }));
  const menus = screen.getAllByRole("menu");
  return menus[menus.length - 1]!;
}

/** Every row's menu item of that name, each menu closed again once read. */
function itemsNamed(name: RegExp): HTMLElement[] {
  const found: HTMLElement[] = [];
  for (const row of document.querySelectorAll<HTMLElement>("article[data-message-id]")) {
    if (!within(row).queryByRole("button", { name: "More actions" })) continue;
    const menu = menuOf(row);
    const item = within(menu).queryByRole("menuitem", { name });
    if (item) found.push(item);
    fireEvent.keyDown(menu, { key: "Escape" });
  }
  return found;
}

/** The first row's menu item of that name, its menu left open to press it. */
async function findItem(name: RegExp): Promise<HTMLElement> {
  let found: HTMLElement | null = null;
  await waitFor(() => {
    for (const row of document.querySelectorAll<HTMLElement>("article[data-message-id]")) {
      if (!within(row).queryByRole("button", { name: "More actions" })) continue;
      const menu = menuOf(row);
      found = within(menu).queryByRole("menuitem", { name });
      if (found) return;
      fireEvent.keyDown(menu, { key: "Escape" });
    }
    expect(found).not.toBeNull();
  });
  return found!;
}

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
  const box = (await screen.findByLabelText("Message #general")) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: text } });
  // SEND OPENS ONCE THE VIEWER READ SAYS WHO IS WRITING: a message is a
  // change made as the person, and nobody is a person until then.
  const send = screen.getByRole("button", { name: /^send$/i });
  await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.keyDown(box, { key: "Enter" });
}

/** The room's own refusal, as the callout above the transcript says it. */
function callout(text: RegExp): HTMLElement | null {
  return (
    [...document.querySelectorAll<HTMLElement>(".crewlet-callout, .chat-join-bar")].find((el) =>
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
  expect(document.querySelector("article.is-sending, article.is-failed")).toBeTruthy();

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

  await waitFor(() =>
    expect(document.querySelector("article.is-sending, article.is-failed")).toBeNull(),
  );
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
  expect(document.querySelector("article.is-failed")).toBeTruthy();
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
  await screen.findByRole("button", { name: /send again/i });
  act(() => store.setConnected(false));
  const retry = () => screen.getByRole("button", { name: /send again/i });
  await waitFor(() => expect(retry().getAttribute("title")).toBe(WRITE_REASONS.offline));
  expect(retry().getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(retry());
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("post_message")).toHaveLength(1);
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

test("a post the room's state refuses says what refused it, never to retry", async () => {
  // AN ARCHIVED ROOM IS THE ROOM'S STATE (`409 conflict`, the one rule every
  // domain's refusals are classed by): the same post lands once somebody
  // reopens the room and not before, so the row says what refused it and who
  // changes it, in the engine's own words — the chat domain's, which the
  // engine sends as `detail` for a state it decided — and offers nothing to
  // press again. A fixed sentence for the class ended "retry".
  reply = (tool) =>
    refused(
      tool,
      "conflict",
      "This room is archived, so it takes no new messages. Only whoever created it, the lead of its team or of a team above it, or an admin can reopen it.",
      409,
    );
  mount();
  await screen.findByText("line 2");
  await say("anyone there?");
  await waitFor(() =>
    expect(screen.getByText(/This room is archived, so it takes no new messages/)).toBeTruthy(),
  );
  expect(screen.queryByText(/retry/i)).toBeNull();
  expect(screen.queryByRole("button", { name: /try again/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /send again/i })).toBeNull();
});

test("a post answered pending is asked again under its own key, and its refusal is drawn", async () => {
  // DECIDED WHERE IT IS APPLIED (ADR-0046): the node that answered had not
  // applied the post, and the room was archived just ahead of it. Asked again
  // under the SAME operation key once that node has had a read's wait, the
  // ledger answers with what its applier decided — and the row says so
  // rather than "sent" for ever, since the transcript that settles a row
  // never carries a message that is in no room.
  let asked = 0;
  reply = (tool) => {
    asked += 1;
    return asked === 1
      ? pending(tool, { message_id: "m-said" })
      : refused(tool, "conflict", "This room was archived before the message reached it.", 409);
  };
  mount();
  await screen.findByText("line 2");
  await say("anyone there?");
  await screen.findByText(/Waiting for this node to catch up/);
  await waitFor(
    () =>
      expect(screen.getByText(/This room was archived before the message reached it/)).toBeTruthy(),
    { timeout: PENDING_REASK_MS * 3 },
  );
  const sent = pressesOf("post_message");
  expect(sent).toHaveLength(2);
  expect(sent[1]!.opId).toBe(sent[0]!.opId);
  expect(sent[1]!.args).toEqual(sent[0]!.args);
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
  const box = (await screen.findByLabelText("Message #general")) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "later" } });
  const send = screen.getByRole("button", { name: /^send$/i });
  await waitFor(() => expect(send.getAttribute("title")).toBe(WRITE_REASONS.offline));
  expect(box.disabled).toBe(false);
  fireEvent.keyDown(box, { key: "Enter" });
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
  expect(box.value).toBe("later");
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
    // UNFURLED: the app's name, the reference it is, and that it opens apart.
    expect(link.textContent).toContain("GitHub");
    expect(link.textContent).toContain("gophrase/backend#14");
    expect(link.textContent).toContain("opens in a new tab");
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
});

/** Apply one frame and wait until the room has re-read for it — the same
 *  run of the live signal every view of the room answers on. */
async function handled(store: Store, roomReads: () => number, seq: number, over = {}) {
  const before = roomReads();
  act(() => frame(store, seq, over));
  await waitFor(() => expect(roomReads()).toBeGreaterThan(before));
  // THE NEXT FRAME IS NOT COALESCED INTO THIS ONE.
  await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 20));
}

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
  // THE ROOM TAKES THE BOX'S PLACE with why, and the way on.
  await waitFor(() => expect(callout(/is archived/i)).not.toBeNull());
  expect(callout(/takes no new messages/i)).not.toBeNull();
  expect(screen.queryByLabelText("Message #general")).toBeNull();
  expect(screen.getByRole("button", { name: /browse rooms/i })).toBeTruthy();
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
  // NO BOX TO TYPE IN: the bar says why in its place.
  expect(screen.queryByLabelText("Message #board")).toBeNull();
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
  expect(screen.queryByLabelText("Message #general")).toBeNull();
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
  await waitFor(() =>
    expect(document.querySelector('[data-message-id="m-2"]')?.textContent).toContain(
      "bo is drafting a reply",
    ),
  );
  // THE KIT'S WORKING LINE, as a thread's answer is drawn everywhere.
  expect(
    document.querySelector('[data-message-id="m-2"] .crewlet-thinking__status'),
  ).not.toBeNull();
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
  // A PERSON IS NOT TAGGED with the kind they signed in as; an agent is said.
  expect(within(mine).queryByText("Agent")).toBeNull();
  expect(within(mine).queryByText("human")).toBeNull();
  const seat = document.querySelector<HTMLElement>('[data-message-id="m-4"]')!;
  expect(within(seat).getByText("Agent")).toBeTruthy();

  // AND THE THREAD SAYS IT THE SAME WAY, through the same line.
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
  expect(itemsNamed(/^delete message/i)[0] ?? null).toBeNull();
  expect(itemsNamed(/^edit message/i)[0] ?? null).toBeNull();

  cleanup();
  mount(ADMIN_ADA);
  await screen.findByText("line 2");
  await waitFor(() => expect(itemsNamed(/^delete message/i).length).toBe(2));
  expect(itemsNamed(/^edit message/i)[0] ?? null).toBeNull();

  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  cleanup();
  mount();
  await screen.findByText("mine");
  await waitFor(() => expect(itemsNamed(/^edit message/i)[0]!).toBeTruthy());
  expect(itemsNamed(/^delete message/i)[0]!).toBeTruthy();
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

  fireEvent.click(await findItem(/^edit message/i));
  const box = screen.getByLabelText("Edit your message");
  fireEvent.change(box, { target: { value: "over to you @bo" } });
  fireEvent.keyDown(box, { key: "Enter" });

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

  fireEvent.click(await findItem(/^edit message/i));
  const box = screen.getByLabelText("Edit your message") as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "a very long rewrite" } });
  fireEvent.keyDown(box, { key: "Enter" });
  await screen.findByText(/longer than a message may be/);
  expect((screen.getByLabelText("Edit your message") as HTMLTextAreaElement).value).toBe(
    "a very long rewrite",
  );

  // AND TAKEN, IT CLOSES: the row is read back at the write's floor.
  reply = (tool) => applied(tool);
  fireEvent.keyDown(screen.getByLabelText("Edit your message"), { key: "Enter" });
  await waitFor(() => expect(screen.queryByLabelText("Edit your message")).toBeNull());
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
  fireEvent.click(await findItem(/^edit message/i));
  const box = screen.getByLabelText("Edit your message");
  fireEvent.change(box, { target: { value: "   " } });
  const save = screen.getByRole("button", { name: /^save$/i });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  expect(save.getAttribute("title")).toMatch(/^Write the message first/);
  fireEvent.keyDown(box, { key: "Enter" });
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
  fireEvent.click(await findItem(/^edit message/i));
  const box = screen.getByLabelText("Edit your message");
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
  fireEvent.click(await findItem(/^edit message/i));
  const box = screen.getByLabelText("Edit your message");
  fireEvent.change(box, { target: { value: "x".repeat(MAX_BODY_BYTES + 1) } });
  const save = screen.getByRole("button", { name: /^save$/i });
  expect(holdOf(save)).toBe(SHORTEN_FIRST);
  expect(screen.getByText(/Nothing is cut to fit/)).toBeTruthy();
  fireEvent.keyDown(box, { key: "Enter" });
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
  if (control.getAttribute("aria-disabled") !== "true") return null;
  const said = (control.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => (id ? document.getElementById(id)?.textContent : ""))
    .join(" ")
    .trim();
  return control.getAttribute("title") ?? (said || control.textContent);
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
  expect(screen.queryByLabelText("Message #general")).toBeNull();
  await waitFor(() => expect(holdOf(chip)).toBeNull());
  expect(holdOf(itemsNamed(/^edit message/i)[0]!)).toBeNull();
  expect(holdOf(itemsNamed(/^delete message/i)[0]!)).toBeNull();
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
  await waitFor(() => expect(chip.getAttribute("aria-disabled")).toBe("true"));
  expect(holdOf(itemsNamed(/^delete message/i)[0]!)).toBeNull();
  expect(holdOf(itemsNamed(/^edit message/i)[0]!)).toBeNull();
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
  await waitFor(() => expect(chip.getAttribute("aria-disabled")).toBe("true"));
  const edit = itemsNamed(/^edit message/i)[0]!;
  const remove = itemsNamed(/^delete message/i)[0]!;
  expect(holdOf(edit)).toMatch(/newer version of the engine/);
  expect(holdOf(remove)).toMatch(/newer version of the engine/);
  fireEvent.click(chip);
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
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
  // THE BAR SAYS WHAT HOLDS THE BOX: the room's own answer, not here yet.
  expect(callout(/reading the room/i)).not.toBeNull();
  expect(chip.getAttribute("aria-disabled")).toBe("true");
  expect(holdOf(itemsNamed(/^edit message/i)[0]!)).toMatch(/reading the room/i);
  expect(holdOf(itemsNamed(/^delete message/i)[0]!)).toMatch(/reading the room/i);
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
  expect(callout(/couldn’t be read/i)).not.toBeNull();
  expect(callout(/reading the room/i)).toBeNull();
  expect(holdOf(itemsNamed(/^delete message/i)[0]!)).toMatch(/couldn’t be read/i);
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
  fireEvent.click(await findItem(/^edit message/i));
  const box = screen.getByLabelText("Edit your message");
  fireEvent.change(box, { target: { value: "mine, fixed" } });
  await waitFor(() => expect(holdOf(screen.getByRole("button", { name: /^save$/i }))).toBeNull());
  fireEvent.keyDown(box, { key: "Enter" });
  await waitFor(() => expect(pressesOf("edit_message")).toHaveLength(1));
  fireEvent.keyDown(box, { key: "Enter" });
  fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("edit_message")).toHaveLength(1);
  act(() => release());
  await waitFor(() => expect(screen.queryByLabelText("Edit your message")).toBeNull());
  expect(pressesOf("edit_message")).toHaveLength(1);
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
    messages: [
      { ...ownWithReaction({ id: "m-6", body: "second" }), channel_seq: 6 },
      ownWithReaction(),
    ],
  };
  mount();
  await screen.findByText("second");
  const rowOf = (id: string) => document.querySelector<HTMLElement>(`[data-message-id="${id}"]`)!;
  const buttonIn = (id: string, name: RegExp) =>
    [...rowOf(id).querySelectorAll<HTMLElement>("button")].find((b) =>
      name.test(b.textContent ?? ""),
    )!;
  const boxIn = (id: string) =>
    rowOf(id).querySelector<HTMLTextAreaElement>('[aria-label="Edit your message"]')!;

  const editIn = (id: string) =>
    fireEvent.click(within(menuOf(rowOf(id))).getByRole("menuitem", { name: /^edit message/i }));
  editIn("m-6");
  await waitFor(() => expect(boxIn("m-6")).not.toBeNull());
  fireEvent.change(boxIn("m-6"), { target: { value: "second, fixed" } });
  fireEvent.keyDown(boxIn("m-6"), { key: "Enter" });
  await waitFor(() => expect(pressesOf("edit_message")).toHaveLength(1));

  editIn("m-5");
  await waitFor(() => expect(boxIn("m-5")).not.toBeNull());
  fireEvent.change(boxIn("m-5"), { target: { value: "mine, fixed" } });
  await waitFor(() => expect(holdOf(buttonIn("m-5", /^save$/i))).toBe(EDIT_IN_FLIGHT));
  fireEvent.keyDown(boxIn("m-5"), { key: "Enter" });
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
  fireEvent.click(await findItem(/^edit message/i));
  const box = screen.getByLabelText("Edit your message");
  await waitFor(() =>
    expect(holdOf(screen.getByRole("button", { name: /^save$/i }))).toBe(NOTHING_CHANGED),
  );
  fireEvent.change(box, { target: { value: "  mine  " } });
  expect(holdOf(screen.getByRole("button", { name: /^save$/i }))).toBe(NOTHING_CHANGED);
  fireEvent.keyDown(box, { key: "Enter" });
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("edit_message")).toHaveLength(0);
});

test("clearing the rewrite keeps the editor open", async () => {
  // AN EMPTY BOX IS AN EMPTY REWRITE, NOT A CLOSED EDITOR: selecting the
  // words to type them again used to throw the editor away under the person.
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  mount();
  fireEvent.click(await findItem(/^edit message/i));
  fireEvent.change(screen.getByLabelText("Edit your message"), { target: { value: "" } });
  expect(screen.getByLabelText("Edit your message")).toBeTruthy();
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
  const held = () => screen.getByRole("button", { name: /send again/i });
  await waitFor(() => expect(holdOf(held())).toMatch(/archived/i));
  fireEvent.click(held());
  await new Promise((r) => setTimeout(r, 20));
  expect(pressesOf("post_message")).toHaveLength(1);
});

test("a refused reaction is said above the transcript", async () => {
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
});

test("a delete opens the screen's delete dialog, which asks before it writes", async () => {
  // THE DIALOG IS THE PANE HOST'S (`nav.openDialog`): a tombstone is not
  // undone, so the row only asks for it and writes nothing itself.
  answers.chat_messages = { ...served, channel_id: "room-1", messages: [ownWithReaction()] };
  mount();
  fireEvent.click(await findItem(/^delete message/i));
  await waitFor(() => expect(document.querySelector('[data-dialog="delete"]')).not.toBeNull());
  expect(pressesOf("delete_message")).toHaveLength(0);
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
  await waitFor(() => expect(screen.getByText(/This message was deleted/)).toBeTruthy());
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
  await waitFor(() => expect(screen.getByText(/This message was deleted/)).toBeTruthy());
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
  await waitFor(() => expect(screen.getByText(/This message was deleted/)).toBeTruthy());
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
  // THE BAR SAYS WHERE THE READER STANDS, in the box's place.
  await waitFor(() => expect(callout(/join to post and react/i)).not.toBeNull());
  expect(screen.queryByLabelText("Message #general")).toBeNull();

  const join = screen.getByRole("button", { name: /^join room$/i });
  await waitFor(() => expect(join.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(join);
  await waitFor(() => expect(pressesOf("join_channel")).toHaveLength(1));
  expect(pressesOf("join_channel")[0]!.args).toEqual({ channel: "room-1" });
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
    fireEvent.click(await findItem(/^edit message/i));
    const box = screen.getByLabelText("Edit your message") as HTMLTextAreaElement;

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
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(pressesOf("edit_message")).toHaveLength(1));
    expect(setDraft({ "rewrite.5": "again" })).toMatchObject({
      outcome: "invalid",
      reason: "That rewrite is being saved.",
    });
    await act(async () => {
      release();
    });
    await waitFor(() => expect(screen.queryByLabelText("Edit your message")).toBeNull());
    await waitFor(() =>
      expect(context().draft?.fields.map((field) => field.path)).toEqual(["message"]),
    );
  });
});
