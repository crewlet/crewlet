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

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
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
  expect(state.tag).toBe("Not checked");
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
test("absent, paused and unchecked are told apart", () => {
  // NO TAG at all: the Connect button beside it is the whole message, and a
  // chip saying "not connected" on every unconfigured row reads as a fault
  // list rather than a catalogue.
  expect(rollUp(slack, rowsOf()).tag).toBe("");
  expect(rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: false })).tag).toBe(
    "Paused",
  );
  // Configured with no pass behind it is NOT "connected": there is no
  // measured claim, and inventing one is the whole failure this screen is
  // built to avoid.
  expect(rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: true })).tag).toBe(
    "Not checked",
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

// THE CARD LEADS WITH THE TOOL, and the plumbing is behind a disclosure that
// starts closed: an operator scanning six integrations wants six names and
// six states, not six paragraphs. Opening it shows the counts, the paths and
// what the loop found, per surface.
test("a configured tool discloses its surfaces", () => {
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
  // Closed to begin with: nothing from the body is on screen.
  expect(screen.queryByText("/webhooks/jira")).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getByText("Jira")).toBeTruthy();
  expect(screen.getByText("Confluence")).toBeTruthy();
  // The inbound PATH, which is the address to paste at the vendor. The raw
  // in / dropped / merged counters that used to sit here are gone: all three
  // read zero on a healthy surface, so the line cost a row of jargon to say
  // nothing.
  expect(screen.getByText("/webhooks/jira")).toBeTruthy();
  expect(screen.queryByText(/in: 4/)).toBeNull();
});

// A TOOL NOBODY SET UP HAS NOTHING TO DISCLOSE, so it gets no disclosure: a
// chevron that opens an empty box is a control that lies.
test("an absent tool is a plain card with no disclosure and no badge", () => {
  const { container } = render(<EntryRow entry={slack} rows={rowsOf()} />);
  // NO STATUS BADGE. Nothing has been configured, so there is nothing to
  // report; the Connect button is the whole message.
  expect(container.querySelector(".badge")).toBeNull();
  expect(screen.queryByRole("button", { name: /details/i })).toBeNull();
  // And it still says what the tool is for, because the catalogue is what
  // tells a reader the engine serves it at all.
  expect(screen.getByText("Team communication")).toBeTruthy();
});

// CONNECTED FIRST. An operator comes here to finish something, so what is
// already working belongs above the catalogue of what is not.
test("a set-up tool sorts above one that is not", () => {
  const rows = rowsOf({ key: "datadog", configured: true });
  const order = [...CATALOG].sort((a, b) => Number(isSetUp(b, rows)) - Number(isSetUp(a, rows)));
  expect(order[0]?.key).toBe("datadog");
});

// A READY PHASE DOES NOT SILENCE A REFUSED WEBHOOK SECRET.
//
// The loop says nothing about ingress at all: the Jira and GitHub passes run
// with no webhook base, and secret_usable is computed separately from what
// the process resolved. So the two answer different questions, and a row that
// stopped at the phase put a green tag over a surface nothing could reach.
test("a ready tool still reports a refused secret", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "github")!,
    rowsOf({
      key: "github",
      configured: true,
      secret_usable: false,
      reconcile: { phase: "ready" },
    }),
  );
  expect(state.tag).toBe("ready");
  expect(state.status).toMatch(/^the webhook secret did not resolve/);
  expect(state.attention).toBe(true);
});

// And an unrouted surface, the other way a configured tool is silently not
// working, is reported on a ready phase too.
test("a ready tool still reports that nothing routes", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "datadog")!,
    rowsOf({ key: "datadog", configured: true, routes: false, reconcile: { phase: "ready" } }),
  );
  expect(state.status).toMatch(/nothing routes them to a seat$/);
  expect(state.attention).toBe(true);
});

// ATTENTION MEANS A PERSON IS NEEDED, which is the actor's question, not the
// phase's. Amber beside a neutral tag told an operator to act while the
// engine was still working.
test("only a phase a person owes draws attention", () => {
  const owed = ["admin", "operator"];
  const notOwed = ["engine", "provider"];
  for (const actor of owed) {
    const state = rollUp(
      atlassian,
      rowsOf({
        key: "jira",
        configured: true,
        reconcile: { phase: "degraded", actor, detail: "x" },
      }),
    );
    expect([actor, state.attention]).toEqual([actor, true]);
  }
  for (const actor of notOwed) {
    const state = rollUp(
      atlassian,
      rowsOf({
        key: "jira",
        configured: true,
        reconcile: { phase: "activating", actor, detail: "Atlassian is applying it" },
      }),
    );
    expect([actor, state.attention]).toEqual([actor, false]);
    // The line is still shown; it is just not dressed as a problem.
    expect(state.status).toBe("Jira: Atlassian is applying it");
  }
});

