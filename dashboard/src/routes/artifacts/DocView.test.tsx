/**
 * A doc read and edited here: its tabs, the outline of the one on show, and a
 * person's edit saved as a version against the version it started from, with
 * a save that raced somebody else's merged onto theirs rather than lost.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { DocView } from "./DocView.tsx";
import { Router } from "~/app/router.tsx";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import type { ArtifactDetail, ArtifactDoc } from "~/protocol/index.ts";

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
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

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
          <DocView
            detail={detail}
            version={2}
            presence={{ at: vi.fn() } as never}
            onPublished={onPublished}
          />
        </Router>
      </LayerHost>
    </ToastProvider>,
  );
  return onPublished;
}

/** Opens the editor on the tab on show and replaces its text. */
async function editTo(text: string) {
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const box = screen.getByRole("textbox", { name: /in Markdown/ });
  fireEvent.change(box, { target: { value: text } });
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

test("saves an edit as the one changed passage, against the version it started from", async () => {
  const onPublished = show();
  await editTo(START.replace("ten minutes", "five minutes"));
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
  await editTo(START.replace("ten minutes", "five minutes"));
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("Version 3 is merged into your draft.")).toBeDefined();
  const box = screen.getByRole("textbox", { name: /in Markdown/ }) as HTMLTextAreaElement;
  expect(box.value).toContain("Read this before your first shift.");
  expect(box.value).toContain("five minutes");

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
  await editTo(START.replace("ten minutes", "five minutes"));
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  const clash = await screen.findByRole("region", {
    name: /You and version 3 changed the same lines/,
  });
  fireEvent.click(within(clash).getByRole("radio", { name: "Theirs" }));
  fireEvent.click(within(clash).getByRole("button", { name: "Apply to my draft" }));
  const box = screen.getByRole("textbox", { name: /in Markdown/ }) as HTMLTextAreaElement;
  expect(box.value).toContain("two minutes");
});

test("adds, renames, moves and removes tabs, and saves them as one version", async () => {
  const onPublished = show();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const tabs = screen.getByRole("list", { name: "Tabs of On-call handbook" });
  fireEvent.change(within(tabs).getByRole("textbox", { name: "Title of tab 1" }), {
    target: { value: "Start here" },
  });
  // A NEW TAB'S FILE IS NEVER ONE THE VERSION HOLDS: tabs/budget.md is taken.
  fireEvent.change(screen.getByRole("textbox", { name: "New tab title" }), {
    target: { value: "Budget" },
  });
  fireEvent.click(screen.getByRole("button", { name: /Add tab/ }));
  const added = screen.getByRole("textbox", { name: /in Markdown/ }) as HTMLTextAreaElement;
  expect(added.value).toBe("# Budget\n");
  fireEvent.change(added, { target: { value: "# Budget\n\nTwo seats.\n" } });
  fireEvent.click(within(tabs).getByRole("button", { name: "Move Budget up" }));
  fireEvent.click(within(tabs).getByRole("button", { name: "Remove On call" }));
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
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  fireEvent.change(screen.getByRole("textbox", { name: "Title of tab 1" }), {
    target: { value: "Start here" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  const note = await screen.findByText(/Version 3 renamed “Start here” too/);
  const save = screen.getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(
    within(note.closest('[role="alert"]')!).getByRole("button", { name: "Take theirs" }),
  );
  expect((screen.getByRole("textbox", { name: "Title of tab 1" }) as HTMLInputElement).value).toBe(
    "Read me",
  );
  expect(await screen.findByText("Version 3 is merged into your draft.")).toBeDefined();
});
