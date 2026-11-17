/** The comment layer over a real page host: layout asked only when needed, pins, and picks. */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { PIN_INSET } from "~/lib/artifacts/comments.ts";
import type { ArtifactCommentThread, ArtifactDetail, LiveSocket } from "~/protocol/index.ts";
import { BRIDGE_MESSAGE } from "~/runtime/bridge.ts";
import { PageHost } from "../host.ts";
import { PagePins } from "./PagePins.tsx";
import { CommentsScope, type ArtifactComments } from "./state.tsx";

vi.mock("~/lib/artifacts/people.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/people.ts")>(
    "~/lib/artifacts/people.ts",
  );
  return { ...actual, useArtifactPeople: () => ({ seatName: (s: string) => s }) };
});

afterEach(cleanup);

const PAGE = "index.html";

function thread(id: string, over: Partial<ArtifactCommentThread>): ArtifactCommentThread {
  const anchor = over.anchor ?? { kind: "html" as const, page: PAGE, name: "pricing" };
  return {
    id,
    version: 1,
    anchor,
    place: anchor,
    label: "Section: Pricing",
    placement: "here",
    status: "open",
    created_by: "avery",
    created_by_kind: "human",
    created_at: "2026-11-15T10:00:00Z",
    updated_at: "2026-11-15T10:00:00Z",
    remarks: [
      {
        id: `${id}-r`,
        author: "avery",
        author_kind: "human",
        body: "Lead with annual",
        created_at: "2026-11-15T10:00:00Z",
      },
    ],
    ...over,
  };
}

const THREADS = [
  thread("t1", {}),
  thread("t2", {
    anchor: { kind: "html", page: PAGE, point: { x: 0.5, y: 0.25 } },
    label: "A point on the page",
  }),
  thread("t3", { status: "resolved" }),
  thread("t4", {
    anchor: { kind: "html", page: "about.html", name: "team" },
    label: "Section: Team",
  }),
];

