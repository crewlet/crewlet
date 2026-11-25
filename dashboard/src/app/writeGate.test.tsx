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

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { ComponentType } from "react";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import * as writes from "~/components/writes.tsx";
import * as seatControls from "~/components/seatControls.tsx";
import * as foldIn from "~/components/FoldInButtons.tsx";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { Router } from "~/app/router.tsx";
import type { ActionTool } from "~/protocol/act.ts";
import type { OrgServices } from "~/contract/services.ts";
import type { SeatLive } from "~/lib/seatLive.ts";
import { useAct } from "~/lib/useAct.ts";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { ANSWER_SEAT_HELD, PROJECT_EDIT_HELD, WRITE_REASONS } from "~/lib/useWriteAccess.ts";
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
  "components/seatControls.tsx": {
    why:
      "a seat's Pause, Pause and stop, Resume and Steer, for one seat or a selection, the " +
      "dialog each opens, and the remedy a seat's state asks for, each rendered below",
    suite: null,
  },
  "app/palette/Palette.tsx": {
    why:
      "the command palette's three actions — ROWS of the kit's list rather than " +
      "buttons, because focus never leaves its field — each drawn for every reader " +
      "with the reason it cannot act as its hint",
    suite: "app/palette/Palette.test.tsx",
  },
  "routes/home/board/kept.ts": {
    why: "Home's Done and a group folded outside editing save the reader's board; a press that cannot save says why",
    suite: "routes/home/Board.test.tsx",
  },
  "routes/settings/AskCrewlet.tsx": {
    why: "Settings › Ask Crewlet's Save on the reader's own instructions; a Save that cannot send says why",
    suite: "routes/settings/AskCrewlet.test.tsx",
  },
  "routes/atlas/AddEntry.tsx": {
    why: "Company › Atlas's Add entry, and the dialog every atlas write is made in",
    suite: "routes/atlas/writes.test.tsx",
  },
  "routes/atlas/AdvisorFix.tsx": {
    why: "an atlas finding's fix in Settings › Advisor: an owner, a monitor or a read asked for, one press per entry",
    suite: "routes/atlas/AdvisorFix.test.tsx",
  },
  "routes/atlas/Decisions.tsx": {
    why: "the decisions a disagreement or a waiting entry allows, on an entry's Sources tab and in Review",
    suite: "routes/atlas/writes.test.tsx",
  },
  "routes/atlas/EntryActions.tsx": {
    why: "an entry's Confirm, Correct and More menu, and the dialog each opens",
    suite: "routes/atlas/writes.test.tsx",
  },
  "routes/atlas/LinkEntry.tsx": {
    why:
      "the task rail's Link an atlas entry dialog (update_work_item's linked_entries), opened " +
      "only by a reader who may change the task and never offered to anybody else",
    suite: "routes/worklet/WorkItem.test.tsx",
  },
  "routes/atlas/Sources.tsx": {
    why: "Refresh now on a source's card, held inside the engine's floor and for a reader who may not ask",
    suite: "routes/atlas/Sources.test.tsx",
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
  "components/FoldInButtons.tsx": {
    why:
      "a fold-in's two answers, whether another person's request joins a running executor's " +
      "work, drawn in a decision row and the Inbox's pane, each rendered below",
    suite: null,
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
  "routes/worklet/ProjectSettings.tsx": {
    why:
      "a project's Add label, Rename and Archive on each label, and Add field, Edit and " +
      "Archive on each of its own fields (write_project's tag facets and its fields), on " +
      "About, each held with the sentence that says why where the reader cannot act",
    suite: "routes/worklet/ProjectSettings.test.tsx",
  },
  "routes/worklet/taskMenu.tsx": {
    why:
      "a task's Move to project, Move to trash, Restore and Delete permanently — menu entries on its page, " +
      "its peek and each row of every shape, and a selection's bar — each held with the " +
      "sentence that says why where the reader cannot act",
    suite: "routes/worklet/taskMenu.test.tsx",
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
  "routes/doclet/page/PageView.tsx": {
    why:
      "a draft's Publish and a published page's Move back to draft, a save_page carrying " +
      "only the status, from the draft's notice and from More",
    suite: "routes/doclet/page/PageView.test.tsx",
  },
  "routes/doclet/page/Tidy.tsx": {
    why:
      "a page's Labels and Move (save_page with its labels or its parent), beside the " +
      "rename, trash, restore and purge its own routes take",
    suite: "routes/doclet/page/PageView.test.tsx",
  },
  "routes/agents/ScheduleControls.tsx": {
    why:
      "a schedule's Run now (run_schedule), on its page and its peek, held with the " +
      "schedule's own reason where it is paused or nobody can run it",
    suite: "routes/agents/Schedules.test.tsx",
  },
  "routes/artifacts/new/fileTask.ts": {
    why:
      "a create page's ask an agent: one create_work_item filed to the chosen seat, " +
      "whose reason the page draws on its action for a reader who cannot file",
    suite: "routes/artifacts/new/fileTask.test.tsx",
  },
  "routes/gitlet/change/ChangePage.tsx": {
    why:
      "a change's page bar — Approve, Request changes, Comment, Request review, " +
      "Close and Reopen — and the dialogs each opens",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/ChangeEdits.tsx": {
    why:
      "a live change's Convert to draft, retarget and work item (code_update_change), and a " +
      "finished change's Restore branch (code_create_branch), from its rail",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/StackNotes.tsx": {
    why: "a comment on lines picked on Files changed (code_comment_on_change with its anchor)",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/AskDialog.tsx": {
    why: "asking reviewers from the page bar and the rail (code_request_review)",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/Rail.tsx": {
    why: "a change rail's Request and re-request (code_request_review)",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/ReviewDialog.tsx": {
    why: "the review sent with its drafts (code_review_change), from the page bar and Finish review",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/CommentCard.tsx": {
    why: "a comment's Edit (code_comment_on_change) and Delete (code_delete_comment), from its menu",
    suite: "routes/gitlet/change/CommentCard.test.tsx",
  },
  "routes/gitlet/change/Conversation.tsx": {
    why: "a change's comment, a reply to a thread or a finding, and a thread's Resolve",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/Description.tsx": {
    why: "a change's Edit of its title and description (code_update_change)",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/Finding.tsx": {
    why: "a finding's Apply suggestion and Resolve, on the conversation and on Files changed",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/Merge.tsx": {
    why:
      "a change's merge box: Merge or Add to queue, Override and its dialog, Re-run " +
      "failed checks and Re-request, each a WriteButton held with the gate's sentence",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/checks/Rerun.tsx": {
    why: "one check run again (code_rerun_checks)",
    suite: "routes/gitlet/Checks.test.tsx",
  },
  "routes/gitlet/ImportRepo.tsx": {
    why:
      "the repositories' Import a repository — one code_import_repo, a new repository " +
      "filled from another host or made its pull mirror — and the dialog it opens",
    suite: "routes/gitlet/Repositories.test.tsx",
  },
  "routes/gitlet/repo/RepoWrites.tsx": {
    why:
      "a repository's settings writes: its details, its name, its archive, its search " +
      "mode, its pull and push mirrors (code_update_repo) and Purge unreachable history " +
      "(code_purge_unreachable), each a WriteButton held with the gate's sentence",
    suite: "routes/gitlet/repo/RepositoryPage.test.tsx",
  },
  "routes/gitlet/repo/PathOwners.tsx": {
    why:
      "a repository's path owners: Add a path owner, and an owner's Edit and Remove " +
      "(code_update_repo's path_owners), each a WriteButton held with the gate's sentence",
    suite: "routes/gitlet/repo/RepositoryPage.test.tsx",
  },
  "routes/gitlet/NewRepo.tsx": {
    why: "the repositories' New repository, one code_create_repo, and the dialog it opens",
    suite: "routes/gitlet/Repositories.test.tsx",
  },
  "routes/gitlet/repo/RepoLife.tsx": {
    why:
      "a repository's Delete repository (code_delete_repo, held until its name is " +
      "typed) and a deleted repository's Restore (code_restore_repo)",
    suite: "routes/gitlet/repo/RepositoryPage.test.tsx",
  },
  "routes/gitlet/repo/Protect.tsx": {
    why: "a repository's branch rules: Protect a branch, and a rule's Edit and Remove (code_protect_branch)",
    suite: "routes/gitlet/repo/RepositoryPage.test.tsx",
  },
  "routes/gitlet/repo/BranchWrites.tsx": {
    why:
      "Branches' New branch and a row's Delete (code_create_branch, code_delete_branch), " +
      "held with the engine's sentence where its rules refuse a delete",
    suite: "routes/gitlet/repo/RepositoryPage.test.tsx",
  },
  "routes/gitlet/repo/TagWrites.tsx": {
    why:
      "the Tags view's New tag, a tag's Delete (held with the engine's sentence where a rule " +
      "keeps the tag) and a commit page's Tag (code_create_tag, code_delete_tag)",
    suite: "routes/gitlet/repo/RepositoryPage.test.tsx",
  },
  "routes/gitlet/repo/FileWrites.tsx": {
    why:
      "a branch's files: Edit (a pencil), New file (a plus in a file's tree), Delete file " +
      "(a More menu's entry, held with its reason), the README's Edit and an empty " +
      "repository's Add a file, each one code_commit_files",
    suite: "routes/gitlet/repo/RepositoryPage.test.tsx",
  },
  "routes/gitlet/commit/CommitWrites.tsx": {
    why: "a commit reverted or picked from the page bar (code_revert, code_cherry_pick)",
    suite: "routes/gitlet/commit/CommitPage.test.tsx",
  },
  "routes/gitlet/change/ChangeWrites.tsx": {
    why:
      "a change's Rebase, Take out of the queue and a merged change's Revert " +
      "(code_rebase_change, code_dequeue_change, code_revert_change)",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/queue/Requeue.tsx": {
    why: "a change taken out of its queue, added again (code_merge_change)",
    suite: "routes/gitlet/Queue.test.tsx",
  },
  "routes/gitlet/change/FindingAnswer.tsx": {
    why: "a standing finding's Answer finding (code_resolve_finding)",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/change/Viewed.tsx": {
    why: "a change's file marked viewed (code_mark_viewed)",
    suite: "routes/gitlet/change/ChangePage.test.tsx",
  },
  "routes/gitlet/commit/ComparePage.tsx": {
    why:
      "a comparison's Open change — one code_open_change of the branch compared into " +
      "the base branch — and the dialog it opens",
    suite: "routes/gitlet/commit/CommitPage.test.tsx",
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
      "a room's Join bar above its composer, for a room the reader may read and is not " +
      "in (join_channel), or the star a team's room offers in its place, and in the " +
      "composer's place where the engine takes no post, the room's own refusal said once " +
      "above the transcript",
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
      "sentence beside Add people, the description, the topic, the star and Archive room",
    suite: "routes/chatlet/TopBar.test.tsx",
  },
  "routes/chatlet/RoomAdmin.tsx": {
    why:
      "a room's Archive and Reopen (update_channel archived), asked in a dialog and held " +
      "with the engine's sentence for anybody but its creator, its team's lead or an admin",
    suite: "routes/chatlet/Details.test.tsx",
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
    why:
      "a room's details pane: Leave room (leave_channel), and each agent member's " +
      "gets-every-message switch (set_channel_members), held with the room's sentence",
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

/** How a control is mounted, the name it is found by, and the tool it makes its change through. */
interface Fixture {
  props: object;
  name: RegExp;
  tool: ActionTool;
  /** A dialog its opener keeps open is handed the opener's write (`write=`), as its opener hands it. */
  handed?: true;
  /** What the reader writes first, for a control held until there is something to send. */
  fill?: () => void;
  /**
   * Its sentence for a reader the engine does not serve, where the control
   * names the rule that admits a reader rather than saying the frame's.
   */
  notServed?: string;
  /**
   * Whether it also holds, in its `notServed` words, a reader the seat's own
   * rule does not admit: one neither an admin nor a lead in the seat's line,
   * whom the engine refuses on this seat however many changes it makes for them.
   */
  seatRule?: true;
  /**
   * The sentence it holds with for a reader every change is made for, where
   * the object's own rule admits only its lead or an admin and none of these
   * readers leads it: a project's Edit project.
   */
  leadRule?: string;
  /** What the company runs, where the control's change turns on it; every service otherwise. */
  services?: OrgServices;
}

/** A running agent seat the reader leads, as a screen hands it to a seat's controls. */
const SWE = {
  seat: { handle: "swe", name: "SWE", kind: "agent" },
  state: "working",
  paused: false,
  controllable: true,
  ask: null,
} as unknown as SeatLive;

/** The controls' dialogs, held by a screen the fixture does not draw. */
const CONTROLS: seatControls.SeatControls = {
  open: () => {},
  steer: () => {},
  present: () => {},
  dialog: null,
};

/** How each control of `components/writes.tsx` is mounted. */
const WRITES: Readonly<Record<keyof typeof writes, Fixture>> = {
  AssignButton: {
    props: { item: "ENG-1", version: 3, assignee: "" },
    name: /^Assign$/,
    tool: "update_work_item",
  },
  RestoreButton: { props: { item: "ENG-1" }, name: /^Restore ENG-1$/, tool: "restore_work_item" },
  MoveWorkConfirm: {
    props: { label: "Confirm", onConfirm: () => {} },
    name: /^Confirm$/,
    tool: "update_work_item",
  },
  EditProjectButton: {
    props: { project: "ENG", target: "2026-12-18", lead: "eng-lead" },
    name: /^Edit project$/,
    tool: "write_project",
    leadRule: PROJECT_EDIT_HELD,
  },
  PinButton: {
    props: { view: "v1", name: "Triage", pinned: false },
    name: /^Pin$/,
    tool: "set_pins",
  },
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
    tool: "comment_on_work_item",
  },
  AnswerRunButton: {
    props: {
      run: { agent_handle: "swe", turn_id: "run-1", question: "Drop the fallback?" },
      seat: "Frontend SWE",
    },
    name: /^Answer$/,
    tool: "answer_executor",
    notServed: ANSWER_SEAT_HELD,
    seatRule: true,
  },
  EditViewButton: {
    props: {
      view: {
        id: "v1",
        name: "Triage",
        container: "project:ENG",
        type: "board",
        params: { priority: "high,urgent" },
        personal: false,
      },
    },
    name: /^Edit view$/,
    tool: "save_work_view",
  },
  SaveViewButton: {
    props: { container: "project:ENG", type: "board", params: { priority: "high,urgent" } },
    name: /^View$/,
    tool: "save_work_view",
  },
  // THE ASK, on a company whose chat is a vendor's: where the company runs
  // Chatlet, Message is a direct conversation for a reader who holds it
  // (`open_dm`), which `routes/agents/AskTask.test.tsx` holds.
  MessageSeatButton: {
    props: { handle: "swe" },
    name: /^Message$/,
    tool: "create_work_item",
    services: { tracker: "worklet", knowledge: "doclet", chat: "vendor", code: "gitlet" },
  },
  AssignToSeatButton: {
    props: { handle: "swe", name: "SWE" },
    name: /^Assign task$/,
    tool: "update_work_item",
  },
  SteerTurnButton: {
    props: { turnId: "run-1", seat: "SWE", running: true },
    name: /^Steer$/,
    tool: "steer_turn",
  },
  DeleteViewButton: {
    props: { view: { id: "v1", name: "Triage", personal: false }, onDeleted: () => {} },
    name: /^Delete view$/,
    tool: "delete_work_view",
  },
  SteerTurnDialog: {
    props: { turnId: "run-1", seat: "SWE", onClose: () => {} },
    name: /^Send note$/,
    tool: "steer_turn",
    handed: true,
    fill: () =>
      fireEvent.change(screen.getByLabelText("Note"), { target: { value: "Ship it first." } }),
  },
};

/** How each control of `components/seatControls.tsx` is mounted. */
const SEAT_CONTROLS: Readonly<Record<string, Fixture>> = {
  SeatControlButtons: {
    props: { s: SWE, controls: CONTROLS },
    name: /^Pause SWE$/,
    tool: "pause_seat",
    notServed: "Pausing a seat needs Admin access, or leading it.",
  },
  SeatSelectionBar: {
    props: { selected: [SWE], onClear: () => {}, controls: CONTROLS },
    name: /^Pause 1$/,
    tool: "pause_seat",
    notServed: "Pausing a seat needs Admin access, or leading it.",
  },
  SeatControlDialog: {
    props: { kind: "pause", seats: [SWE], onClose: () => {} },
    name: /^Pause$/,
    tool: "pause_seat",
  },
  SeatRemedy: {
    props: {
      s: {
        ...SWE,
        ask: { agent_handle: "swe", turn_id: "run-1", question: "Drop the fallback?" },
      },
    },
    name: /^Answer$/,
    tool: "answer_executor",
    notServed: ANSWER_SEAT_HELD,
    seatRule: true,
  },
};

/**
 * A fold-in put to the reader. Who may answer is the engine's (the run's
 * requester, a lead of its seat, an administrator; never the asker), so the
 * buttons hold only as the tool does and draw the engine's refusal under them.
 */
const FOLD_IN: Readonly<Record<string, Fixture>> = {
  FoldInButtons: {
    props: {
      item: {
        id: "f-1",
        turn_id: "run-1",
        launch_id: "job-1",
        seat: "swe",
        requester: "jane",
        asker: "ceo",
        asker_kind: "human",
        conversation: "chat:C1",
        words: "also redo the logo",
        cut: 0,
        put_to: ["jane"],
        asked_at: "2026-11-14T12:00:00Z",
        decision: {
          question: "ceo asked swe for work of their own",
          options: [],
          role: "approver",
        },
      },
      seat: "SWE",
    },
    name: /^Add it to the running work$/,
    tool: "answer_executor",
  },
};

/** The modules whose controls are rendered below, each with a fixture for every control it exports. */
const RENDERED: readonly {
  path: string;
  module: object;
  fixtures: Readonly<Record<string, Fixture>>;
}[] = [
  { path: "components/writes.tsx", module: writes, fixtures: WRITES },
  { path: "components/seatControls.tsx", module: seatControls, fixtures: SEAT_CONTROLS },
  { path: "components/FoldInButtons.tsx", module: foldIn, fixtures: FOLD_IN },
];

/** The controls a module exports: its components, by their capitalised names. */
function controlsOf(module: object): string[] {
  return Object.entries(module)
    .filter(([key, value]) => typeof value === "function" && /^[A-Z]/.test(key))
    .map(([key]) => key)
    .sort();
}

const EVERY_TOOL = [
  ...new Set(RENDERED.flatMap(({ fixtures }) => Object.values(fixtures).map((f) => f.tool))),
];

const NOBODY: ViewerState = {
  login: "",
  grants: [],
  admin: false,
  worklet: false,
  doclet: false,
  chatlet: false,
  gitlet: false,
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
  failed: null,
  failedRefusal: null,
  configWriter: true,
  configManagedBy: [],
  assistant: "no_model",
  assistantModel: null,
  assistantReadsImages: false,
  assistantCanAssign: false,
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
      gitlet: true,
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
      gitlet: true,
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

  // EVERY MODULE RENDERED BELOW IS ONE THIS GATE LISTS AS RENDERED BELOW, and
  // the other way round: a module listed with no fixtures is never drawn.
  test("has a fixture for every control each module rendered below exports, and no other", () => {
    expect(RENDERED.map((r) => r.path).sort()).toEqual(
      Object.entries(WRITE_MODULES)
        .filter(([, { suite }]) => suite === null)
        .map(([path]) => path)
        .sort(),
    );
    for (const { path, module, fixtures } of RENDERED) {
      expect(Object.keys(fixtures).sort(), path).toEqual(controlsOf(module));
    }
  });
});

/** A dialog mounted with the write its opener holds, as its opener mounts it. */
function Handed({
  Control,
  tool,
  ...props
}: {
  Control: ComponentType<Record<string, unknown>>;
  tool: ActionTool;
}) {
  const write = useAct(tool);
  return <Control {...props} write={write} />;
}

describe.each(
  RENDERED.flatMap(({ module, fixtures }) =>
    Object.entries(fixtures).map(([control, fixture]) => ({
      control,
      Control: (module as Record<string, ComponentType<Record<string, unknown>>>)[control]!,
      fixture,
    })),
  ),
)("$control", ({ Control, fixture }) => {
  const { props, name, tool, handed, fill, notServed, seatRule, leadRule, services } = fixture;

  test.each(READERS)("is drawn for $who, enabled only where it can act", (reader) => {
    const viewer = reader.viewer(tool);
    // THE SEAT'S RULE ADMITS AN ADMIN OR A LEAD, and none of these readers leads the seat.
    // A PROJECT'S RULE ADMITS ITS LEAD OR AN ADMIN, and none of these readers leads it.
    const reason =
      reader.reason === null && seatRule && !viewer.admin
        ? notServed!
        : reader.reason === null && leadRule && !viewer.admin
          ? leadRule
          : reader.reason;
    vi.mocked(useViewer).mockReturnValue(viewer);
    if (services) vi.mocked(useOrg).mockReturnValue({ name: "Acme", services } as never);
    // THE FRAME'S ROUTER AND LIVE REGION, as every screen has them: a remedy
    // opens the seat panel, and a selection bar says its count aloud.
    render(
      <Router>
        <AppAnnouncer />
        {handed ? <Handed Control={Control} tool={tool} {...props} /> : <Control {...props} />}
      </Router>,
    );
    fill?.();
    const button = screen.getByRole("button", { name });
    if (reason === null) {
      expect(button.getAttribute("aria-disabled")).not.toBe("true");
      return;
    }
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const said = reason === WRITE_REASONS.not_served ? (notServed ?? reason) : reason;
    const described = (button.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");
    expect(described).toContain(said);
  });
});
