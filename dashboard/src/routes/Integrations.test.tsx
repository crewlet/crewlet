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
import { CATALOG, EntryRow, Reconcile, phaseTone, rollUp } from "./Integrations.tsx";
import type { IntegrationRow } from "~/protocol/types.ts";

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

// --- one row per tool ----------------------------------------------------- //

const atlassian = CATALOG.find((e) => e.key === "atlassian")!;
const slack = CATALOG.find((e) => e.key === "slack")!;

function rowsOf(...rows: IntegrationRow[]): Map<string, IntegrationRow> {
  return new Map(rows.map((r) => [r.key, r]));
}

// A TOOL IS THE LEAST READY OF ITS SURFACES. Atlassian is one row over three
// engine surfaces, and a row that reported the first surface it found would
// call the tool ready while its Jira was refusing every credential.
test("a tool reports its least ready surface, and names it", () => {
  const state = rollUp(
    atlassian,
    rowsOf(
      { key: "confluence", configured: true, reconcile: { phase: "ready" } },
      {
        key: "jira",
        configured: true,
        reconcile: { phase: "degraded", actor: "admin", detail: "swe has no Jira account" },
      },
    ),
  );
  expect(state.tag).toBe("degraded");
  expect(state.tone).toBe("caution");
  expect(state.status).toBe("Jira: swe has no Jira account");
  expect(state.attention).toBe(true);
});

// A tool with one surface does not prefix its own name: "Slack: ..." on the
// Slack row says nothing.
test("a single-surface tool does not name itself", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true, secret_usable: false }));
  expect(state.tag).toBe("configured");
  expect(state.status).toMatch(/^the webhook secret did not resolve/);
  expect(state.attention).toBe(true);
});

// A ready tool says nothing under its name. The tag is the claim, and a
// status line repeating "ready" would be the invented reassurance this
// screen refuses to show.
test("a ready tool has no status line", () => {
  const state = rollUp(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready", detail: "3 seats" } }),
  );
  expect(state.tag).toBe("ready");
  expect(state.status).toBeUndefined();
  expect(state.attention).toBe(false);
});

// NOT CONFIGURED, PAUSED and CONFIGURED are three different facts. Absent is
// nobody set it up, paused is somebody switched it off on purpose, and the
// two used to collapse into the state most likely to be mistaken for a
// mistake.
test("absent, paused and configured are told apart", () => {
  expect(rollUp(slack, rowsOf()).tag).toBe("not configured");
  expect(rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: false })).tag).toBe(
    "paused",
  );
  expect(rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: true })).tag).toBe(
    "configured",
  );
});

// A phase a newer node wrote outranks ready and is outranked by every phase
// this build knows is broken, so it never reads as healthy and never hides a
// real problem behind it.
test("an unknown phase is never the tool's ready state", () => {
  const unknown = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "ready" } },
      { key: "confluence", configured: true, reconcile: { phase: "something_new" } },
    ),
  );
  expect(unknown.tag).toBe("something new");
  expect(unknown.tone).not.toBe("positive");

  const broken = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "degraded", detail: "x" } },
      { key: "confluence", configured: true, reconcile: { phase: "something_new" } },
    ),
  );
  expect(broken.tag).toBe("degraded");
});

// THE DETAILS ARE STILL THERE. The row leads with the tool, and the counts,
// the inbound path and the findings sit under a disclosure rather than
// disappearing: an operator who needs to know why can open it.
test("a configured tool folds its surfaces under a disclosure", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        { key: "jira", configured: true, inbound: 4, inbound_path: "/webhooks/jira" },
        { key: "confluence", configured: true, inbound: 1 },
      )}
    />,
  );
  expect(screen.getByText("Atlassian")).toBeTruthy();
  expect(screen.getByText("Details")).toBeTruthy();
  expect(screen.getByText("Jira")).toBeTruthy();
  expect(screen.getByText("Confluence")).toBeTruthy();
  expect(screen.getByText("/webhooks/jira")).toBeTruthy();
  expect(screen.getByText(/in: 4/)).toBeTruthy();
});

// A tool nobody set up has no details to open, and says so in its tag.
test("an absent tool has no disclosure", () => {
  render(<EntryRow entry={slack} rows={rowsOf()} />);
  expect(screen.getByText("not configured")).toBeTruthy();
  expect(screen.queryByText("Details")).toBeNull();
});
