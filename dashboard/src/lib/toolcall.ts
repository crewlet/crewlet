/**
 * The read-only product's answer to an edit button.
 *
 * This dashboard does not write to the company. That is deliberate — every
 * change is attributed to whoever made it, and a browser form posting as "the
 * dashboard" would be the one actor an audit trail cannot name. So when a
 * reader wants to change what they are looking at, the honest answer is to
 * show them the call their own assistant would make, pre-filled with this
 * object's ids, ready to copy.
 *
 * # Why a table here rather than a schema walk
 *
 * The operator surface publishes each tool's JSON Schema, and a block could be
 * generated from it. What a schema cannot say is WHICH id goes in WHICH field:
 * `update_work_item` takes `item`, `save_page` takes `page`, `set_priorities`
 * takes `handle` — three names for "the thing on screen", and nothing in the
 * schema marks one. That mapping is a decision, so it is written down.
 *
 * # What stops it lying
 *
 * A table of tool names and field names written in TypeScript is a
 * documentation surface that starts lying on the first rename in Go. So it is
 * GATED: `TestEveryToolCallTheScreenOffersIsOneAnOperatorHas` builds the real
 * operator catalogue and checks every tool this file names, every argument it
 * fills, and that none of them is a read — `my_work` and `remove_work_item`
 * are both operator tools and only one is an answer to "how do I change this".
 * It is the `rooms` idiom, which this repository already uses for exactly this
 * class of cross-language claim.
 */

/** One call a reader can copy. */
export interface ToolCall {
  /** The operator tool's name, exactly as the surface serves it. */
  tool: string;
  /** The arguments, with this object's ids already in them. */
  args: Record<string, unknown>;
  /** What it does, in the imperative, for the row's own label. */
  label: string;
  /** Irreversible. The row says so and is not the first one offered. */
  destructive?: boolean;
}

/** What a screen knows about the object it is showing. */
export interface Subject {
  kind: "item" | "page" | "seat" | "project" | "goal" | "notice";
  /** The identifier the tools take — a key, a uuid, a handle. */
  id: string;
  /** Whose object it is, where a screen has it for a label. */
  handle?: string;
  /**
   * The version or log position a call has to state, where the object has
   * one — a page's `base_version`, a notice's own position. Both are the same
   * shape of fact: "the state I am acting on", which the engine compares
   * against what it holds.
   */
  version?: number;
  /**
   * The object is in the trash.
   *
   * It changes which call is offered rather than whether one is: "take it off
   * the board" for something already off the board is the one call in this
   * table that cannot do anything, and it sat where the restore belongs.
   */
  removed?: boolean;
}

/**
 * The calls offered for one object.
 *
 * ORDERED BY WHAT A READER MOST LIKELY WANTS, with anything destructive last
 * and marked. A block that opened on "remove this" would be a delete button
 * wearing a disclosure.
 *
 * A PLACEHOLDER IS PROSE, not an empty string: `"body": "…"` copied into an
 * assistant is a prompt to write something, where `"body": ""` is a call that
 * would land an empty comment if somebody sent it unedited.
 */
export function callsFor(subject: Subject): ToolCall[] {
  const { kind, id } = subject;
  if (!id) return [];
  switch (kind) {
    case "item":
      // A TASK IN THE TRASH IS OFFERED THE WAY BACK, not the way out. The
      // removal is reversible at any age and there is no window, so the
      // restore is the ordinary call for this state and nothing here is
      // destructive: it is the live task's removal that is.
      if (subject.removed) {
        return [
          {
            tool: "restore_work_item",
            label: "Bring it back from the trash",
            args: { item: id },
          },
          {
            tool: "comment_on_work_item",
            label: "Say something on it",
            args: { item: id, body: "…" },
          },
        ];
      }
      return [
        {
          tool: "update_work_item",
          label: "Change its status, assignee or dates",
          args: { item: id, status: "in_progress" },
        },
        {
          tool: "comment_on_work_item",
          label: "Say something on it",
          args: { item: id, body: "…" },
        },
        {
          tool: "remove_work_item",
          label: "Take it off the board",
          args: { item: id },
          destructive: true,
        },
      ];
    case "page":
      return [
        {
          tool: "save_page",
          label: "Edit the body",
          args: {
            page: id,
            body: "…",
            // THE VERSION IT EDITED, because a page save states one: there
            // is no per-field merge that makes overwriting prose safe, so a
            // call with no `base_version` is one the engine refuses.
            base_version: subject.version ?? 0,
          },
        },
        { tool: "comment_on_page", label: "Comment on it", args: { page: id, body: "…" } },
      ];
    case "seat":
      return [
        {
          tool: "set_priorities",
          label: "Arrange what they work on next",
          args: { handle: id, items: ["ENG-1", "ENG-2"] },
        },
      ];
    case "project":
      return [
        {
          tool: "write_project",
          label: "Set the sprint policy, fields or default assignee",
          args: { project: id, sprints: { length_days: 14, measure: "points" } },
        },
        {
          tool: "manage_sprint",
          label: "Start or close a sprint",
          args: { project: id, action: "start", sprint: 1 },
        },
      ];
    case "goal":
      return [{ tool: "write_work_goal", label: "Post an update", args: { id, update: "…" } }];
    case "notice":
      // THE INBOX IS THE CALLER'S OWN — `mark_inbox` names no handle, because
      // the operator's credential is whose inbox it is. And an entry is a
      // RECORD AND ITS POSITION, not a bare id: a position from a recreated
      // stream compares as current, so the two travel together.
      return [
        {
          tool: "mark_inbox",
          label: "Mark it read",
          args: { read: [{ record_id: id, position: subject.version ?? 0 }] },
        },
        {
          // `until` IS WHAT MAKES IT A SNOOZE rather than a second spelling
          // of unread: the entry comes back at that instant. RFC3339, and a
          // placeholder rather than a date this screen invented — "tomorrow"
          // is a decision the person makes, not one a copied call should
          // have made for them.
          tool: "mark_inbox",
          label: "Put it off until a time you name",
          args: {
            snoozed: [
              { record_id: id, position: subject.version ?? 0, until: "2026-01-01T09:00:00Z" },
            ],
          },
        },
      ];
  }
}

/** One call as an operator's assistant is asked for it. */
export function callText(call: ToolCall): string {
  return `${call.tool} ${JSON.stringify(call.args)}`;
}
