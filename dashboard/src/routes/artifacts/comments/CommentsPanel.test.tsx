/** The Comments panel: how a commenter starts, each card state, a lower role's view, the writes. */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import type {
  ArtifactCommentThread,
  ArtifactCommentsAnswer,
  ArtifactDetail,
} from "~/protocol/index.ts";
import { CommentsPanel } from "./CommentsPanel.tsx";
import { CommentsScope, type ArtifactComments } from "./state.tsx";

const ID = "0192f2c4-3333-7000-8000-000000000003";
const NOW = Date.parse("2026-11-16T10:00:00Z");

const reader = vi.hoisted(() => ({
  handle: "avery",
  login: "avery",
  owner: "avery",
  admin: false,
}));
vi.mock("~/lib/viewer.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/viewer.ts")>("~/lib/viewer.ts");
  return { ...actual, useViewer: () => reader };
});
vi.mock("~/lib/artifacts/people.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/people.ts")>(
    "~/lib/artifacts/people.ts",
  );
  const names: Record<string, string> = { avery: "Avery Lin", pm: "Product Lead" };
  const people = {
    seatName: (s: string) => names[s] ?? s,
    seatPrincipal: (s: string) => ({
      id: s,
      name: names[s] ?? s,
      kind: s === "pm" ? "agent" : "person",
    }),
  };
  return { ...actual, useArtifactPeople: () => people };
});

const SECTION = { kind: "html", page: "index.html", name: "pricing" } as const;

function remark(id: string, author: string, body: string) {
  return {
    id,
    author,
    author_kind: author === "pm" ? "agent" : "human",
    body,
    created_at: "2026-11-15T10:00:00Z",
  };
}

function thread(over: Partial<ArtifactCommentThread> = {}): ArtifactCommentThread {
  return {
    id: "t1",
    version: 3,
    anchor: SECTION,
    label: "Section: Pricing",
    placement: "here",
    place: SECTION,
    quote: "Plans from $10 a month",
    status: "open",
    created_by: "avery",
    created_by_kind: "human",
    created_at: "2026-11-15T10:00:00Z",
    updated_at: "2026-11-15T10:00:00Z",
    remarks: [
      remark("r1", "avery", "The annual price is buried."),
      remark("r2", "pm", "Moved up."),
    ],
    ...over,
  };
}

function answer(threads: ArtifactCommentThread[]): ArtifactCommentsAnswer {
  return {
    artifact: ID,
    version: 4,
    head: 4,
    counts: {
      open: threads.filter((t) => t.status === "open" && t.placement !== "outdated").length,
      resolved: threads.filter((t) => t.status === "resolved").length,
      outdated: threads.filter((t) => t.status === "open" && t.placement === "outdated").length,
    },
    can_comment: true,
    role: "editor",
    threads,
  };
}

/** The page's comments, as the panel reads them, with every call recorded. */
function comments(over: Partial<ArtifactComments> = {}): ArtifactComments {
  const a = over.answer === undefined ? answer([thread()]) : over.answer;
  const threads = a?.threads ?? [];
  return {
    detail: { id: ID, kind: "html", head_number: 4, role: "editor" } as ArtifactDetail,
    version: 4,
    answer: a,
    threads,
    failed: null,
    numberOf: (id) => threads.findIndex((t) => t.id === id) + 1,
    refetch: vi.fn(),
    filter: "open",
    setFilter: vi.fn(),
    selected: null,
    select: vi.fn(),
    openThread: vi.fn(),
    showPanel: vi.fn(),
    stepAside: vi.fn(),
    placing: false,
    setPlacing: vi.fn(),
    draft: null,
    startDraft: vi.fn(() => 1),
    setDraftCrop: vi.fn(),
    dockDraft: vi.fn(),
    clearDraft: vi.fn(),
    draftWords: { current: "" },
    can: true,
    reason: undefined,
    placeable: true,
    narrow: false,
    ownerName: "Product Lead",
    reveal: vi.fn(),
    setRevealer: vi.fn(),
    lendPage: vi.fn(() => () => {}),
    pageHost: () => null,
    ...over,
  };
}

let sent: { method: string; path: string; body: unknown; key: string | null }[];

beforeEach(() => {
  sent = [];
  reader.admin = false;
  location.hash = `#/artifacts/${ID}?panel=comments`;
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        path: new URL(String(input), "http://engine.test").pathname,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : null,
        key: new Headers(init?.headers).get("Idempotency-Key"),
      });
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    }),
  });
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

function mount(value: ArtifactComments, onPlace = vi.fn()) {
  render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <CommentsScope value={value}>
            <CommentsPanel now={NOW} onPlace={onPlace} />
          </CommentsScope>
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
  return onPlace;
}

/** A thread's card, by the words it is about. */
const card = (label: string) => screen.getByText(label).closest("li")!;

