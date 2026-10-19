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
import type { ArtifactDetail, ArtifactDoc } from "~/protocol/index.ts";

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
        body: init?.body ? JSON.parse(String(init.body)) : undefined,
      });
      const read = /\/versions\/(\d+)\/doc$/.exec(path);
      if (method === "GET" && read) {
        const doc = docs[Number(read[1])];
        return doc ? json(200, { artifact: ID, version: Number(read[1]), doc }) : json(404, {});
      }
      if (method === "POST" && path === `/artifacts/${ID}/versions`) return publishAnswer();
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
