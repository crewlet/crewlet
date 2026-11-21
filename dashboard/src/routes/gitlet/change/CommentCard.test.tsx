/**
 * A comment's Edit (its author's alone) and Delete, and a deleted one's tombstone with its replies.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type CodeChangeDetail, type CodeCommentRow } from "~/protocol/index.ts";
import { engineJson } from "~/test/engineAnswer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { COMMENT_MOVED } from "./CommentCard.tsx";
import { Thread } from "./Conversation.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };

/** The reader: the person `jane`, offered both comment writes. */
const VIEWER = { ...ADMIN_VIEWER, acts: ["code_comment_on_change", "code_delete_comment"] };

const CHANGE = {
  id: "change-1",
  repo_id: "repo-web",
  number: 42,
  title: "Retry the flaky upload",
  state: "open",
  source_ref: "refs/heads/retry",
  target_ref: "refs/heads/main",
  head: "0123456789abcdef0123456789abcdef01234567",
  revision: 2,
  owner: "dev",
  owner_kind: "agent",
  opened_at: "2026-10-27T10:00:00Z",
  opened_position: POSITION,
  version: 5,
  position: POSITION,
  repo: "web",
  ref: "web!42",
  authors: [{ author: "dev", kind: "agent", first_revision: 1, position: POSITION }],
} as unknown as CodeChangeDetail;

const comment = (over: Partial<CodeCommentRow>): CodeCommentRow => ({
  id: "c-root",
  thread_id: "c-root",
  author: "jane",
  author_kind: "person",
  body: "Why a minte?",
  position: POSITION,
  at: "2026-10-27T10:05:00Z",
  ...over,
});

interface Acted {
  tool: string;
  args: Record<string, unknown>;
}

/** The engine: every act recorded and applied. */
function stubEngine(): Acted[] {
  const acted: Acted[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.startsWith("/operator/act/")) {
        acted.push({
          tool: decodeURIComponent(url.pathname.split("/operator/act/")[1] ?? ""),
          args: (JSON.parse(String(init?.body)) as { args: Record<string, unknown> }).args,
        });
        return engineJson({ outcome: "applied" }, 200);
      }
      return new Promise<Response>(() => {});
    }),
  );
  return acted;
}

function mount(root: CodeCommentRow, replies: CodeCommentRow[] = [], viewer: unknown = VIEWER) {
  const store = new Store();
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  const view = (r: CodeCommentRow, rs: CodeCommentRow[]) => (
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Thread root={r} replies={rs} change={CHANGE} />
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>
  );
  const rendered = render(view(root, replies));
  return {
    redraw: (r: CodeCommentRow, rs: CodeCommentRow[] = replies) => rendered.rerender(view(r, rs)),
  };
}

async function openMenu(at = 0) {
  const menus = await screen.findAllByRole("button", { name: "More actions on the comment" });
  fireEvent.click(menus[at]!);
}

// EDIT IS THE AUTHOR'S: one code_comment_on_change naming the comment, Save
// held over the words as they stand. Mutation: send it as a new comment, or
// offer Edit on another's comment, and this fails.
test("a person edits their own comment in place, and only their own", async () => {
  const acted = stubEngine();
  mount(comment({}), [
    comment({
      id: "c-reply",
      parent_id: "c-root",
      author: "dev",
      author_kind: "agent",
      body: "The limit.",
    }),
  ]);
  await openMenu(1);
  expect(screen.queryByRole("menuitem", { name: "Edit" })).toBeNull();
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  await openMenu(0);
  fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));
  const field = (await screen.findByRole("textbox", { name: "Comment" })) as HTMLTextAreaElement;
  const save = screen.getByRole("button", { name: "Save" });
  expect(save.getAttribute("title")).toBe("Change the comment first.");
  fireEvent.change(field, { target: { value: "Why a minute?" } });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_comment_on_change",
    args: { change: "web!42", edit: "c-root", body: "Why a minute?" },
  });
});

// AN EDIT RACED BY ANOTHER: Save is held once the words on screen moved. Mutation:
// compare against the words as they are now, and the stale edit is sent.
test("an edit is held once the comment changed under it", async () => {
  const acted = stubEngine();
  const { redraw } = mount(comment({}));
  await openMenu();
  fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));
  const field = (await screen.findByRole("textbox", { name: "Comment" })) as HTMLTextAreaElement;
  fireEvent.change(field, { target: { value: "Mine." } });
  redraw(comment({ body: "Edited elsewhere.", edited_at: "2026-10-27T10:06:00Z" }));
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Save" }).getAttribute("title")).toBe(COMMENT_MOVED),
  );
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(acted).toHaveLength(0);
});

// DELETE IS CONFIRMED, then one code_delete_comment; anybody's comment is a
// person's to delete. Mutation: delete on the menu press, or hide Delete on
// another's comment.
test("a person deletes anybody's comment after a confirmation", async () => {
  const acted = stubEngine();
  mount(comment({ author: "dev", author_kind: "agent" }));
  await openMenu();
  fireEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
  const dialog = await screen.findByRole("alertdialog", { name: "Delete this comment?" });
  expect(acted).toHaveLength(0);
  expect(dialog.textContent).toContain("every reply stay");
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete comment" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_delete_comment",
    args: { change: "web!42", comment: "c-root" },
  });
});

// A DELETED COMMENT IS A TOMBSTONE: who deleted it, no words, no menu, its
// reply under it; an edited one says so. Mutation: drop the deleted root's
// thread, or draw its (empty) words with a menu.
test("a deleted comment keeps its place and its replies, and an edited one says edited", () => {
  stubEngine();
  mount(
    comment({
      body: "",
      deleted_at: "2026-10-27T10:07:00Z",
      deleted_by: "cto",
      deleted_by_kind: "agent",
    }),
    [
      comment({
        id: "c-reply",
        parent_id: "c-root",
        author: "dev",
        author_kind: "agent",
        body: "Rotated.",
        edited_at: "2026-10-27T10:08:00Z",
      }),
    ],
  );
  const thread = screen.getByRole("article");
  expect(thread.textContent).toMatch(/This comment was deleted by \S+/);
  expect(thread.textContent).toContain("Rotated.");
  expect(screen.getAllByRole("button", { name: "More actions on the comment" })).toHaveLength(1);
  expect(within(thread).getByText("edited").getAttribute("title")).toMatch(/^Edited /);
});

// EVERY READER SEES THE MENU, its writes held with the reason they cannot act.
// Mutation: hide Delete from a reader who cannot act, and this fails.
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
    ["Delete"],
  ],
  [
    "a person the engine does not serve",
    { ...VIEWER, acts: [] },
    WRITE_REASONS.not_served,
    ["Edit", "Delete"],
  ],
])(
  "a comment's menu is drawn for %s, held with the reason",
  async (_who, viewer, reason, items) => {
    const acted = stubEngine();
    mount(comment({}), [], viewer);
    await openMenu();
    for (const name of items) {
      const item = await screen.findByRole("menuitem", { name: new RegExp(`^${name}`) });
      await waitFor(() => expect(item.getAttribute("aria-disabled"), name).toBe("true"));
      expect(item.textContent, name).toContain(reason);
    }
    expect(acted).toHaveLength(0);
  },
);
