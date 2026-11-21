/**
 * A held growth call is decided on its card: Allow once names the digest of
 * what was shown, either answer carries the person's note, and a refused
 * answer is said on the card while the call still waits.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import type { DelegatedAct } from "~/contract/delegated.ts";
import type { GrowthCard } from "~/contract/growth.ts";
import { HeldCallButtons } from "~/components/HeldCallButtons.tsx";
import { answerHeldCall, readHeldCall } from "~/lib/heldCall.ts";
import { GrowthDecision, GrowthDecisionReview } from "./GrowthDecisionReview.tsx";

const CARD: GrowthCard = {
  act: "ad_spend",
  product: "google_ads",
  tool: "google_ads_raise_budget",
  account: {
    product: "google_ads",
    account: "123-456-7890",
    account_name: "Nimbus Ads",
    currency: "EUR",
    time_zone: "Europe/Berlin",
    state: "serving",
  },
  acts: [
    {
      target: "Spring sale",
      summary: "Raise the daily budget",
      changes: [
        {
          field: "amount_micros",
          label: "Daily budget",
          before: "40000000",
          after: "60000000",
          money: true,
        },
      ],
    },
  ],
  money: null,
  findings: [],
  reason: "Spent out by noon.",
  lapses_at: new Date(Date.now() + 3_600_000).toISOString(),
  moved: false,
  other_open_asks: 0,
};

const WHOLE = {
  id: "h1",
  server: "crewlet",
  tool: "google_ads_raise_budget",
  seat: "growth",
  turn_id: "t",
  launch_id: "l",
  question: "Raise the daily budget of Spring sale?",
  context: "",
  arguments: { campaign: "1", amount: "60.00" },
  arguments_sha256: "ef".repeat(32),
  card: CARD as GrowthCard | null,
};

vi.mock("~/lib/heldCall.ts", async (actual) => ({
  ...(await actual<typeof import("~/lib/heldCall.ts")>()),
  answerHeldCall: vi.fn(async () => ({ outcome: "pending" })),
  readHeldCall: vi.fn(async () => WHOLE),
}));

vi.mock("~/lib/useWriteAccess.ts", () => ({ useAnswerHold: () => undefined }));

afterEach(() => {
  cleanup();
  vi.mocked(answerHeldCall).mockClear();
  vi.mocked(readHeldCall).mockClear();
});

test("allow once names the digest of what was shown, and carries the note back to the agent", async () => {
  const onAnswered = vi.fn();
  render(<GrowthDecisionReview id="h1" onClose={() => {}} onAnswered={onAnswered} />);
  await screen.findByText("Raise the daily budget of Spring sale?");
  fireEvent.change(screen.getByRole("textbox", { name: /Note to the agent/ }), {
    target: { value: "Once only." },
  });
  await new Promise((resolve) => setTimeout(resolve, 600)); // the primary arms after it appears
  fireEvent.click(screen.getByRole("button", { name: "Allow once" }));
  await waitFor(() =>
    expect(answerHeldCall).toHaveBeenCalledWith("h1", "once", "ef".repeat(32), "Once only."),
  );
  expect(onAnswered).toHaveBeenCalledWith(expect.stringMatching(/^Allowed once/), true);
});

test("deny names no digest and still carries the note", async () => {
  render(<GrowthDecisionReview id="h1" onClose={() => {}} onAnswered={() => {}} />);
  await screen.findByText("Raise the daily budget of Spring sale?");
  fireEvent.change(screen.getByRole("textbox", { name: /Note to the agent/ }), {
    target: { value: "Not this week." },
  });
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  await waitFor(() =>
    expect(answerHeldCall).toHaveBeenCalledWith("h1", "deny", undefined, "Not this week."),
  );
});

test("a refused answer is said on the card, which still waits", async () => {
  vi.mocked(answerHeldCall).mockRejectedValueOnce(
    new Error("The budget moved since this was asked."),
  );
  const onClose = vi.fn();
  render(<GrowthDecisionReview id="h1" onClose={onClose} onAnswered={() => {}} />);
  await screen.findByText("Raise the daily budget of Spring sale?");
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  expect(await screen.findByText(/The budget moved since this was asked/)).toBeTruthy();
  expect(onClose).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Deny" })).toBeTruthy();
});

test("a held growth call offers its card to decide on, never Always allow", async () => {
  render(
    <ToastProvider>
      <HeldCallButtons
        id="h1"
        seat="growth"
        fixed
        approvers="admin"
        act={"ad_spend" as DelegatedAct}
      />
    </ToastProvider>,
  );
  expect(screen.queryByRole("button", { name: "Always allow" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Review and decide" }));
  expect(await screen.findByText("Raise the daily budget of Spring sale?")).toBeTruthy();
  expect(readHeldCall).toHaveBeenCalledWith("h1");
});

test("a call whose card could not be read is decided on the call itself, Deny included", async () => {
  vi.mocked(readHeldCall).mockResolvedValueOnce({ ...WHOLE, card: null });
  render(<GrowthDecision id="h1" onAnswered={() => {}} />);
  expect(await screen.findByText(/this ask comes without its card/)).toBeTruthy();
  expect(screen.getByText("Raise the daily budget of Spring sale?")).toBeTruthy();
  // Every argument, whole: the code block colours the JSON in parts.
  expect(document.body.textContent).toContain('"amount": "60.00"');
  fireEvent.click(screen.getByRole("button", { name: "Allow once" }));
  await waitFor(() =>
    expect(answerHeldCall).toHaveBeenCalledWith("h1", "once", "ef".repeat(32), ""),
  );
  expect(screen.getByRole("button", { name: "Deny" })).toBeTruthy();
});

test("a denial on its way holds Allow once with what is happening, never Applying", async () => {
  let settle = () => {};
  vi.mocked(answerHeldCall).mockImplementationOnce(
    () => new Promise((resolve) => (settle = () => resolve({ outcome: "pending" } as never))),
  );
  render(<GrowthDecision id="h1" onAnswered={() => {}} />);
  await screen.findByText("Raise the daily budget of Spring sale?");
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  expect((await screen.findAllByText("Sending your denial.")).length).toBeGreaterThan(0);
  expect(screen.queryByText("Applying")).toBeNull();
  // A second press while the first is on its way sends nothing more.
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  expect(answerHeldCall).toHaveBeenCalledTimes(1);
  settle();
});

test("a refused answer reads the call again, so the card says where it stands", async () => {
  vi.mocked(answerHeldCall).mockRejectedValueOnce(new Error("The call moved."));
  vi.mocked(readHeldCall)
    .mockResolvedValueOnce(WHOLE)
    .mockResolvedValueOnce({ ...WHOLE, card: { ...CARD, moved: true } });
  render(<GrowthDecision id="h1" onAnswered={() => {}} />);
  await screen.findByText("Raise the daily budget of Spring sale?");
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  await waitFor(() => expect(readHeldCall).toHaveBeenCalledTimes(2));
  // A moved target runs nothing if allowed: Allow once is held with why, and Deny stays.
  expect((await screen.findAllByText(/moved since the agent asked/)).length).toBeGreaterThan(0);
  expect(screen.getByRole("button", { name: "Allow once" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  await waitFor(() => expect(answerHeldCall).toHaveBeenLastCalledWith("h1", "deny", undefined, ""));
});

test("a call that cannot be read again after a refusal keeps its card, and says both", async () => {
  vi.mocked(answerHeldCall).mockRejectedValueOnce(new Error("The call moved."));
  vi.mocked(readHeldCall)
    .mockResolvedValueOnce(WHOLE)
    .mockRejectedValueOnce(new Error("The runs could not be read; try again."));
  render(<GrowthDecision id="h1" onAnswered={() => {}} />);
  await screen.findByText("Raise the daily budget of Spring sale?");
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  expect(await screen.findByText(/The call moved\./)).toBeTruthy();
  expect(
    await screen.findByText(/could not be read again: The runs could not be read/),
  ).toBeTruthy();
  expect(screen.getByRole("button", { name: "Deny" })).toBeTruthy();
});

test("the next held call opens fresh, never with the last one's note", async () => {
  const { rerender } = render(<GrowthDecision id="h1" onAnswered={() => {}} />);
  await screen.findByText("Raise the daily budget of Spring sale?");
  fireEvent.change(screen.getByRole("textbox", { name: /Note to the agent/ }), {
    target: { value: "For the last one." },
  });
  rerender(<GrowthDecision id="h2" onAnswered={() => {}} />);
  await waitFor(() => expect(readHeldCall).toHaveBeenLastCalledWith("h2"));
  const note = await screen.findByRole("textbox", { name: /Note to the agent/ });
  expect((note as HTMLTextAreaElement).value).toBe("");
});
