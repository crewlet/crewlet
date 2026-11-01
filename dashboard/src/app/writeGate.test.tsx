/**
 * EVERY CONTROL THAT CHANGES THE COMPANY IS DRAWN FOR EVERY READER, AND SAYS
 * WHY IT CANNOT ACT WHERE IT CANNOT.
 *
 * Two halves, because either alone passes over the other's failure:
 *
 *  - THE SOURCE: the one write (`protocol/act.ts`'s `act`) is called from
 *    `lib/useAct.ts` and nowhere else, and every module that makes a change
 *    through `useAct` is a module of write controls listed here with a
 *    fixture — so a new write surface cannot land without being rendered
 *    below, and a screen cannot reach `act` around the gating;
 *  - THE RENDER: every control of every such module, for an anonymous
 *    reader, a person bound to no seat, a bound person the engine makes the
 *    change for, and one it does not. The control is PRESENT in all four — a
 *    write control is never hidden — enabled for whoever the engine serves,
 *    bound to a seat or not, and otherwise disabled with the sentence that
 *    says what would change that.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { ComponentType } from "react";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import * as writes from "~/components/writes.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { SRC, type Node, isLocalSource, modules, parse } from "~/test/source.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

/**
 * Every module that may make a change, why it is one, and the suite that
 * renders its controls for every reader — `null` for the controls rendered
 * below.
 */
