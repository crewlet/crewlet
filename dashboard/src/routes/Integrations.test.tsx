/**
 * What the reconcile status may and may not claim.
 *
 * This screen's whole posture is that it never invents health: an idle
 * integration and a broken one look identical in the traffic counters, so a
 * green dot derived from anything but a real finding is reporting the weather.
 * The reconcile block is the first thing on the screen that CAN make a health
 * claim, which is exactly why what it does with an absent or unreadable
 * answer is worth pinning.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Reconcile, phaseTone } from "./Integrations.tsx";

afterEach(cleanup);

// NULL RENDERS NOTHING. It is either a process with no loop to ask or a
// surface the loop has not reached, and neither is a claim that the surface
// is fine. Anything drawn here would be the invented health this screen
// refuses to show.
test("a surface with no status renders nothing", () => {
  const { container } = render(<Reconcile status={null} />);
  expect(container.innerHTML).toBe("");

  const absent = render(<Reconcile status={undefined} />);
  expect(absent.container.innerHTML).toBe("");
});

// A phase a newer node wrote must never read as healthy. Claiming a surface
// is fine on the strength of a word this build cannot interpret is the one
// answer that is certainly wrong.
test("an unknown phase is never positive", () => {
  expect(phaseTone("something_a_newer_build_wrote")).not.toBe("positive");
  expect(phaseTone("")).not.toBe("positive");
  expect(phaseTone("ready")).toBe("positive");
});

// Degraded is caution rather than critical, and the distinction is real:
// agents are still working, which is what separates it from a surface that
// cannot authenticate at all.
test("the tone follows how much is not working", () => {
  expect(phaseTone("degraded")).toBe("caution");
  expect(phaseTone("unconfigured")).toBe("critical");
});

// The detail is the sentence that saves somebody reading logs, and the link
// is where they go to act on it.
test("a blocked surface says what to do and where", () => {
  render(
    <Reconcile
      status={{
        phase: "degraded",
        actor: "admin",
        detail: "swe has no Jira account, so no issue reaches it",
        action_url: "https://jira.example.com/admin",
        findings: [{ kind: "identity_failed", subject: "swe" }],
      }}
    />,
  );
  expect(screen.getByText(/swe has no Jira account/)).toBeTruthy();
  expect(screen.getByText(/you, at the vendor/)).toBeTruthy();
  expect(screen.getByRole("link").getAttribute("href")).toBe("https://jira.example.com/admin");
});

// THE FINDINGS BEHIND THE HEADLINE. The report says what to do next and the
// findings say what is actually wrong, so an operator who fixes the first
// should not wait a full pass to learn there were four more.
test("the findings the phase was not derived from are still reachable", () => {
  render(
    <Reconcile
      status={{
        phase: "degraded",
        actor: "admin",
        detail: "ceo needs maintainer on api-gateway",
        findings: [
          { kind: "grant_short", subject: "ceo", detail: "ceo needs maintainer" },
          { kind: "grant_excess", subject: "cto", detail: "cto holds owner on api-gateway" },
        ],
      }}
    />,
  );
  expect(screen.getByText(/1 more finding/)).toBeTruthy();
  expect(screen.getByText(/cto holds owner/)).toBeTruthy();
});

// A pass that could not read the surface is a FAULT, not a finding, and it is
// labelled as one rather than rendered as something about the vendor.
test("a failed pass is reported as a failed pass", () => {
  render(
    <Reconcile
      status={{ phase: "activating", actor: "engine", last_error: "502 from the instance" }}
    />,
  );
  expect(screen.getByText(/Last pass failed: 502 from the instance/)).toBeTruthy();
});
