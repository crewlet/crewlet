/**
 * A doc read and edited here: its tabs, the outline of the one on show, and a
 * person's edit saved as a version against the version it started from, with
 * a save that raced somebody else's merged onto theirs rather than lost.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { DocView } from "./DocView.tsx";
import { context, draftField, openFrame, setDraft } from "./draftTesting.ts";
import { WithKindBar } from "~/test/kindControls.tsx";
import { Router } from "~/app/router.tsx";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import type { ArtifactDetail, ArtifactDoc } from "~/protocol/index.ts";
import { append, loadRichEditor, retype } from "~/test/richEditor.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

beforeAll(() => loadRichEditor());

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn() };
});

vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: vi.fn() };
});

const ID = "0192f2c4-2222-7000-8000-000000000002";

const detail = {
  id: ID,
  kind: "doc",
  title: "On-call handbook",
  head_number: 2,
  role: "editor",
} as unknown as ArtifactDetail;

const START = "# Welcome\n\nRead this first.\n\n## Paging\n\nPage the lead within ten minutes.\n";
const ONCALL = "# On call\n\nHand over at nine.\n";

const docs: Record<number, ArtifactDoc> = {
  2: {
    indexed: true,
    tabs: [
      { path: "tabs/start.md", title: "Start", text: START },
      { path: "tabs/oncall.md", title: "On call", text: ONCALL },
    ],
  },
};

/** The index each version opens on, as the raw file route answers it. */
const INDEX = `{"v":1,"tabs":[{"path":"tabs/start.md","title":"Start"},{"path":"tabs/oncall.md","title":"On call"}]}\n`;

/** Every request the view sent, as method, path, If-Match and body. */
let sent: { method: string; path: string; ifMatch: string | null; body: unknown }[] = [];
/** How the next publish is answered. */
let publishAnswer: () => Response;

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
  });

