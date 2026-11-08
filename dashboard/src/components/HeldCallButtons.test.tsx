import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