function comments(over: Partial<ArtifactComments> = {}): ArtifactComments {
  return {
    detail: { id: "a1", kind: "html", head_number: 1, role: "editor" } as ArtifactDetail,
    version: 1,
    answer: null,
    threads: THREADS,
    failed: null,
    numberOf: (id) => THREADS.findIndex((t) => t.id === id) + 1,
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
    startDraft: vi.fn(() => 7),
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

/** A page host with its page attached, and everything said to the page. */
function page() {
  const host = new PageHost({
    artifact: "a1",
    version: 1,
    capabilities: [],
    socket: {} as LiveSocket,
    store: null,
    clientID: "c1",
    newKey: () => "k",
    openLink: async () => true,
    setPresence: () => {},
  });
  const said: Record<string, unknown>[] = [];
  host.attach({
    postMessage: (m: Record<string, unknown>) => said.push(m),
  } as unknown as MessagePort);
  const call = (method: string, params: Record<string, unknown> = {}) =>
    act(async () => {
      await host.receive({ type: BRIDGE_MESSAGE.call, id: 1, method, params }, {
        postMessage: () => {},
      } as unknown as MessagePort);
    });
  const modes = () =>
    said.filter((m) => m["topic"] === "comments" && "mode" in m).map((m) => m["mode"]);
  return { host, said, call, modes };
}

function mount(host: PageHost, value: ArtifactComments) {
  const ui = (v: ArtifactComments) => (
    <Router>
      <ToastProvider>
        <CommentsScope value={v}>
          <PagePins host={host} page={PAGE} />
        </CommentsScope>
      </ToastProvider>
    </Router>
  );
  const out = render(ui(value));
  return { ...out, update: (v: ArtifactComments) => out.rerender(ui(v)) };
}

const LAYOUT = {
  anchors: [{ name: "pricing", x: 40, y: 1000, w: 800, h: 400 }],
  doc: { w: 1280, h: 4000 },
  scroll: { x: 0, y: 900 },
  viewport: { w: 1280, h: 800 },
};

const pin = (name: RegExp) => screen.getByRole("button", { name }) as HTMLElement;

describe("the layer over a page", () => {
  test("asks for the layout while it has pins to draw, and stands each where the page says", async () => {
    const p = page();
    const { unmount } = mount(p.host, comments());
    expect(p.modes()).toEqual(["show"]);
    expect(screen.queryAllByRole("button")).toEqual([]);
    await p.call("comments.layout", LAYOUT);
    // ON THE SECTION'S TOP END CORNER, the document's pixels less its scroll.
    const section = pin(/on Section: Pricing/);
    expect(section.style.left).toBe(`${40 + 800 - PIN_INSET}px`);
    expect(section.style.top).toBe(`${1000 - 900 + PIN_INSET}px`);
    const point = pin(/on A point on the page/);
    expect(point.style.left).toBe("640px");
    expect(point.style.top).toBe("100px");
    // A RESOLVED THREAD under Open, and one on another page, have no pin here.
    expect(screen.getAllByRole("button")).toHaveLength(2);
    unmount();
    expect(p.modes()).toEqual(["show", "off"]);
  });

  test("asks for nothing on a page with no thread to pin", () => {
    const p = page();
    mount(p.host, comments({ threads: [THREADS[3]!] }));
    expect(p.modes()).toEqual([]);
  });

  test("opens a thread from its pin", async () => {
    const p = page();
    const value = comments();
    mount(p.host, value);
    await p.call("comments.layout", LAYOUT);
    act(() => pin(/on Section: Pricing/).click());
    expect(value.openThread).toHaveBeenCalledWith("t1");
  });

  test("while placing, outlines and names the part under the pointer", async () => {
    const p = page();
    const { update } = mount(p.host, comments());
    update(comments({ placing: true }));
    expect(p.modes().at(-1)).toBe("place");
    await p.call("comments.layout", { ...LAYOUT, hover: "pricing" });
    expect(screen.getByText("Pricing")).toBeTruthy();
  });

  test("turns a pick into a draft, and the picture the page draws for it into the draft's", async () => {
    const p = page();
    const value = comments({ placing: true });
    mount(p.host, value);
    await p.call("comments.pick", {
      name: "pricing",
      quote: "Plans from $10",
      point: { x: 0.1, y: 0.3 },
    });
    expect(value.startDraft).toHaveBeenCalledWith({
      anchor: { kind: "html", page: PAGE, name: "pricing" },
      label: "Section: Pricing",
      quote: "Plans from $10",
      crop: undefined,
    });
    const capture = p.said.find((m) => "capture" in m)!["capture"] as { id: number };
    expect(capture).toMatchObject({ name: "pricing" });
    const png = "data:image/png;base64,iVBORw0KGgo=";
    await p.call("comments.crop", { id: capture.id, image: png });
    expect(value.setDraftCrop).toHaveBeenCalledWith(7, png);
  });

  test("a pick on no part is a point, and on a phone opens the panel to write it in", async () => {
    const p = page();
    const value = comments({ placing: true, narrow: true });
    mount(p.host, value);
    await p.call("comments.pick", { name: null, quote: "", point: { x: 0.1, y: 0.3 } });
    expect(value.startDraft).toHaveBeenCalledWith(
      expect.objectContaining({
        anchor: { kind: "html", page: PAGE, point: { x: 0.1, y: 0.3 } },
        label: "A point on the page",
      }),
    );
    expect(value.showPanel).toHaveBeenCalled();
    expect(value.stepAside).not.toHaveBeenCalled();
  });

  test("a pick beside the page asks a panel drawn over the page to make way for the draft", async () => {
    const p = page();
    const value = comments({ placing: true });
    mount(p.host, value);
    await p.call("comments.pick", { name: null, quote: "", point: { x: 0.5, y: 0.5 } });
    expect(value.stepAside).toHaveBeenCalled();
    expect(value.showPanel).not.toHaveBeenCalled();
  });

  test("Escape in the page ends Comment mode", async () => {
    const p = page();
    const value = comments({ placing: true });
    mount(p.host, value);
    await p.call("comments.escape");
    expect(value.setPlacing).toHaveBeenCalledWith(false);
  });

  test("Jump to asks the page to bring the part into view", () => {
    const p = page();
    const value = comments();
    mount(p.host, value);
    const reveal = vi.mocked(value.setRevealer).mock.calls.at(-1)![0]!;
    reveal(THREADS[1]!);
    expect(p.said.at(-1)).toMatchObject({ reveal: { point: { x: 0.5, y: 0.25 } } });
  });
});