const WRITE_MODULES: Readonly<Record<string, { why: string; suite: string | null }>> = {
  "components/writes.tsx": {
    why: "the write controls every screen draws, each rendered below",
    suite: null,
  },
  "app/palette/Palette.tsx": {
    why:
      "the command palette's three actions — ROWS of the kit's list rather than " +
      "buttons, because focus never leaves its field — each drawn for every reader " +
      "with the reason it cannot act as its hint",
    suite: "app/palette/Palette.test.tsx",
  },
  "routes/inbox/Inbox.tsx": {
    why: "the Inbox's Mark all read, which reads through the newest notice loaded",
    suite: "routes/inbox/Inbox.test.tsx",
  },
  "routes/inbox/NoticeActions.tsx": {
    why:
      "an update's Mark as read, Mark as unread, Snooze and Unsnooze, a kit Menu where the " +
      "reader can act, and the same control disabled with its reason where they cannot",
    suite: "routes/inbox/Inbox.test.tsx",
  },
  "routes/inbox/InboxPane.tsx": {
    why:
      "a parked run's answer, made in the pane, and the read that marks an answered " +
      "question's notices (an ask is answered through components/writes.tsx)",
    suite: "routes/inbox/Inbox.test.tsx",
  },
  "components/Composer.tsx": {
    why:
      "the Inbox pane's reply and the task page's comment or ask, and the task link and " +
      "page either can attach",
    suite: "routes/inbox/Inbox.test.tsx",
  },
  "routes/worklet/item/edit.tsx": {
    why:
      "the task page's one update_work_item — status, priority, labels, schedule, fields, " +
      "title and description, a checklist tick, following it — for a writer, and the " +
      "reason said once for everybody else",
    suite: "routes/worklet/WorkItem.test.tsx",
  },
  "routes/worklet/item/Body.tsx": {
    why: "a sub-task filed under the task from its + (create_work_item with parent)",
    suite: "routes/worklet/WorkItem.test.tsx",
  },
  "routes/worklet/shapes/Board.tsx": {
    why:
      "a board card dragged, or moved with Alt and an arrow, into its new place — a " +
      "gesture with no button, so the reason it cannot act is one line above the lanes",
    suite: "routes/worklet/shapes/Board.test.tsx",
  },
  "routes/doclet/page/Editor.tsx": {
    why:
      "a page's Save (save_page against the revision it opened) and its conflict's " +
      "way on top of somebody else's save",
    suite: "routes/doclet/page/PageView.test.tsx",
  },
  "routes/doclet/page/CommentThread.tsx": {
    why: "a page's Comment and Reply (comment_on_page)",
    suite: "routes/doclet/page/PageView.test.tsx",
  },
  "routes/doclet/Composer.tsx": {
    why:
      "the composer's Publish and Save as draft, one write_page with its status, filed " +
      "in a space or under a page",
    suite: "routes/doclet/page/PageView.test.tsx",
  },
  "routes/doclet/Pages.tsx": {
    why:
      "a space's Create at the end of an open level: a folder named in place is a " +
      "write_page with an empty body, and a document opens the composer",
    suite: "routes/doclet/Pages.test.tsx",
  },
  "routes/doclet/page/Watch.tsx": {
    why: "a page's eye, a save_page carrying only the person's own watch",
    suite: "routes/doclet/page/PageView.test.tsx",
  },
  "routes/artifacts/new/fileTask.ts": {
    why:
      "a create page's ask an agent: one create_work_item filed to the chosen seat, " +
      "whose reason the page draws on its action for a reader who cannot file",
    suite: "routes/artifacts/new/fileTask.test.tsx",
  },
  "routes/worklet/NewTask.tsx": {
    why:
      "the New task sheet's Create — one create_work_item carrying the whole task — " +
      "disabled with its reason for a reader who cannot file",
    suite: "routes/worklet/NewTask.test.tsx",
  },
  "routes/worklet/shapes/cells.tsx": {
    why:
      "a list or table row's status, priority and assignee set in place — pickers " +
      "for a writer, the value and one sentence above the grid for everybody else",
    suite: "routes/worklet/shapes/cells.test.tsx",
  },
  "routes/me/Priorities.tsx": {
    why:
      "a priority row dragged, or moved with Alt and an arrow, to a new place — a " +
      "gesture with no button, so the reason it cannot act is one line above the rows; " +
      "held on somebody else's day but for a lead's own reorder",
    suite: "routes/me/MyWork.test.tsx",
  },
  "app/palette/answer.ts": {
    why:
      "the palette's answer from the company's knowledge, asked only for a reader " +
      "who may ask, and telling an anonymous or unbound one what would let them",
    suite: "app/palette/Palette.test.tsx",
  },
  "routes/chatlet/Room.tsx": {
    why:
      "a room's Join bar in place of its composer, for a room the reader may read and is " +
      "not in (join_channel), the room's own refusal said once above the transcript",
    suite: "routes/chatlet/Access.test.tsx",
  },
  "routes/chatlet/scope.tsx": {
    why:
      "a message's reactions, Edit and Delete, and the reader's Save and Mark unread " +
      "(react_to_message, edit_message, delete_message, mark_chat_read), each held by its " +
      "own tool wherever the message is drawn",
    suite: "routes/chatlet/Access.test.tsx",
  },
  "routes/chatlet/Chat.tsx": {
    why:
      "the frame's one star for a room (mark_chat_read star and unstar), pressed from the " +
      "room's More menu, held there with the browser's reason or the engine's cap",
    suite: "routes/chatlet/TopBar.test.tsx",
  },
  "routes/chatlet/RoomActions.tsx": {
    why:
      "the room's More menu on the top bar: Leave (leave_channel), each held with its " +
      "sentence beside Add people, the description, the topic and the star",
    suite: "routes/chatlet/TopBar.test.tsx",
  },
  "routes/chatlet/outbox.ts": {
    why:
      "a message said in a room or a thread, and its retry under the same operation key — " +
      "the composer that sends it is held with the browser's reason",
    suite: "routes/chatlet/Access.test.tsx",
  },
  "routes/chatlet/cursor.ts": {
    why:
      "the read cursor, flushed in the background as the reader reads — no control, so " +
      "a reader who cannot act is sent nothing at all",
    suite: "routes/chatlet/Access.test.tsx",
  },
  "routes/chatlet/Dialogs.tsx": {
    why:
      "Chatlet's dialogs: New room's Create (create_channel), Add people's Add " +
      "(add_channel_members) and an admin's auto-join switch, a topic's, a description's " +
      "and a template's Save (update_channel), and a delete's Delete (delete_message)",
    suite: "routes/chatlet/Dialogs.test.tsx",
  },
  "routes/chatlet/Details.tsx": {
    why: "a room's details pane: Leave room (leave_channel)",
    suite: "routes/chatlet/Details.test.tsx",
  },
  "routes/chatlet/notify.tsx": {
    why: "how a room reaches its reader, every message, mentions or muted (mark_chat_read)",
    suite: "routes/chatlet/Details.test.tsx",
  },
  "routes/chatlet/Thread.tsx": {
    why: "a thread pane's Follow and Following (mark_chat_read)",
    suite: "routes/chatlet/Thread.test.tsx",
  },
  "routes/chatlet/Views.tsx": {
    why:
      "the reader's views: Activity marking a row read, Threads' Unfollow and Saved's " +
      "Remove (mark_chat_read), and New message opening a conversation (open_dm)",
    suite: "routes/chatlet/Views.test.tsx",
  },
  "routes/chatlet/Browse.tsx": {
    why: "Browse rooms' Join on each room the reader may join (join_channel)",
    suite: "routes/chatlet/Browse.test.tsx",
  },
};

