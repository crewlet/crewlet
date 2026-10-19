/**
 * A page artifact before it has a frame: while its ticket is minted it shows
 * the frame's own placeholder, and a refused ticket is said as the frame says
 * a page that failed to start, in the one box the page will fill.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { RestError, type ArtifactDetail } from "~/protocol/index.ts";
import { mintFrame } from "~/lib/artifacts/api.ts";
import { FrameView } from "./FrameView.tsx";

vi.mock("~/lib/artifacts/api.ts", () => ({ mintFrame: vi.fn(), reportReady: vi.fn() }));
// Each hook answers the same object every render, as the real ones do while
// nothing moves: the host is rebuilt for a new socket, so a fresh one per
// render would rebuild it forever.
vi.mock("~/lib/store-hooks.ts", async (importOriginal) => {
  const client = { socket: {} };
  const connection = { connected: true };
  return {
    ...(await importOriginal<typeof import("~/lib/store-hooks.ts")>()),
    useClient: () => client,
    useConnection: () => connection,
  };
});
vi.mock("~/lib/viewer.ts", () => {
  const viewer = { handle: "", kind: "human", name: "" };
  return { useViewer: () => viewer };
});
vi.mock("~/lib/artifacts/people.ts", () => {
  const people = { pagePeople: [], pagePerson: () => ({}), seatName: (s: string) => s };
  return { useArtifactPeople: () => people };
});

const detail = {
  id: "0192f2c4-3333-7000-8000-000000000003",
  kind: "html",
  title: "Retro board",
  head_number: 2,
  capabilities: [],
  role: "owner",
} as unknown as ArtifactDetail;

const room = {
  state: { kind: "joined", role: "owner", session: "s-reader" },
  roster: { members: new Map(), counts: null, truncated: false },
} as never;

function mount(className?: string) {
  return render(
    <FrameView
      detail={detail}
      version={2}
      versions={[]}
      room={room}
      presence={{ at: () => {} }}
      bridgeStores={new Set()}
      className={className}
    />,
  );
}

beforeEach(() => {
  vi.mocked(mintFrame).mockReset();
});
afterEach(cleanup);

test("shows the frame's own placeholder, named for the page, while its ticket is minted", () => {
  vi.mocked(mintFrame).mockReturnValue(new Promise(() => {}));
  const { container } = mount("design-board-frame");
  const region = screen.getByRole("region", { name: "Retro board" });
  expect(region.getAttribute("aria-busy")).toBe("true");
  expect(region.classList.contains("artifact-frame")).toBe(true);
  expect(screen.getByRole("status").textContent).toBe("Loading Retro board");
  // The caller's box holds every state, not only the drawn page.
  expect(container.firstElementChild?.className).toBe("artifact-frame-box design-board-frame");
});

test("says a refused ticket as the frame says a failed page, and tries again", async () => {
  vi.mocked(mintFrame)
    .mockRejectedValueOnce(new RestError(500, { error: "internal", detail: "" }))
    .mockReturnValue(new Promise(() => {}));
  const { container } = mount("design-board-frame");
  expect(await screen.findByText("This page could not be opened")).toBeDefined();
  expect(
    screen.getByText("The engine could not open this page. Try again in a moment."),
  ).toBeDefined();
  expect(container.firstElementChild?.className).toBe("artifact-frame-box design-board-frame");
  expect(container.querySelector(".crewlet-sandboxed-frame__end")).not.toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  await waitFor(() => expect(mintFrame).toHaveBeenCalledTimes(2));
});

test("a refusal asking again cannot change offers no Try again", async () => {
  vi.mocked(mintFrame).mockRejectedValue(
    new RestError(503, { error: "artifact_origin_unset", detail: "" }),
  );
  mount();
  expect(await screen.findByText(/not set up to show pages yet/)).toBeDefined();
  expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
});

test("an engine nobody heard from is said in the reader's words, not the transport's", async () => {
  vi.mocked(mintFrame).mockRejectedValue(
    new RestError(0, {
      error: "unreachable",
      detail: "the engine did not answer within 30 seconds",
    }),
  );
  mount();
  expect(
    await screen.findByText(
      "The engine could not be reached. Check your connection, then try again.",
    ),
  ).toBeDefined();
  expect(screen.queryByText(/did not answer within/)).toBeNull();
});
