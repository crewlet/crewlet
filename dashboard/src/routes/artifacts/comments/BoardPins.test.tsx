/** Comment pins on a design's canvas: a pin placed on a board, and the picture its page draws. */

import { act, cleanup, render } from "@testing-library/react";
import { createRef } from "react";
import { afterEach, expect, test, vi } from "vitest";
import type { CanvasHandle } from "@crewlethq/ui";
import type { ArtifactDetail, LiveSocket } from "~/protocol/index.ts";
import { PageHost } from "../host.ts";
import { BoardPins } from "./BoardPins.tsx";
import { CommentsScope, type ArtifactComments } from "./state.tsx";

afterEach(cleanup);

const BOARD = { id: "boards/phone.html", x: 1000, y: 200, w: 390, h: 844, title: "Phone" };

function comments(host: PageHost): ArtifactComments {
  return {
    detail: { id: "a1", kind: "design", head_number: 1, role: "editor" } as ArtifactDetail,
    version: 1,
    answer: null,
    threads: [],
    failed: null,
    numberOf: () => 0,
    refetch: vi.fn(),
    filter: "open",
    setFilter: vi.fn(),
    selected: null,
    select: vi.fn(),
    openThread: vi.fn(),
    showPanel: vi.fn(),
    stepAside: vi.fn(),
    placing: true,
    setPlacing: vi.fn(),
    draft: null,
    startDraft: vi.fn(() => 3),
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
    pageHost: (id) => (id === BOARD.id ? host : null),
  };
}

test("a pin on a board asks its page for the picture at the board's pixels, which the page scrolls past", () => {
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
  // THE CANVAS AT ITS OWN SCALE: a client point is the world point.
  const canvas = createRef<CanvasHandle>() as { current: CanvasHandle | null };
  canvas.current = {
    worldAt: (p: { x: number; y: number }) => p,
    screenRect: (r: { x: number; y: number; width: number; height: number }) => r,
    reveal: vi.fn(),
  } as unknown as CanvasHandle;
  const value = comments(host);
  const { container } = render(
    <CommentsScope value={value}>
      <BoardPins canvas={canvas} boards={[BOARD]} moved={new Set()} />
    </CommentsScope>,
  );
  const catcher = container.querySelector(".artifact-comment-catch")!;
  act(() => {
    catcher.dispatchEvent(
      new MouseEvent("click", { bubbles: true, clientX: BOARD.x + 30, clientY: BOARD.y + 700 }),
    );
  });
  expect(value.startDraft).toHaveBeenCalledWith(
    expect.objectContaining({ anchor: { kind: "design", board: BOARD.id, x: 30, y: 700 } }),
  );
  expect(said.find((m) => "capture" in m)).toMatchObject({
    capture: { view: { x: 30, y: 700 } },
  });
});
