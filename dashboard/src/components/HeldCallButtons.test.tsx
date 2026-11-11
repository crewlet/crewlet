import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { HeldCallButtons } from "./HeldCallButtons.tsx";
import { answerHeldCall, readHeldCall } from "~/lib/heldCall.ts";

vi.mock("~/lib/heldCall.ts", async (actual) => ({
  ...(await actual<typeof import("~/lib/heldCall.ts")>()),
  answerHeldCall: vi.fn(async () => ({ outcome: "pending" })),
  readHeldCall: vi.fn(async () => ({
    id: "h1",
    question: "May sre run a command on api-1 as crewlet-ops?",
    arguments: { host: "api-1", command: "sudo sh -s", stdin: "echo one\necho two\n" },
    arguments_sha256: "ab".repeat(32),
  })),
}));

vi.mock("~/lib/useWriteAccess.ts", () => ({
  useAnswerHold: () => "Only the seat's lead answers this.",
}));

afterEach(cleanup);

// AN OPERATE CALL IS ALLOWED ONCE ONLY AFTER IT IS READ WHOLE: it offers no
// Always allow, its approvers are decided by the engine rather than held here,
// and Allow once opens the whole call and answers with the digest it showed.
test("an operate call is allowed once only after it is read whole", async () => {
  render(
    <ToastProvider>
      <HeldCallButtons id="h1" seat="sre" fixed approvers="ssh" />
    </ToastProvider>,
  );
  expect(screen.queryByText("Always allow")).toBeNull();
  const review = screen.getByText("Review and allow once").closest("button");
  expect(review?.disabled || review?.getAttribute("aria-disabled") === "true").toBe(false);
  fireEvent.click(review as HTMLButtonElement);
  await waitFor(() => expect(readHeldCall).toHaveBeenCalledWith("h1"));
  await screen.findByText(/echo two/);
  fireEvent.click(screen.getByRole("button", { name: "Allow once" }));
  await waitFor(() => expect(answerHeldCall).toHaveBeenCalledWith("h1", "once", "ab".repeat(32)));
});

// ANY OTHER HELD CALL KEEPS ITS THREE ANSWERS, held to the seat's rule.
test("any other held call keeps its three answers", () => {
  render(
    <ToastProvider>
      <HeldCallButtons id="h2" seat="swe" />
    </ToastProvider>,
  );
  expect(screen.getByText("Always allow")).toBeTruthy();
  const once = screen.getByText("Allow once").closest("button");
  expect(once?.disabled || once?.getAttribute("aria-disabled") === "true").toBe(true);
});

// A MERGE IS REVIEWED AS ITS REQUEST: read whole like any fixed call and
// answered with the digest shown, its values drawn as text rather than JSON.
// Mutation: draw a merge's arguments through the JSON block, and this is red.
test("a merge's review reads as the merge, and Allow once names what was shown", async () => {
  vi.mocked(readHeldCall).mockResolvedValueOnce({
    id: "h3",
    server: "crewlet",
    tool: "ask_to_merge",
    seat: "sre",
    turn_id: "t-3",
    launch_id: "l-3",
    question: "May sre merge web!42 at 4f1c2a9b7e3d?",
    context: "",
    arguments: {
      change: "web!42",
      head: "4f1c2a9b7e3d5f60718293a4b5c6d7e8f9012345",
      method: "squash",
      reason: "**urgent** [see](https://evil.example)",
    },
    arguments_sha256: "cd".repeat(32),
  });
  render(
    <ToastProvider>
      <HeldCallButtons id="h3" seat="sre" fixed approvers="release" act="pr_merge" />
    </ToastProvider>,
  );
  fireEvent.click(screen.getByText("Review and allow once").closest("button") as HTMLElement);
  const dialog = await screen.findByRole("dialog", { name: "Review the merge" });
  expect(within(dialog).getByText("One squashed commit")).toBeTruthy();
  expect(within(dialog).getByText("**urgent** [see](https://evil.example)")).toBeTruthy();
  expect(dialog.querySelector("a, strong, pre")).toBeNull();
  fireEvent.click(within(dialog).getByRole("button", { name: "Allow once" }));
  await waitFor(() => expect(answerHeldCall).toHaveBeenCalledWith("h3", "once", "cd".repeat(32)));
});