// THE LEAST READY SURFACE IS THE ENGINE'S OWN ORDER, integration.Phases: a
// degraded integration is still working and one still coming up is not, so
// activating outranks degraded rather than the reverse.
test("the phase order is the engine's", () => {
  const state = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "degraded", detail: "partly" } },
      {
        key: "confluence",
        configured: true,
        reconcile: { phase: "activating", detail: "coming up" },
      },
    ),
  );
  expect(state.tag).toBe("activating");
  expect(state.status).toBe("Confluence: coming up");
});

// --- the action slot -------------------------------------------------------- //

import { actionFor, isSetUp, sectionsFor } from "./Integrations.tsx";
import type { SetupToolState } from "~/protocol/types.ts";

function toolState(over: Partial<SetupToolState>): SetupToolState {
  return {
    key: "github",
    configured: true,
    enabled: true,
    satisfied: true,
    requirements: [],
    ...over,
  };
}

// THE ACTION IS A PURE FUNCTION of what the engine says, and of the same
// inputs the state tag beside it is derived from. Two derivations would
// eventually tell an operator two different things in one row.
test("the action follows the state", () => {
  const ready = rollUp(atlassian, rowsOf({ key: "jira", configured: true }));
  expect(actionFor(ready, [toolState({ configured: false })])?.label).toBe("Connect");
  expect(actionFor(ready, [toolState({ satisfied: false })])?.label).toBe("Continue");
  expect(actionFor(ready, [toolState({})])?.label).toBe("Manage");

  // A row a person owes something on offers Fix, narrowed to the fields
  // that clear what the loop found.
  const owed = rollUp(
    atlassian,
    rowsOf({
      key: "jira",
      configured: true,
      reconcile: { phase: "degraded", actor: "admin", detail: "x" },
    }),
  );
  const fix = actionFor(owed, [toolState({})]);
  expect(fix?.label).toBe("Fix");
  expect(fix?.blocks).toBe("credential_missing");
});

// A TOOL IS COMPLETE ONLY WHEN EVERY CONFIGURED SURFACE IS. Atlassian with
// Jira set up and Confluence half done is neither Connect nor Manage: it is a
// tool with something left to do, and reading only the first surface would
// have called it finished.
test("one unfinished surface makes the whole tool unfinished", () => {
  const ready = rollUp(atlassian, rowsOf({ key: "jira", configured: true }));
  const mixed = actionFor(ready, [
    toolState({ key: "jira", configured: true, satisfied: true }),
    toolState({ key: "confluence", configured: true, satisfied: false }),
  ]);
  expect(mixed?.label).toBe("Continue");

  // And a surface nobody has configured does not make the tool unfinished:
  // a company using Jira and not Confluence is not half broken.
  const partial = actionFor(ready, [
    toolState({ key: "jira", configured: true, satisfied: true }),
    toolState({ key: "confluence", configured: false, satisfied: false }),
  ]);
  expect(partial?.label).toBe("Manage");
});

// A tool this build knows nothing about offers nothing: a button that
// discovers on a press that there is no surface behind it is worse than none.
test("a tool with no setup surface offers no action", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true }));
  expect(actionFor(state, [])).toBeNull();
});

// --- a per-seat vendor ------------------------------------------------------ //

// SLACK GETS A SECTION PER SEAT, because each agent has its own app and so its
// own bot token. A seat with no app yet is exactly the seat somebody opened
// this dialog to give one to, so it is listed too.
test("a per-seat vendor becomes one section per agent", () => {
  const slackTool = toolState({
    key: "slack",
    configured: true,
    requirements: [],
    seats: [
      { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
      { handle: "cto", name: "CTO", requirements: [], satisfied: false },
    ],
  });
  const sections = sectionsFor(slack, new Map([["slack", slackTool]]));
  expect(sections.map((s) => s.seat)).toEqual(["sre-lead", "cto"]);
  expect(sections.map((s) => s.name)).toEqual(["SRE Lead", "CTO"]);
});

// A company whose Slack block exists and whose seats hold no app is not
// connected: nothing can post.
test("a per-seat vendor with no seat set up is unfinished", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true }));
  const action = actionFor(state, [
    toolState({
      key: "slack",
      configured: true,
      satisfied: true,
      seats: [{ handle: "cto", requirements: [], satisfied: false }],
    }),
  ]);
  expect(action?.label).toBe("Continue");
});

// --- where a per-surface control lives -------------------------------------- //