/** How each control of `components/writes.tsx` is mounted, and the name it is found by. */
const FIXTURES: Readonly<Record<keyof typeof writes, { props: object; name: RegExp }>> = {
  AssignButton: { props: { item: "ENG-1", version: 3, assignee: "" }, name: /^Assign$/ },
  RestoreButton: { props: { item: "ENG-1" }, name: /^Restore ENG-1$/ },
  EditProjectButton: { props: { project: "ENG", target: "2026-12-18" }, name: /^Edit project$/ },
  PinButton: { props: { view: "v1", name: "Triage", pinned: false }, name: /^Pin$/ },
  AnswerAsk: {
    props: {
      item: "LEAD-12",
      comment: "c-1",
      asker: "CEO",
      options: [{ id: "hold", label: "Hold release" }],
      recommended: "hold",
      open: false,
      onOpenChange: () => {},
      onAnswered: () => {},
    },
    name: /^Accept$/,
  },
  AnswerRunButton: {
    props: { run: { turn_id: "run-1", question: "Drop the fallback?" }, seat: "Frontend SWE" },
    name: /^Answer$/,
  },
  SaveViewButton: {
    props: { container: "project:ENG", type: "board", params: { priority: "high,urgent" } },
    name: /^View$/,
  },
  MessageSeatButton: { props: { handle: "swe" }, name: /^Message$/ },
  AssignToSeatButton: { props: { handle: "swe", name: "SWE" }, name: /^Assign task$/ },
  PauseSeatButton: {
    props: { handle: "swe", name: "SWE", paused: false, working: true },
    name: /^Pause$/,
  },
  SteerTurnButton: {
    props: { turnId: "run-1", seat: "SWE", running: true },
    name: /^Steer$/,
  },
};

/** The tool each control makes its change through, so a reader the engine does not serve it is refused. */
const TOOLS: Readonly<Record<keyof typeof writes, string>> = {
  AssignButton: "update_work_item",
  RestoreButton: "restore_work_item",
  EditProjectButton: "write_project",
  PinButton: "set_pins",
  AnswerAsk: "comment_on_work_item",
  AnswerRunButton: "answer_executor",
  SaveViewButton: "save_work_view",
  MessageSeatButton: "create_work_item",
  AssignToSeatButton: "update_work_item",
  PauseSeatButton: "pause_seat",
  SteerTurnButton: "steer_turn",
};

const EVERY_TOOL = Object.values(TOOLS);

const NOBODY: ViewerState = {
  login: "",
  grants: [],
  admin: false,
  worklet: false,
  doclet: false,
  chatlet: false,
  handle: "",
  owner: "",
  name: "",
  acts: [],
  project: "",
  kind: "",
  unbound: false,
  anonymous: true,
  loading: false,
  asking: false,
  configWriter: true,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
  chatVendors: [],
};

const READERS: readonly {
  who: string;
  viewer: (tool: string) => ViewerState;
  reason: string | null;
}[] = [
  { who: "an anonymous reader", viewer: () => NOBODY, reason: WRITE_REASONS.anonymous },
  {
    // AN UNBOUND PRINCIPAL ACTS, under its own login (ADR-0024): a binding
    // adds the seat a person acts as, never the right to act.
    who: "a person bound to no seat the engine makes it for",
    viewer: () => ({
      ...NOBODY,
      anonymous: false,
      login: "ops.lead",
      owner: "ops.lead",
      grants: ["worklet", "doclet"],
      unbound: true,
      acts: EVERY_TOOL,
    }),
    reason: null,
  },
  {
    who: "a bound person the engine makes it for",
    viewer: () => ({
      ...NOBODY,
      anonymous: false,
      login: "founder",
      grants: ["admin", "worklet", "doclet"],
      admin: true,
      worklet: true,
      doclet: true,
      chatlet: true,
      handle: "jane",
      owner: "jane",
      name: "Jane Founder",
      kind: "human",
      acts: EVERY_TOOL,
    }),
    reason: null,
  },
  {
    who: "a bound person the engine does not make it for",
    viewer: (tool) => ({
      ...NOBODY,
      anonymous: false,
      login: "founder",
      grants: ["admin", "worklet", "doclet"],
      admin: true,
      worklet: true,
      doclet: true,
      chatlet: true,
      handle: "jane",
      owner: "jane",
      name: "Jane Founder",
      kind: "human",
      acts: EVERY_TOOL.filter((t) => t !== tool),
    }),
    reason: WRITE_REASONS.not_served,
  },
];

