/**
 * Reviewing a server's changed tools: what moved, the suspicious direction
 * said first, and each decision sent by the digest of the definition reviewed.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { RestResult } from "~/lib/useRest.ts";
import { TestClient } from "~/test/client.tsx";
import { reviewChanges, type ServerDetail } from "../api.ts";
import { LINEAR_CHANGES, LINEAR_DETAIL, mcpData } from "../fixtures.ts";
import { DriftDialog } from "./DriftDialog.tsx";

let detail: ServerDetail = { ...LINEAR_DETAIL, changes: LINEAR_CHANGES };
const reload = vi.fn(async () => {});
vi.mock("../data.ts", async (actual) => ({
  ...(await actual<typeof import("../data.ts")>()),
  useServerDetail: (): RestResult<ServerDetail> => ({
    data: detail,
    error: null,
    code: null,
    refusal: null,
    loading: false,
    reload,
  }),
}));
vi.mock("../api.ts", async (actual) => ({
  ...(await actual<typeof import("../api.ts")>()),
  reviewChanges: vi.fn(),
}));

const onDone = vi.fn();
const onClose = vi.fn();

function mount() {
  const server = mcpData("admin").server("linear")!;
  render(
    <TestClient>
      <ToastProvider>
        <LayerHost>
          <DriftDialog server={server} onClose={onClose} onDone={onDone} />
        </LayerHost>
      </ToastProvider>
    </TestClient>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

beforeEach(() => {
  detail = { ...LINEAR_DETAIL, changes: LINEAR_CHANGES };
  vi.mocked(reviewChanges).mockReset();
  onDone.mockReset();
  onClose.mockReset();
  reload.mockClear();
});

afterEach(() => cleanup());

// THE SUSPICIOUS DIRECTION IS SAID: a change that claims less risk is shown
// first, flagged, with its hints before and after.
test("a change that claims less risk is shown first, flagged, with what moved", () => {
  mount();
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("It now claims to be less risky")).toBeDefined();
  expect(within(dialog).getByText("Description")).toBeDefined();
  expect(within(dialog).getByText("Hints")).toBeDefined();
  expect(within(dialog).getAllByText("Destructive").length).toBeGreaterThan(0);
  expect(within(dialog).getByText(/the tool it holds asks for approval/)).toBeDefined();
});

// A DECISION NAMES THE DEFINITION REVIEWED: accepting sends the change's
// `after.digest` by the server's own name for the tool, keeping it blocked
// the same, and a removal is accepted by null. Mutation: send the accepted
// digest, or the prefixed name, and this is red.
test("each decision is sent by the digest of the definition reviewed", async () => {
  vi.mocked(reviewChanges).mockResolvedValue({
    kind: "ok",
    status: 200,
    value: { server: "linear", accepted: ["delete_issue"], blocked: [], moved: [], revision: null },
  });
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept change" }));
  await settle();
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { accept: { delete_issue: "c".repeat(64) } },
    "Accepted the change to delete_issue on linear",
  );
  expect(onDone).toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Keep blocked" }));
  await settle();
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { block: { delete_issue: "c".repeat(64) } },
    "Kept delete_issue on linear blocked",
  );
  fireEvent.click(screen.getByRole("button", { name: /export_board/ }));
  expect(screen.queryByRole("button", { name: "Keep blocked" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Accept that it went" }));
  await settle();
  expect(reviewChanges).toHaveBeenLastCalledWith(
    "linear",
    { accept: { export_board: null } },
    "Accepted the change to export_board on linear",
  );
});

// A DEFINITION SWAPPED SINCE THE REVIEW is refused, said, and read again.
test("a decision refused as stale says so and reads the changes again", async () => {
  vi.mocked(reviewChanges).mockResolvedValue({
    kind: "refused",
    status: 409,
    code: "stale_version",
    sentence: "delete_issue changed again since it was reviewed; nothing was written.",
    body: { tools: ["delete_issue"] },
  });
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Accept change" }));
  await settle();
  expect(screen.getAllByText(/changed again since it was reviewed/).length).toBeGreaterThan(0);
  expect(reload).toHaveBeenCalled();
  expect(onDone).not.toHaveBeenCalled();
});

test("a server with nothing changed says so", () => {
  detail = { ...LINEAR_DETAIL, changes: [] };
  mount();
  expect(screen.getByText("No changes to review")).toBeDefined();
});