// A PASS BELONGS TO A SURFACE, so its button sits in that surface's row
// rather than in the card's header. Putting them in the header made Atlassian
// the only card in the list with four controls in it, purely because it is
// the only tool with more than one provisionable surface, and nothing on
// screen explained why.
test("a per-surface pass sits in that surface's row, not the header", () => {
  const passes: string[] = [];
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true })}
      sections={[
        { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
        { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
      ]}
      onPass={(tool) => passes.push(tool.key)}
    />,
  );

  // Closed, the header offers no pass at all.
  expect(screen.queryByRole("button", { name: "Run setup" })).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  const runs = screen.getAllByRole("button", { name: "Run setup" });
  expect(runs.length).toBe(2);
  expect(screen.getAllByRole("button", { name: "Recheck" }).length).toBe(2);

  // And each one runs ITS OWN surface. A single handler for the card would
  // have provisioned Jira from the Confluence row.
  fireEvent.click(runs[1]!);
  expect(passes).toEqual(["confluence"]);
});

// A surface this build cannot provision offers nothing: a button that
// discovers on a press that there is nothing behind it is worse than none.
test("a surface with no pass behind it gets no button", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "datadog")!}
      rows={rowsOf({ key: "datadog", configured: true })}
      sections={[{ name: "Datadog", tool: toolState({ key: "datadog", can_provision: false }) }]}
      onPass={() => {}}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Datadog details/ }));
  expect(screen.queryByRole("button", { name: "Run setup" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Recheck" })).toBeNull();
});

// A DROP IS A PROBLEM, so it survives the counters being removed.
//
// The three counters were dropped from the surface line because all three
// read zero on a healthy surface. One of them carried a real fault: a
// delivery this engine verified and then routed nowhere means whatever sent
// it is reaching the engine and reaching no agent. That becomes a sentence,
// beside the reconcile findings, rather than a number to interpret.
test("a dropped delivery is stated, not counted", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "github")!}
      rows={rowsOf({ key: "github", configured: true, inbound: 12, skipped: 3 })}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show GitHub details/ }));
  expect(screen.getByText(/3 deliveries were verified and dropped/)).toBeTruthy();
});

// And a surface dropping nothing says nothing, which is the whole reason the
// counters went: a zero is not news.
test("a surface dropping nothing carries no note about it", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "github")!}
      rows={rowsOf({ key: "github", configured: true, inbound: 12, skipped: 0 })}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show GitHub details/ }));
  expect(screen.queryByText(/verified and dropped/)).toBeNull();
});

// --- disconnecting ---------------------------------------------------------- //

// A TOOL NOBODY CONFIGURED HAS NOTHING TO DISCONNECT FROM, so it offers no
// control for it. Otherwise a catalogue of six unconfigured vendors would
// carry six buttons that undo nothing.
test("an unconfigured tool offers no disconnect", () => {
  render(<EntryRow entry={slack} rows={rowsOf()} onDisconnect={() => {}} />);
  expect(screen.queryByRole("button", { name: "Disconnect" })).toBeNull();
});

// And a connected one does, beside its own action rather than hidden in the
// body: taking an integration away is a decision about the whole tool.
test("a connected tool offers a disconnect", () => {
  const asked: string[] = [];
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "github")!}
      rows={rowsOf({ key: "github", configured: true })}
      onDisconnect={() => asked.push("github")}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  expect(asked).toEqual(["github"]);
});

// A DISCONNECT SHOWS THE MOMENT IT IS ASKED FOR, not when the first teardown
// pass happens to run.
//
// Between the request and that pass the stored phase is still whatever the
// last reconcile concluded — usually ready — so a screen reading the phase
// alone showed a connected integration somebody had already asked to remove,
// and they pressed the button again. Datadog showed nothing at all, because
// it has no pass to write a phase and the row carried none.
test("a disconnect that has been asked for reads as Disconnecting", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "datadog")!,
    rowsOf({
      key: "datadog",
      configured: true,
      // What the engine sends between the ask and the first pass: the
      // intent set, and the phase still the last reconcile's.
      reconcile: { phase: "ready", disconnecting: true },
    }),
  );
  expect(state.tag).toBe("Disconnecting");
  // NOT attention: the engine is doing it and nobody is owed anything.
  expect(state.attention).toBe(false);
});

// And it outranks a surface that is broken, because a tool being removed is
// not a tool anybody should be sent to fix.
test("a teardown outranks a failing surface", () => {
  const state = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "unconfigured" } },
      { key: "confluence", configured: true, reconcile: { phase: "ready", disconnecting: true } },
    ),
  );
  expect(state.tag).toBe("Disconnecting");
});