beforeEach(() => {
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(null as never);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

/** The local modules `text` imports `name` from, by what they resolve to under `src/`. */
function importsOf(text: string, lang: "ts" | "tsx" | "dts"): Map<string, string> {
  const out = new Map<string, string>();
  for (const statement of parse(text, lang).body as Node[]) {
    if (statement.type !== "ImportDeclaration" || statement.importKind === "type") continue;
    const from = String((statement.source as Node).value);
    if (!isLocalSource(from)) continue;
    for (const spec of statement.specifiers as Node[]) {
      if (spec.type !== "ImportSpecifier" || spec.importKind === "type") continue;
      const imported = spec.imported as Node;
      out.set(String(imported.type === "Identifier" ? imported.name : imported.value), from);
    }
  }
  return out;
}

describe("the source", () => {
  const tree = modules().filter(({ path }) => !/\.test\.tsx?$/.test(path));

  test("reaches the one write from the one hook", () => {
    const callers = tree
      .filter(({ text, lang }) => importsOf(text, lang).get("act")?.endsWith("protocol/act.ts"))
      .map(({ path }) => path);
    expect(callers, "a screen makes a change through useAct, which gates it").toEqual([
      "lib/useAct.ts",
    ]);
  });

  test("makes a change only from a module of write controls this gate renders", () => {
    const writers = tree
      .filter(({ text, lang }) => importsOf(text, lang).has("useAct"))
      .map(({ path }) => path)
      .sort();
    expect(writers).toEqual(Object.keys(WRITE_MODULES).sort());
  });

  test("draws each of those controls with the gated button", () => {
    for (const [path, { suite }] of Object.entries(WRITE_MODULES)) {
      if (suite !== null) continue;
      const module = tree.find((m) => m.path === path)!;
      expect(importsOf(module.text, module.lang).has("WriteButton"), path).toBe(true);
    }
  });

  // A MODULE WHOSE CONTROLS ARE NOT BUTTONS is rendered by its own suite, and
  // that suite has to be one that draws them for the readers who cannot act:
  // naming a suite is not the same as the suite covering them.
  test("and a module drawn elsewhere names a suite that renders it for each reader", () => {
    for (const [path, { suite }] of Object.entries(WRITE_MODULES)) {
      if (suite === null) continue;
      const file = join(SRC, suite);
      expect(existsSync(file), `${path} names ${suite}, which does not exist`).toBe(true);
      const text = readFileSync(file, "utf8");
      // THE CHAT SUITES DRAW FROM ONE LIST of the readers who cannot act
      // (`test/chatAccess.ts`), so a suite that renders for `WRITE_READERS`
      // is held to that list's own readers rather than to a copy of it.
      const readers = text.includes("WRITE_READERS")
        ? readFileSync(join(SRC, "test/chatAccess.ts"), "utf8")
        : text;
      for (const block of ["anonymous", "not_served"]) {
        expect(readers, `${suite} never draws ${path} for a reader who is ${block}`).toContain(
          `WRITE_REASONS.${block}`,
        );
      }
    }
  });

  test("has a fixture for every control it exports, and no other", () => {
    expect(Object.keys(FIXTURES).sort()).toEqual(Object.keys(writes).sort());
  });
});

describe.each(Object.keys(FIXTURES) as (keyof typeof writes)[])("%s", (control) => {
  const Control = writes[control] as ComponentType<object>;
  const { props, name } = FIXTURES[control];

  test.each(READERS)("is drawn for $who, enabled only where it can act", ({ viewer, reason }) => {
    vi.mocked(useViewer).mockReturnValue(viewer(TOOLS[control]));
    render(<Control {...props} />);
    const button = screen.getByRole("button", { name });
    if (reason === null) {
      expect(button.getAttribute("aria-disabled")).not.toBe("true");
      return;
    }
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const described = (button.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");
    expect(described).toContain(reason);
  });
});