beforeEach(() => {
  location.hash = `#/artifacts/${ID}`;
  sent = [];
  publishAnswer = () => json(201, { number: 3 });
  vi.mocked(useArtifactAccess).mockReturnValue({ can: true, reason: "" } as never);
  // THE VERSION'S FILES, as the socket's `artifact_version` answers them: the
  // index, the tabs, and a file no tab lists.
  const query = vi.fn(async (what: string) =>
    what === "artifact_version"
      ? {
          manifest: {
            entry: "doc.json",
            files: ["doc.json", "tabs/start.md", "tabs/oncall.md", "tabs/budget.md"].map(
              (path) => ({ path, content_type: "text/markdown", object: { hash: "a", size: 1 } }),
            ),
          },
        }
      : {},
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      const method = init?.method ?? "GET";
      const headers = new Headers(init?.headers);
      sent.push({
        method,
        path,
        ifMatch: headers.get("If-Match"),
        body:
          typeof init?.body === "string" && headers.get("Content-Type")?.includes("json")
            ? JSON.parse(init.body)
            : init?.body,
      });
      const read = /\/versions\/(\d+)\/doc$/.exec(path);
      if (method === "GET" && read) {
        const doc = docs[Number(read[1])];
        return doc ? json(200, { artifact: ID, version: Number(read[1]), doc }) : json(404, {});
      }
      if (method === "POST" && path === `/artifacts/${ID}/versions`) return publishAnswer();
      if (method === "GET" && path === `/artifacts/${ID}/versions/2/files/doc.json`) {
        return new Response(INDEX, { status: 200, headers: { "Content-Type": "text/plain" } });
      }
      if (method === "POST" && path === `/artifacts/${ID}/uploads`) {
        const entries = (JSON.parse(String(init!.body)) as { entries: { path: string }[] }).entries;
        return json(201, {
          upload: "u-1",
          expires_at: "2026-10-19T12:00:00Z",
          transfers: entries.map((e) => ({
            path: e.path,
            method: "PUT",
            url: `/artifacts/uploads/u-1/files/${e.path}`,
            direct: false,
          })),
        });
      }
      if (method === "PUT" && path.startsWith("/artifacts/uploads/u-1/files/")) {
        return json(200, { stored: true });
      }
      return json(404, {});
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  delete docs[3];
  location.hash = "";
});

function show(onPublished = vi.fn()) {
  render(
    <ToastProvider>
      <LayerHost>
        <Router>
          <WithKindBar>
            <DocView
              detail={detail}
              version={2}
              presence={{ at: vi.fn() } as never}
              onPublished={onPublished}
            />
          </WithKindBar>
        </Router>
      </LayerHost>
    </ToastProvider>,
  );
  return onPublished;
}

/** The editor's document, by the name it carries for the tab it edits. */
const editor = () => screen.getByRole("textbox", { name: /^The text of/ });

/** Choose one of a tab's actions from its row's menu in the editor's tab list. */
function tabAction(tab: string, action: string) {
  const tabs = screen.getByRole("list", { name: "Tabs of On-call handbook" });
  fireEvent.click(within(tabs).getByRole("button", { name: `Actions for ${tab}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: action }));
}

/** Rename a tab in place, as its menu's Rename does, and keep the title with Enter. */
function renameTab(tab: string, title: string) {
  tabAction(tab, "Rename");
  const field = screen.getByRole("textbox", { name: /^Title of tab/ });
  fireEvent.change(field, { target: { value: title } });
  fireEvent.keyDown(field, { key: "Enter" });
}

/** The editing state the page bar carries, apart from the editor's own live regions. */
const barStatus = () => document.querySelector(".crewlet-artifact-bar__status") as HTMLElement;

/** Opens the editor on the tab on show and retypes one run of its text. */
async function edit(from: string, to: string) {
  fireEvent.click((await screen.findAllByRole("button", { name: "Edit" }))[0]!);
  await retype(editor(), from, to);
}

test("draws the tab on show with its outline, and moves between tabs", async () => {
  show();
  const article = await screen.findByRole("article", { name: "On-call handbook, Start" });
  expect(within(article).getByRole("heading", { name: "Paging" })).toBeDefined();
  expect(screen.getByRole("navigation", { name: "On this page" })).toBeDefined();
  fireEvent.click(screen.getByRole("tab", { name: "On call" }));
  const next = await screen.findByRole("article", { name: "On-call handbook, On call" });
  expect(within(next).getByText("Hand over at nine.")).toBeDefined();
  expect(location.hash).toContain("doctab=tabs%2Foncall.md");
});

test("says a tab with nothing written is empty, and opens the editor from its Edit", async () => {
  const kept = docs[2]!;
  docs[2] = { ...kept, tabs: [kept.tabs[0]!, { ...kept.tabs[1]!, text: "\n" }] };
  try {
    show();
    await screen.findByRole("article", { name: "On-call handbook, Start" });
    fireEvent.click(screen.getByRole("tab", { name: "On call" }));
    const title = await screen.findByRole("heading", { name: "This tab is empty" });
    const empty = title.closest(".crewlet-empty-state") as HTMLElement;
    expect(within(empty).getByText("Write it, or ask an agent to.")).toBeDefined();
    expect(screen.queryByRole("article")).toBeNull();
    // The empty state takes the whole view, with no outline rail beside it.
    expect(document.querySelector(".doc-read--blank")).not.toBeNull();
    expect(document.querySelector(".doc-read-side")).toBeNull();
    fireEvent.click(within(empty).getByRole("button", { name: "Edit" }));
    expect(await screen.findByRole("list", { name: "Tabs of On-call handbook" })).toBeDefined();
  } finally {
    docs[2] = kept;
  }
});

test("gives the outline a column only for a tab that has one", async () => {
  show();
  await screen.findByRole("article", { name: "On-call handbook, Start" });
  // Start has a heading and a section under it: an outline of two entries.
  expect(screen.getByRole("navigation", { name: "On this page" })).toBeDefined();
  expect(document.querySelector(".doc-read-side")).not.toBeNull();
  // On call has one heading: no outline, and no empty rail beside the text.
  fireEvent.click(screen.getByRole("tab", { name: "On call" }));
  await screen.findByRole("article", { name: "On-call handbook, On call" });
  expect(screen.queryByRole("navigation", { name: "On this page" })).toBeNull();
  expect(document.querySelector(".doc-read-side")).toBeNull();
  expect(document.querySelector(".doc-read--bare")).not.toBeNull();
});

test("saves an edit as the one changed passage, against the version it started from", async () => {
  const onPublished = show();
  await edit("ten minutes", "five minutes");
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(onPublished).toHaveBeenCalledWith(3));
  const publish = sent.find((r) => r.method === "POST")!;
  expect(publish.ifMatch).toBe('"2"');
  const body = publish.body as {
    mode: string;
    edits: { path: string; old_str: string; new_str: string }[];
  };
  expect(body.mode).toBe("patch");
  expect(body.edits).toHaveLength(1);
  expect(body.edits[0]!.path).toBe("tabs/start.md");
  expect(body.edits[0]!.old_str).toBe("ten");
  expect(body.edits[0]!.new_str).toBe("five");
});

test("the page bar carries the editing state, Cancel and Save while the doc is edited", async () => {
  show();
  fireEvent.click((await screen.findAllByRole("button", { name: "Edit" }))[0]!);
  await screen.findByRole("button", { name: "Save" });
  expect(barStatus().textContent).toContain("Editing");
  expect(barStatus().textContent).not.toContain("Unsaved changes");
  expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  await retype(editor(), "ten minutes", "five minutes");
  await waitFor(() => expect(barStatus().textContent).toContain("Unsaved changes"));
  // THE BAR DRAWS A LABELLED TOOL ONCE PER WIDTH; the first is the one on show here.
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  fireEvent.click(await screen.findByRole("button", { name: "Discard" }));
  expect((await screen.findAllByRole("button", { name: "Edit" })).length).toBeGreaterThan(0);
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  expect(screen.getByRole("article", { name: "On-call handbook, Start" }).textContent).toContain(
    "ten minutes",
  );
});

test("a save that raced another publish is merged onto it, and saved again against it", async () => {
  docs[3] = {
    indexed: true,
    tabs: [
      {
        path: "tabs/start.md",
        title: "Start",
        text: START.replace("Read this first.", "Read this before your first shift."),
      },
      { path: "tabs/oncall.md", title: "On call", text: ONCALL },
    ],
  };
  publishAnswer = () => json(412, { error: "version_moved", detail: "moved", live: { number: 3 } });
  show();
  await edit("ten minutes", "five minutes");
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("Version 3 is merged into your draft.")).toBeDefined();
  expect(editor().textContent).toContain("Read this before your first shift.");
  expect(editor().textContent).toContain("five minutes");

  publishAnswer = () => json(201, { number: 4 });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(sent.filter((r) => r.method === "POST")).toHaveLength(2));
  expect(sent.filter((r) => r.method === "POST")[1]!.ifMatch).toBe('"3"');
});

test("where both changed the same lines, the person settles each before saving", async () => {
  docs[3] = {
    indexed: true,
    tabs: [
      { path: "tabs/start.md", title: "Start", text: START.replace("ten minutes", "two minutes") },
      { path: "tabs/oncall.md", title: "On call", text: ONCALL },
    ],
  };
  publishAnswer = () => json(412, { error: "version_moved", detail: "moved", live: { number: 3 } });
  show();
  await edit("ten minutes", "five minutes");
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  const clash = await screen.findByRole("region", {
    name: /You and version 3 changed the same lines/,
  });
  fireEvent.click(within(clash).getByRole("radio", { name: "Theirs" }));
  fireEvent.click(within(clash).getByRole("button", { name: "Apply to my draft" }));
  expect(editor().textContent).toContain("two minutes");
});

test("adds, renames, moves and removes tabs, and saves them as one version", async () => {
  const onPublished = show();
  fireEvent.click((await screen.findAllByRole("button", { name: "Edit" }))[0]!);
  renameTab("Start", "Start here");
  // A NEW TAB'S FILE IS NEVER ONE THE VERSION HOLDS: tabs/budget.md is taken.
  fireEvent.click(screen.getByRole("button", { name: "Add tab" }));
  const named = screen.getByRole("textbox", { name: "New tab title" });
  fireEvent.change(named, { target: { value: "Budget" } });
  fireEvent.keyDown(named, { key: "Enter" });
  // The new tab opens on its title as its heading, in the editor.
  expect(within(editor()).getByRole("heading", { name: "Budget" })).toBeDefined();
  await append(editor(), "Two seats.");
  tabAction("Budget", "Move up");
  tabAction("On call", "Remove");
  const save = screen.getByRole("button", { name: "Save" });
  await waitFor(() => expect(save.getAttribute("aria-disabled")).toBeNull());
  fireEvent.click(save);
  await waitFor(() => expect(onPublished).toHaveBeenCalledWith(3));

  const upload = sent.find((r) => r.path === `/artifacts/${ID}/uploads`)!;
  const entries = (upload.body as { entries: { path: string; size: number; sha256: string }[] })
    .entries;
  expect(entries.map((e) => e.path)).toEqual(["tabs/budget-2.md"]);
  expect(entries[0]!.size).toBe("# Budget\n\nTwo seats.\n".length);
  expect(entries[0]!.sha256).toMatch(/^[0-9a-f]{64}$/);
  expect(sent.find((r) => r.method === "PUT")!.body).toBe("# Budget\n\nTwo seats.\n");

  const publish = sent.find((r) => r.path === `/artifacts/${ID}/versions`)!;
  expect(publish.ifMatch).toBe('"2"');
  const body = publish.body as {
    upload: string;
    files: unknown[];
    edits: { path: string; old_str: string; new_str: string }[];
  };
  expect(body.upload).toBe("u-1");
  expect(body.files).toEqual([
    { path: "tabs/budget-2.md", upload: true },
    { path: "tabs/oncall.md", remove: true },
  ]);
  const index = body.edits.find((e) => e.path === "doc.json")!;
  const at = INDEX.indexOf(index.old_str);
  const next = INDEX.slice(0, at) + index.new_str + INDEX.slice(at + index.old_str.length);
  expect(JSON.parse(next)).toEqual({
    v: 1,
    tabs: [
      { path: "tabs/budget-2.md", title: "Budget" },
      { path: "tabs/start.md", title: "Start here" },
    ],
  });
});

test("a tab both renamed is the person's to keep or take, and the save waits for it", async () => {
  docs[3] = {
    indexed: true,
    tabs: [
      { path: "tabs/start.md", title: "Read me", text: START },
      { path: "tabs/oncall.md", title: "On call", text: ONCALL },
    ],
  };
  publishAnswer = () => json(412, { error: "version_moved", detail: "moved", live: { number: 3 } });
  show();
  fireEvent.click((await screen.findAllByRole("button", { name: "Edit" }))[0]!);
  renameTab("Start", "Start here");
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  const note = await screen.findByText(/Version 3 renamed “Start here” too/);
  const save = screen.getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(
    within(note.closest('[role="alert"]')!).getByRole("button", { name: "Take theirs" }),
  );
  const tabs = screen.getByRole("list", { name: "Tabs of On-call handbook" });
  expect(within(tabs).getByRole("button", { name: "Read me" })).toBeDefined();
  expect(await screen.findByText("Version 3 is merged into your draft.")).toBeDefined();
});

// THE EDIT IS THE ASSISTANT'S TO READ while the doc is edited, over the
// version it is based on, and a change it proposes is saved as the writer's
// own edit: one changed passage against that version.
test("the open edit is the assistant's to read, and a draft.set is saved as the writer's edit", async () => {
  openFrame("artifact");
  const onPublished = show();
  fireEvent.click((await screen.findAllByRole("button", { name: "Edit" }))[0]!);
  await screen.findByRole("button", { name: "Save" });
  expect(context().draft).toMatchObject({ kind: "client", id: ID, revision: "2", step: "tabs.1" });
  expect(draftField("tabs.2.title")).toMatchObject({ value: "On call", editable: true });
  expect(draftField("tabs.1.text")?.value).toBe(START);
  expect(setDraft({ "tabs.1.text": START.replace("ten minutes", "five minutes") })).toMatchObject({
    outcome: "applied",
  });
  await waitFor(() => expect(editor().textContent).toContain("five minutes"));
  expect(setDraft({ "tabs.2.title": " " })).toMatchObject({
    outcome: "invalid",
    reason: "Give the tab a title.",
  });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(onPublished).toHaveBeenCalledWith(3));
  const body = sent.find((r) => r.method === "POST")!.body as {
    edits: { path: string; old_str: string; new_str: string }[];
  };
  expect(body.edits).toEqual([
    expect.objectContaining({ path: "tabs/start.md", old_str: "ten", new_str: "five" }),
  ]);
});