describe("a commenter", () => {
  test("starts on a part or on the version, and reads the open threads under their counts", () => {
    const onPlace = mount(comments());
    fireEvent.click(screen.getByRole("button", { name: "Comment on a part" }));
    expect(onPlace).toHaveBeenCalled();
    expect(screen.getByText("Product Lead owns this artifact and is told of every comment."));
    const lists = screen.getByRole("radiogroup", { name: "Show threads" });
    expect(within(lists).getByRole("radio", { name: /Open/ }).textContent).toContain("1");
    const t = within(card("Section: Pricing"));
    expect(t.getByText("v3")).toBeTruthy();
    expect(t.getByText("Plans from $10 a month")).toBeTruthy();
    expect(t.getByText("The annual price is buried.")).toBeTruthy();
    expect(t.getByRole("button", { name: "Jump to it" })).toBeTruthy();
    expect(t.getByRole("button", { name: "Resolve" })).toBeTruthy();
  });

  test("a kind with no parts to place on offers the version alone", () => {
    mount(comments({ placeable: false }));
    expect(screen.queryByRole("button", { name: "Comment on a part" })).toBeNull();
    expect(screen.getByRole("button", { name: "Comment on v4" })).toBeTruthy();
  });

  test("comments on the version as a whole, as one keyed write naming it", async () => {
    const value = comments();
    mount(value);
    fireEvent.click(screen.getByRole("button", { name: "Comment on v4" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Comment on version 4" }), {
      target: { value: "Ship it" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    });
    expect(sent).toEqual([
      {
        method: "POST",
        path: `/artifacts/${ID}/comments`,
        body: { body: "Ship it", version: 4, anchor: { kind: "version" } },
        key: expect.any(String),
      },
    ]);
    expect(value.refetch).toHaveBeenCalled();
  });

  test("resolves a thread naming the version on show, which addressed it", async () => {
    const value = comments();
    mount(value);
    await act(async () => {
      fireEvent.click(within(card("Section: Pricing")).getByRole("button", { name: "Resolve" }));
    });
    expect(sent).toEqual([
      {
        method: "POST",
        path: `/artifacts/${ID}/comments/t1/resolve`,
        body: { version: 4 },
        key: expect.any(String),
      },
    ]);
  });

  test("Jump to it selects the thread, lets a panel over the artifact make way, and brings it into view", () => {
    const value = comments();
    mount(value);
    fireEvent.click(within(card("Section: Pricing")).getByRole("button", { name: "Jump to it" }));
    expect(value.select).toHaveBeenCalledWith("t1");
    expect(value.stepAside).toHaveBeenCalled();
    expect(value.reveal).toHaveBeenCalledWith(value.threads[0]);
  });

  test("replies only on the thread selected, and edits and deletes only their own remark", async () => {
    mount(comments({ selected: "t1" }));
    const t = within(card("Section: Pricing"));
    expect(t.getAllByRole("button", { name: "Edit your comment" })).toHaveLength(1);
    expect(t.getAllByRole("button", { name: /^Delete the comment by/ })).toHaveLength(1);
    fireEvent.change(t.getByRole("textbox", { name: "Reply on Section: Pricing" }), {
      target: { value: "Thanks" },
    });
    await act(async () => {
      fireEvent.click(t.getByRole("button", { name: "Reply" }));
    });
    expect(sent[0]).toMatchObject({
      method: "POST",
      path: `/artifacts/${ID}/comments/t1/replies`,
      body: { body: "Thanks" },
    });
  });

  test("an administrator may take down anybody's remark, and rewrite only their own", () => {
    reader.admin = true;
    mount(comments());
    const t = within(card("Section: Pricing"));
    expect(t.getAllByRole("button", { name: /^Delete the comment by/ })).toHaveLength(2);
    expect(t.getAllByRole("button", { name: "Edit your comment" })).toHaveLength(1);
  });
});

describe("a reader who may not comment", () => {
  test("reads every thread and is told why they cannot write", () => {
    const reason = "Commenting needs the “Can comment” role, and you can only view this artifact.";
    mount(comments({ can: false, reason }));
    expect(screen.getByText(reason)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Comment on a part" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Comment on v/ })).toBeNull();
    const t = within(card("Section: Pricing"));
    expect(t.getByText("Moved up.")).toBeTruthy();
    expect(t.queryByRole("button", { name: "Resolve" })).toBeNull();
    expect(t.queryByRole("button", { name: "Reply" })).toBeNull();
    expect(t.queryByRole("button", { name: "Edit your comment" })).toBeNull();
    expect(t.queryByRole("textbox")).toBeNull();
    // READING IS NOT WRITING: the part can still be brought into view.
    expect(t.getByRole("button", { name: "Jump to it" })).toBeTruthy();
  });
});

describe("a thread's state", () => {
  test("a resolved thread cites the version that addressed it, links it, and can be reopened", async () => {
    const resolved = thread({
      status: "resolved",
      resolved_by: "pm",
      resolved_by_kind: "agent",
      resolved_version: 5,
      resolved_at: "2026-11-15T12:00:00Z",
    });
    mount(comments({ answer: answer([resolved]), filter: "resolved" }));
    const t = within(card("Section: Pricing"));
    expect(t.getByText("Resolved in v5 by Product Lead")).toBeTruthy();
    expect(t.getByRole("link", { name: "Show v5" }).getAttribute("href")).toBe(
      `#/artifacts/${ID}?panel=comments&version=5`,
    );
    await act(async () => {
      fireEvent.click(t.getByRole("button", { name: "Reopen" }));
    });
    expect(sent[0]).toMatchObject({ method: "POST", path: `/artifacts/${ID}/comments/t1/reopen` });
  });

  test("an outdated thread says why, links the version it was made on, and stays answerable", () => {
    const gone = thread({ version: 2, placement: "outdated", place: undefined });
    mount(comments({ answer: answer([gone]), filter: "outdated" }));
    const t = within(card("Section: Pricing"));
    expect(t.getByText("Outdated: v4 has no pricing section")).toBeTruthy();
    expect(t.getByRole("link", { name: "Show v2, where it was made" }).getAttribute("href")).toBe(
      `#/artifacts/${ID}?panel=comments&version=2`,
    );
    expect(t.queryByRole("button", { name: "Jump to it" })).toBeNull();
    expect(t.getByRole("button", { name: "Resolve" })).toBeTruthy();
    expect(t.getByRole("button", { name: "Reply" })).toBeTruthy();
  });

  test("a list with nothing under it says so, and an artifact with no thread says how to start", () => {
    mount(comments({ filter: "resolved" }));
    expect(screen.getByText("Nothing is resolved yet.")).toBeTruthy();
    cleanup();
    mount(comments({ answer: answer([]) }));
    expect(screen.getByText("No comments yet")).toBeTruthy();
  });
});

describe("a draft", () => {
  test("is written in the panel on a phone: where it points, its words, and its picture as it is drawn", () => {
    const value = comments({
      narrow: true,
      draft: {
        seq: 1,
        anchor: SECTION,
        label: "Section: Pricing",
        quote: "Plans from $10 a month",
        crop: undefined,
      },
    });
    mount(value);
    // THE WELL WAITS, BUSY, and the picture cannot be chosen until it is there.
    expect(screen.getByText("Drawing a picture of this part…").getAttribute("role")).toBe("status");
    expect(document.querySelector(".artifact-comment-preview")!.getAttribute("aria-busy")).toBe(
      "true",
    );
    const include = screen.getByRole("checkbox", { name: /Include this picture/ });
    expect((include as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByRole("textbox", { name: "Comment on Section: Pricing" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(value.clearDraft).toHaveBeenCalled();
  });

  test("posts its anchor, its words and the picture unless the reader leaves it out", async () => {
    const png = "data:image/png;base64,iVBORw0KGgo=";
    const value = comments({
      narrow: true,
      draft: { seq: 1, anchor: SECTION, label: "Section: Pricing", quote: "Plans", crop: png },
    });
    mount(value);
    fireEvent.change(screen.getByRole("textbox", { name: "Comment on Section: Pricing" }), {
      target: { value: "Lead with annual" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    });
    expect(sent[0]).toMatchObject({
      method: "POST",
      path: `/artifacts/${ID}/comments`,
      body: {
        body: "Lead with annual",
        version: 4,
        anchor: SECTION,
        quote: "Plans",
        crop: "iVBORw0KGgo=",
        crop_type: "image/png",
      },
    });
    expect(value.setPlacing).toHaveBeenCalledWith(false);
    cleanup();
    sent = [];
    mount(value);
    fireEvent.click(screen.getByRole("checkbox", { name: /Include this picture/ }));
    fireEvent.change(screen.getByRole("textbox", { name: "Comment on Section: Pricing" }), {
      target: { value: "Again" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    });
    expect(sent[0]!.body).not.toHaveProperty("crop");
  });

  test("says when no picture could be drawn, and posts none", async () => {
    const value = comments({
      narrow: true,
      draft: { seq: 1, anchor: SECTION, label: "Section: Pricing", crop: null },
    });
    mount(value);
    expect(screen.getByText("No picture of this part could be drawn.")).toBeTruthy();
    const include = screen.getByRole("checkbox", { name: /Include this picture/ });
    expect((include as HTMLInputElement).disabled).toBe(true);
    expect((include as HTMLInputElement).checked).toBe(false);
    fireEvent.change(screen.getByRole("textbox", { name: "Comment on Section: Pricing" }), {
      target: { value: "Lead with annual" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    });
    expect(sent[0]!.body).not.toHaveProperty("crop");
  });
});
