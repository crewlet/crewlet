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
import {
  CATALOG,
  EntryRow,
  IN_FLIGHT,
  Reconcile,
  byConfiguredThenName,
  disconnectOrder,
  phaseTone,
  rollUp,
} from "./Integrations.tsx";
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
  // EVERY IN-PROGRESS PHASE IS AMBER. Each one is an integration that does
  // not work yet, and neutral is the tone this screen uses for a state
  // nobody needs to come back to.
  for (const phase of ["provisioning", "activating", "awaiting_admin", "disconnecting"]) {
    expect(phaseTone(phase)).toBe("caution");
  }
});

// The detail is the sentence that saves somebody reading logs, and it is the
// whole of what this band says.
//
// NEITHER AN ACTOR CLAUSE NOR A LINK BESIDE IT. "you, at the third-party app"
// named a place the sentence already names — and names better, because it
// says which app and what to do there — and the anchor under it read "Open
// where this is fixed", unlabelled, directly above the agent row's own
// "Install on GitHub" button pointing at the same address. Two controls for
// one act and a clause repeating the sentence above it.
test("a blocked surface says what to do", () => {
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
  expect(screen.queryByText(/at the third-party app/)).toBeNull();
  expect(screen.queryByRole("link")).toBeNull();
});

// AN OPERATOR'S OWN FINDING NAMES NO PLACE.
//
// It read "you, in the company configuration", which named a place this
// screen IS: the note sits inside the card whose Settings control opens the
// very form the fix is made in, so it sent a reader looking elsewhere for
// what was already in front of them. The finding's own sentence says what to
// change, and the admin's clause stays because the third-party app is
// genuinely somewhere else.
test("a finding the operator owns carries no place to go", () => {
  render(
    <Reconcile
      status={{
        phase: "unconfigured",
        actor: "operator",
        detail: "the webhook secret resolved to nothing",
      }}
    />,
  );
  expect(screen.getByText(/the webhook secret resolved to nothing/)).toBeTruthy();
  expect(screen.queryByText(/company configuration/)).toBeNull();
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
        // THE REPORT'S DETAIL IS THE WINNING FINDING'S. Classify copies it,
        // which is what makes matching by identity possible at all.
        detail: "ceo needs maintainer",
        findings: [
          { kind: "grant_short", subject: "ceo", detail: "ceo needs maintainer" },
          { kind: "grant_excess", subject: "cto", detail: "cto holds owner on api-gateway" },
        ],
      }}
    />,
  );
  expect(screen.getByText(/1 more finding/)).toBeTruthy();
  expect(screen.getByText(/cto holds owner/)).toBeTruthy();
  // AND THE HEADLINE IS NOT REPEATED under it.
  expect(screen.getAllByText(/ceo needs maintainer/)).toHaveLength(1);
});

// THE REPORTED FINDING IS NOT ALWAYS FIRST.
//
// This dropped element zero and rendered the tail, so whenever Classify's
// winner sat anywhere else a real finding was hidden and the headline was
// re-printed as "1 more finding". The engine promotes the winner now, but a
// row written by a peer on an older build carries the vendor's own order and
// a rolling upgrade puts exactly those rows on this screen.
test("a finding list in the vendor's own order still renders correctly", () => {
  render(
    <Reconcile
      status={{
        phase: "unconfigured",
        actor: "operator",
        detail: "the organization credential was refused",
        findings: [
          { kind: "grant_excess", subject: "ceo", detail: "ceo holds owner on api-gateway" },
          {
            kind: "credential_rejected",
            subject: "org",
            detail: "the organization credential was refused",
          },
        ],
      }}
    />,
  );
  expect(screen.getByText(/1 more finding/)).toBeTruthy();
  // The one the report did NOT summarise, which was the one being hidden.
  expect(screen.getByText(/ceo holds owner/)).toBeTruthy();
  expect(screen.getAllByText(/the organization credential was refused/)).toHaveLength(1);
});

// A COUNT SUFFIX IS NOT PART OF THE SENTENCE. Classify appends "(and 2 more)"
// when several findings share the winning kind, so a literal compare would
// never match and the headline would render twice.
test("a report counting its own kind still matches its finding", () => {
  render(
    <Reconcile
      status={{
        phase: "degraded",
        actor: "admin",
        detail: "ceo needs maintainer (and 1 more)",
        findings: [
          { kind: "grant_short", subject: "ceo", detail: "ceo needs maintainer" },
          { kind: "grant_short", subject: "cto", detail: "cto needs maintainer" },
        ],
      }}
    />,
  );
  expect(screen.getByText(/1 more finding/)).toBeTruthy();
  expect(screen.getByText(/cto needs maintainer/)).toBeTruthy();
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
});

// A SURFACE NOTHING CAN REACH IS NOT DRAWN AS HEALTHY, and the tag's colour
// is what says so on a collapsed card. Which surface, and why, is the note in
// the body: the header line is the tool's name, not its latest complaint.
test("a surface whose secret did not resolve colours the tag", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true, secret_usable: false }));
  expect(state.tag).toBe("Connecting");
  expect(state.tone).toBe("caution");
});

// A ready tool is drawn ready. The tag is the whole claim, and its tone must
// not be the caution reserved for something a reader has to act on.
test("a ready tool is drawn ready", () => {
  const state = rollUp(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready", detail: "3 seats" } }),
  );
  expect(state.tag).toBe("ready");
  expect(state.tone).not.toBe("caution");
});

// NOT CONFIGURED, PAUSED and CONFIGURED are three different facts. Absent is
// nobody set it up, paused is somebody switched it off on purpose, and the
// two used to collapse into the state most likely to be mistaken for a
// mistake.
test("absent, paused and connecting are told apart", () => {
  // NO TAG at all: the Connect button beside it is the whole message, and a
  // chip saying "not connected" on every unconfigured row reads as a fault
  // list rather than a catalogue.
  expect(rollUp(slack, rowsOf()).tag).toBe("");
  expect(rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: false })).tag).toBe(
    "Paused",
  );
  // Configured with no report behind it is NOT "connected": there is no
  // measured claim, and inventing one is the whole failure this screen is
  // built to avoid. It is the window between connecting and the loop's first
  // pass, and it says so.
  const connecting = rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: true }));
  expect(connecting.tag).toBe("Connecting");
  expect(connecting.tone).toBe("caution");
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

// A CARD'S BODY IS ITS AGENTS, not a restatement of its header.
//
// It opened with a row per surface — "Jira, /webhooks/jira, Connected" —
// under a header already saying Connected, so the first thing a reader saw
// on opening a card was the engine repeating the tag above it and naming a
// route nobody has to paste any more, now that the loop registers the hook.
test("a working surface adds nothing to the card", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        {
          key: "jira",
          configured: true,
          inbound: 4,
          inbound_path: "/webhooks/jira",
          reconcile: { phase: "ready" },
        },
        { key: "confluence", configured: true, inbound: 1, reconcile: { phase: "ready" } },
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.queryByText("/webhooks/jira")).toBeNull();
  expect(screen.queryByText("Jira")).toBeNull();
  expect(screen.queryByText("Confluence")).toBeNull();
});

// AND A BROKEN ONE STILL SAYS SO, AND SAYS WHICH. The header rolls up to the
// least ready surface, so a tool with two of them has two answers and only
// one fits in the tag.
test("a faulted surface says what is wrong and which surface it is", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        { key: "jira", configured: true, reconcile: { phase: "ready" } },
        {
          key: "confluence",
          configured: true,
          secret_usable: false,
          reconcile: { phase: "degraded", detail: "the webhook secret did not resolve" },
        },
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getByText("Confluence")).toBeTruthy();
  expect(screen.getByText("secret unresolved")).toBeTruthy();
  // And the surface that works is not listed beside it.
  expect(screen.queryByText("Jira")).toBeNull();
});

// THE LINE UNDER A TOOL'S NAME IS WHAT THE TOOL IS, never how it is doing.
//
// It carried the roll-up's status sentence in amber, so a card's identity was
// replaced by its latest complaint: a reader scanning the list for GitHub
// found a warning where the name's own line belongs, on every card that had
// anything to report. What is wrong belongs in the body, with the badge that
// names the surface, the loop's own sentence, and the link that fixes it.
test("a card's header says what the tool is, not what is wrong with it", () => {
  const rows = rowsOf({
    key: "github",
    configured: true,
    secret_usable: false,
    reconcile: {
      phase: "awaiting_admin",
      actor: "admin",
      detail: "sre-lead has no GitHub App of its own",
    },
  });
  render(<EntryRow entry={CATALOG.find((e) => e.key === "github")!} rows={rows} />);

  // THE CATALOGUE'S OWN LINE, and only it.
  expect(screen.getByText("Code and pull requests")).toBeTruthy();
  expect(screen.queryByText(/sre-lead has no GitHub App/)).toBeNull();

  // AND IT IS THERE ONCE THE CARD IS OPEN, which is where the question it
  // answers is actually asked.
  fireEvent.click(screen.getByRole("button", { name: /Show GitHub details/ }));
  expect(screen.getByText(/sre-lead has no GitHub App/)).toBeTruthy();
  expect(screen.getByText("secret unresolved")).toBeTruthy();
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

// A READY PHASE DOES NOT SILENCE A REFUSED WEBHOOK SECRET.
//
// The loop says nothing about ingress at all: the Jira and GitHub passes run
// with no webhook base, and secret_usable is computed separately from what
// the process resolved. So the two answer different questions, and a row that
// stopped at the phase put a green tag over a surface nothing could reach.
test("a ready tool with a refused secret is not drawn ready", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "github")!,
    rowsOf({
      key: "github",
      configured: true,
      secret_usable: false,
      reconcile: { phase: "ready" },
    }),
  );
  // CONNECTED IS ONLY EVER GREEN, so a card nothing can reach does not wear
  // the word at all. Drawn as the engine's word in a colour that disagreed
  // with it, it said "Connected" in amber: a word and a colour saying
  // opposite things, leaving a reader to work out which to believe.
  expect(state.tag).toBe("Action needed");
  expect(state.tone).toBe("caution");
});

// CONNECTED IS ONLY EVER GREEN. The rule, asserted over every way a card can
// arrive at that word: a tag saying the integration works, drawn in the
// colour this screen uses for something to come back to, is two claims in one
// chip and a reader has to guess which one is meant.
test("nothing draws Connected in a colour that disagrees with it", () => {
  const github = CATALOG.find((e) => e.key === "github")!;
  for (const secret_usable of [undefined, true, false]) {
    for (const routes of [undefined, true, false]) {
      const state = rollUp(
        github,
        rowsOf({
          key: "github",
          configured: true,
          secret_usable,
          routes,
          reconcile: { phase: "ready", phase_label: "Connected" },
        }),
      );
      if (state.tag === "Connected") expect(state.tone).toBe("positive");
      // And the card that cannot say Connected says what is true instead.
      if (secret_usable === false || routes === false) {
        expect(state.tag).toBe("Action needed");
      }
    }
  }
});

// AND THE ENGINE'S OWN WORD WINS WHEREVER IT IS MORE SPECIFIC.
//
// A degraded phase already says something is wrong and says which surface in
// the body; replacing it with the screen's blunter word would lose that. A
// phase the engine is working through is worse still: telling a person to act
// is asking them to interrupt it.
test("an ingress fault does not overwrite a phase that says more", () => {
  const github = CATALOG.find((e) => e.key === "github")!;
  for (const phase of ["degraded", "activating", "provisioning"]) {
    const state = rollUp(
      github,
      rowsOf({
        key: "github",
        configured: true,
        secret_usable: false,
        reconcile: { phase, phase_label: `label:${phase}` },
      }),
    );
    expect([phase, state.tag]).toEqual([phase, `label:${phase}`]);
  }
});

// And an unrouted surface, the other way a configured tool is silently not
// working, is read on a ready phase too.
test("a ready tool that routes nothing is not drawn ready", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "datadog")!,
    rowsOf({ key: "datadog", configured: true, routes: false, reconcile: { phase: "ready" } }),
  );
  expect(state.tag).toBe("Action needed");
  expect(state.tone).toBe("caution");
});

// A PHASE THE ENGINE OWNS IS NOT A PROBLEM, and its tone says so: marking
// `activating` amber told an operator to act while the engine was working.
test("a phase the engine is working through is not drawn as a fault", () => {
  const state = rollUp(
    atlassian,
    rowsOf({
      key: "jira",
      configured: true,
      reconcile: { phase: "activating", actor: "engine", detail: "Atlassian is applying it" },
    }),
  );
  expect(state.tone).not.toBe("critical");
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
});

// --- the action slot -------------------------------------------------------- //

import { actionFor, sectionsFor } from "./Integrations.tsx";
import type { Entry } from "./Integrations.tsx";
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
  const ready = rollUp(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready" } }),
  );
  // A TOOL NOBODY HAS CONNECTED: neither half has it, which is what makes
  // Connect the answer. Passing present=true here would be a card whose rows
  // hold a surface its listing has not caught up with, and that one offers
  // nothing until it does.
  expect(actionFor(ready, [toolState({ configured: false })], false)?.label).toBe("Connect");
  expect(actionFor(ready, [toolState({ satisfied: false })], true)?.label).toBe("Continue");
  // A WORKING TOOL OFFERS NOTHING. Nothing a person does moves it, and the
  // Manage button that used to sit here read as a card's primary action
  // while saying only "nobody is needed".
  expect(actionFor(ready, [toolState({})], true)).toBeNull();

  // A FAULT IS NOT AN ACTION. A row a person owes something on says so in
  // its tag and its status line; what to do about it is the settings the
  // gear opens. A Fix button beside Disconnect, appearing and disappearing
  // as an integration breaks and recovers, carried nothing the line above
  // it did not already say.
  const owed = rollUp(
    atlassian,
    rowsOf({
      key: "jira",
      configured: true,
      reconcile: { phase: "degraded", actor: "admin", detail: "x" },
    }),
  );
  expect(actionFor(owed, [toolState({})], true)).toBeNull();
});

// THE ROWS ARE THE FRESHER HALF, IN BOTH DIRECTIONS.
//
// The two halves of this screen arrive separately, and the rows are the
// quicker answer to "does this company have this". Read as fresher in only
// one direction, a disconnect left the card with no control at all: nothing
// to connect it, because the listing still called it configured, and nothing
// to disconnect, because the rows already said it was gone. It stayed that
// way until somebody refreshed the page by hand.
test("a card the rows say is gone offers to connect it again", () => {
  const github = CATALOG.find((e) => e.key === "github")!;
  const gone = rollUp(github, rowsOf());
  expect(gone.tag).toBe("");
  // The listing has not caught up and still calls it configured.
  expect(actionFor(gone, [toolState({ key: "github", configured: true })], false)?.label).toBe(
    "Connect",
  );

  // AND THE OTHER DIRECTION IS UNCHANGED: straight after a connect the rows
  // hold the surface while the listing is still the pre-connect one, and a
  // card then drew Connect beside a tag reading Connected.
  const fresh = rollUp(github, rowsOf({ key: "github", configured: true }));
  expect(actionFor(fresh, [toolState({ key: "github", configured: false })], true)).toBeNull();
});

// A CARD IN MOTION OFFERS NOTHING.
//
// Between a connect and the loop's first report, and between asking for a
// disconnect and its finishing, there is nothing a person does that moves the
// card: the engine is working. A Continue button beside "Connecting" invited
// somebody to act on a card whose state was about to change under them, and
// on a multi-surface tool it appeared the instant the first surface was
// saved, which is the moment the loop had least to say.
test("a card the engine is mid-flight on offers no action", () => {
  // CONNECTED AND UNREPORTED: the window after a save.
  const connecting = rollUp(atlassian, rowsOf({ key: "atlassian", configured: true }));
  expect(connecting.tag).toBe("Connecting");
  // Atlassian is three surfaces, so the other two are unconfigured and this
  // is exactly the card that drew Continue beside Connecting.
  expect(
    actionFor(
      connecting,
      [toolState({ key: "atlassian" }), toolState({ configured: false })],
      true,
    ),
  ).toBeNull();

  // AND BEING TAKEN AWAY, which is the other direction of the same rule.
  const going = rollUp(
    atlassian,
    rowsOf({
      key: "jira",
      configured: true,
      reconcile: { phase: "ready", disconnecting: true },
    }),
  );
  expect(actionFor(going, [toolState({ configured: false })], true)).toBeNull();
});

// A TOOL IS COMPLETE ONLY WHEN EVERY CONFIGURED SURFACE IS. Atlassian with
// Jira set up and Confluence half done is neither Connect nor finished: it is
// a tool with something left to do, and reading only the first surface would
// have called it done.
test("one unfinished surface makes the whole tool unfinished", () => {
  const ready = rollUp(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready" } }),
  );
  const mixed = actionFor(
    ready,
    [
      toolState({ key: "jira", configured: true, satisfied: true }),
      toolState({ key: "confluence", configured: true, satisfied: false }),
    ],
    true,
  );
  expect(mixed?.label).toBe("Continue");

  // AND A SURFACE NOBODY HAS CONFIGURED IS SOMETHING TO CONTINUE, not to
  // connect.
  //
  // It does not make the tool UNFINISHED — a company using Jira and not
  // Confluence is not half broken, and the tag says what the connected
  // surfaces are doing — but it is something a person can act on, and
  // connecting the organization alone left the Atlassian card with no
  // button at all: nothing on screen would add the products.
  //
  // The WORD matters: this said Connect, beside a tag reading Connected, so
  // the card claimed both at once. Continue is what this screen already says
  // about a tool with something left to do.
  const partial = actionFor(
    ready,
    [
      toolState({ key: "jira", configured: true, satisfied: true }),
      toolState({ key: "confluence", configured: false, satisfied: false }),
    ],
    true,
  );
  expect(partial?.label).toBe("Continue");

  // A tool whose every surface is connected and working offers nothing.
  const done = actionFor(
    ready,
    [
      toolState({ key: "jira", configured: true, satisfied: true }),
      toolState({ key: "confluence", configured: true, satisfied: true }),
    ],
    true,
  );
  expect(done).toBeNull();
});

// A tool this build knows nothing about offers nothing: a button that
// discovers on a press that there is no surface behind it is worse than none.
test("a tool with no setup surface offers no action", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true }));
  expect(actionFor(state, [], true)).toBeNull();
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
    // WHAT MAKES IT PER-SEAT. Every app carries a roster now; this flag is
    // the difference between a list of agents to read and a form per agent.
    seats_required: true,
    seats: [
      { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
      { handle: "cto", name: "CTO", requirements: [], satisfied: false },
    ],
  });
  const sections = sectionsFor(slack, new Map([["slack", slackTool]]));
  expect(sections.map((s) => s.seat)).toEqual(["sre-lead", "cto"]);
  expect(sections.map((s) => s.name)).toEqual(["SRE Lead", "CTO"]);
});

// AND AN INFORMATIONAL ROSTER DOES NOT. A Datadog seat holds a credential
// written in its mcp_env, which this dialog does not edit: splitting the form
// into one section per agent gave every one of them a copy of the company's
// own fields to fill in.
test("an informational roster stays one section", () => {
  const datadog = CATALOG.find((e) => e.key === "datadog")!;
  const tool = toolState({
    key: "datadog",
    configured: true,
    seats: [
      { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
      { handle: "cto", name: "CTO", requirements: [], satisfied: false },
    ],
  });
  const sections = sectionsFor(datadog, new Map([["datadog", tool]]));
  expect(sections.map((s) => s.name)).toEqual(["Datadog"]);
  expect(sections.map((s) => s.seat)).toEqual([undefined]);
});

// AND THE ROSTER IS COUNTED ONCE. A per-seat app contributes one section per
// agent, all carrying the same tool, so reading the seats off every section
// rendered each agent as many times as there were sections.
test("a roster is listed once however many sections carry it", () => {
  render(
    <EntryRow
      entry={slack}
      rows={rowsOf({ key: "slack", configured: true })}
      sections={sectionsFor(
        slack,
        new Map([
          [
            "slack",
            toolState({
              key: "slack",
              configured: true,
              requirements: [],
              seats_required: true,
              seats: [{ handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true }],
            }),
          ],
        ]),
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Slack details/ }));
  expect(screen.getAllByText("SRE Lead").length).toBe(1);
});

// A company whose Slack block exists and whose seats hold no app is not
// connected: nothing can post.
// A PER-SEAT APP'S WORK IS ON THE AGENT'S ROW, not on the card.
//
// This asserted a Continue button, which opens the company form: a form that
// can neither create an agent's app nor install it, because both are clicks
// at the third-party app on the agent's own row. It was a control leading
// nowhere on the one card with real work outstanding. The card still SAYS so,
// through the tag its reconcile phase drives.
test("a per-seat app leaves the work to the agent's own row", () => {
  // SETTLED, because this is about what the action IS. A card between a
  // connect and the loop's first report offers nothing at all, which is a
  // different rule with its own test.
  const state = rollUp(
    slack,
    rowsOf({ key: "slack", configured: true, reconcile: { phase: "ready" } }),
  );
  // GITHUB'S SHAPE: unfinished, and with nothing left to type. Both acts that
  // produce an agent's app happen at GitHub, from that agent's own row, so a
  // GitHub seat carries no requirements and the dialog has no box a Continue
  // button could take anybody to.
  const action = actionFor(
    state,
    [
      toolState({
        key: "github",
        configured: true,
        satisfied: false,
        form_complete: true,
        seats_required: true,
        seats: [{ handle: "cto", requirements: [], satisfied: false, enrolled: true }],
      }),
    ],
    true,
  );
  expect(action).toBeNull();

  // AND SLACK'S: unfinished because a seat's own credential is unanswered,
  // which is a box in this very dialog. Suppressing the button there would
  // leave the agent's app with no way to be filled in at all.
  const typeable = actionFor(
    state,
    [
      toolState({
        key: "slack",
        configured: true,
        satisfied: false,
        form_complete: false,
        seats_required: true,
        seats: [{ handle: "cto", requirements: [], satisfied: false }],
      }),
    ],
    true,
  );
  expect(typeable?.label).toBe("Continue");

  // AND AN INFORMATIONAL ROSTER IS NOT WORK OUTSTANDING. Every app lists its
  // agents now, and most of those credentials are an upgrade on an app that
  // already works: reading any roster as unfinished put a Continue button on
  // every connected card.
  const informational = actionFor(
    state,
    [
      toolState({
        key: "datadog",
        configured: true,
        satisfied: true,
        seats: [{ handle: "cto", requirements: [], satisfied: false }],
      }),
    ],
    true,
  );
  expect(informational).toBeNull();
});

// --- what a card offers, and what it no longer does ------------------------ //

// CONNECTING IS THE PERMISSION, so nothing on a connected card asks for one
// again. The reconcile loop registers the hooks and creates the accounts on
// its own timer now, which took the meaning out of every button that used to
// stand for a permission a person had to grant a second time: Run setup and
// Recheck per surface, and Manage in the header beside Disconnect.
test("a connected card offers no pass controls", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true })}
      sections={[
        { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
        { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
      ]}
      onConnect={() => {}}
      onDisconnect={() => {}}
    />,
  );

  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  for (const gone of ["Run setup", "Recheck", "Manage"]) {
    expect(screen.queryByRole("button", { name: gone })).toBeNull();
  }
  expect(screen.getByRole("button", { name: "Disconnect" })).toBeTruthy();
});

// SETTINGS SIT BESIDE THE DISCLOSURE, as a square the size of the chevron.
//
// At the foot of the open card it was an always-available control behind a
// disclosure and one scroll away; as a labelled button in the header it
// competed with Disconnect for the reader's eye. An icon square does
// neither, and it is available without opening the card.
test("settings open from the header, beside the chevron", () => {
  let opened = 0;
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "datadog")!}
      rows={rowsOf({ key: "datadog", configured: true })}
      sections={[{ name: "Datadog", tool: toolState({ key: "datadog" }) }]}
      onConnect={() => {
        opened += 1;
      }}
      onDisconnect={() => {}}
    />,
  );

  // Closed, without opening the card at all.
  fireEvent.click(screen.getByRole("button", { name: "Datadog settings" }));
  expect(opened).toBe(1);
});

// AND A TOOL NOBODY HAS CONNECTED HAS NO SETTINGS TO OPEN: its Connect
// button is the whole of what it offers.
test("an unconnected tool offers no settings square", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "gitlab")!}
      rows={rowsOf()}
      sections={[{ name: "GitLab", tool: toolState({ key: "gitlab", configured: false }) }]}
      onConnect={() => {}}
    />,
  );
  expect(screen.queryByRole("button", { name: "GitLab settings" })).toBeNull();
  expect(screen.getByRole("button", { name: "Connect" })).toBeTruthy();
});

// THE AGENTS ARE THE POINT, so the card lists them.
//
// Only Slack had a roster, because it was the only app with a route per seat
// and the roster was built from that. Every other card opened on a surface
// row and nothing else: Connected, over no answer at all to which of this
// company's agents can actually work in the app.
test("the card lists each agent and what it holds", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "datadog")!}
      rows={rowsOf({ key: "datadog", configured: true })}
      sections={[
        {
          name: "Datadog",
          tool: toolState({
            key: "datadog",
            seats: [
              {
                handle: "sre-lead",
                name: "SRE Lead",
                requirements: [],
                present: true,
                satisfied: true,
                detail: "mcp_env.datadog.DD_APP_KEY",
              },
              {
                handle: "cto",
                name: "CTO",
                requirements: [],
                present: false,
                satisfied: false,
                detail: "not in Datadog yet, created on the next sync",
              },
            ],
          }),
        },
      ]}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Datadog details/ }));
  expect(screen.getByText("SRE Lead")).toBeTruthy();
  expect(screen.getByText("CTO")).toBeTruthy();
  // AND WHAT EACH ONE HOLDS. A roster of names with no state is a list of
  // agents, not an answer about the integration.
  expect(screen.getByText("mcp_env.datadog.DD_APP_KEY")).toBeTruthy();
  expect(screen.getByText("not in Datadog yet, created on the next sync")).toBeTruthy();
});

// ONE ROW PER AGENT, whatever the card is made of.
//
// A company with one agent saw three rows on Atlassian — Organization, Jira,
// Confluence — because every surface reports its own roster and the card
// listed them all. They are one person's one account: Atlassian is where it
// is created, and the two products are what it then works in.
test("an agent is listed once however many surfaces report it", () => {
  const seat = (detail: string, satisfied: boolean) => [
    { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied, detail },
  ];
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true })}
      sections={[
        {
          name: "Organization",
          tool: toolState({
            key: "atlassian",
            can_provision: true,
            seats: seat("mcp_env.atlassian.JIRA_API_TOKEN", true),
          }),
        },
        {
          name: "Jira",
          tool: toolState({ key: "jira", seats: seat("no Jira credential yet", false) }),
        },
        {
          name: "Confluence",
          tool: toolState({
            key: "confluence",
            seats: seat("no Confluence credential yet", false),
          }),
        },
      ]}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getAllByText("SRE Lead").length).toBe(1);
  // THE SURFACE THAT PROVISIONS WINS, because its answer is about the
  // account rather than about one product's credential slot.
  expect(screen.getByText("mcp_env.atlassian.JIRA_API_TOKEN")).toBeTruthy();
  expect(screen.queryByText(/no Confluence credential/)).toBeNull();
});

// WHAT THIS COMPANY HAS COMES FIRST, then what it could have.
//
// Connected covers ATTEMPTED as well as working: a card with a block behind
// it, whatever the loop says about it, because a broken integration is one
// this company has and is the row most worth reaching first. Sorting on
// health instead would move a row out from under the cursor of somebody
// watching an app recover, which is exactly when they are looking at it.
test("configured integrations lead the list, each half alphabetical", () => {
  const rows = rowsOf(
    // Datadog works; Jira is broken. Both are this company's.
    { key: "datadog", configured: true, reconcile: { phase: "ready" } },
    { key: "jira", configured: true, reconcile: { phase: "degraded", detail: "x" } },
  );
  const order = [...CATALOG].sort(byConfiguredThenName(rows)).map((e) => e.name);
  expect(order).toEqual(["Atlassian", "Datadog", "GitHub", "GitLab", "Mattermost", "Slack"]);
  // And with nothing configured it is plain alphabetical, unchanged.
  const empty = [...CATALOG].sort(byConfiguredThenName(rowsOf())).map((e) => e.name);
  expect(empty).toEqual([...empty].sort((a, b) => a.localeCompare(b)));
});

// DISCONNECT MEANS THE CARD, and the provisioner goes last.
//
// It took the surface that happened to be listed first: on Atlassian the
// organization's account was deleted and its block removed while Jira and
// Confluence were left untouched, under a card still reading Connected. And
// the order is not arbitrary — the organization's credential is what removes
// the accounts, so taking it first would strand what the products still hold.
test("disconnect takes every configured surface, provisioner last", () => {
  const rows = rowsOf(
    { key: "atlassian", configured: true },
    { key: "jira", configured: true },
    { key: "confluence", configured: true },
  );
  const sections = [
    // can_provision IS TRUE ON ALL THREE, because the engine registers a
    // pass for each. Saying false for the products is the fixture that let
    // this function ship ordering by a field that cannot discriminate.
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
  ];
  // The catalogue's dependency order reversed. What this asserts is the LAST
  // position; the two products are peers and either order between them takes
  // the same things away.
  expect(disconnectOrder(atlassian, new Map(), sections)).toEqual(["jira", "confluence", "atlassian"]);
});

// THE FORGE RELAY IS NOT A SURFACE ANYTHING CAN DISCONNECT, and it used to be
// the first one this asked to.
//
// It is an ingress PATH for Jira and Confluence Cloud rather than a kind:
// `integration.Kinds` does not contain it, so `DELETE /setup/integrations/forge`
// answers 404 — and the setup payload, built from the same set, offers it no
// tool state at all. This function also admitted anything with a TRAFFIC row,
// and the relay has one on every Cloud company. So `forge` sorted ahead of the
// three real surfaces (nothing gave it a section, so it fell in the
// non-provisioning half) and the dialog aborted on its very first request,
// having touched nothing.
test("disconnect leaves out the relay, which is not a kind anything can delete", () => {
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
    // NO SECTION FOR forge, which is what the engine actually serves: the
    // setup payload is built from integration.Kinds and the relay is not one.
  ];
  const order = disconnectOrder(atlassian, new Map(), sections);
  expect(order).not.toContain("forge");
  expect(order).toEqual(["jira", "confluence", "atlassian"]);
});

// THE ORGANIZATION GOES LAST EVEN THOUGH EVERY SURFACE PROVISIONS, which is
// the production shape and the one the old fixtures denied.
//
// `can_provision` says a surface has a reconcile pass. The engine registers one
// for Atlassian, Jira and Confluence alike, so partitioning on it put all three
// in the same half and shipped the catalogue's own order — organization FIRST,
// the exact order its doc forbade. The order is the declared dependency
// reversed instead, which cannot collapse.
test("disconnect puts the account-creating surface last however many provision", () => {
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
  ];
  const order = disconnectOrder(atlassian, new Map(), sections);
  expect(order[order.length - 1]).toBe("atlassian");
  expect(order).toHaveLength(3);
});

// AND A SURFACE NOBODY CONFIGURED IS NOT DELETED. A card lists what a tool
// CAN be made of; a delete against a surface with no block behind it is a
// request with nothing behind it.
test("disconnect skips the surfaces this company does not have", () => {
  const rows = rowsOf({ key: "jira", configured: true });
  const sections = [{ name: "Jira", tool: toolState({ key: "jira" }) }];
  expect(disconnectOrder(atlassian, new Map(), sections)).toEqual(["jira"]);
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
// control for it. Otherwise a catalogue of six unconfigured third-party apps
// would carry six buttons that undo nothing.
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
  // NEUTRAL: the engine is doing it and nobody is owed anything, so a card
  // being taken away must not be dressed as one that has broken.
  expect(state.tone).toBe("neutral");
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

// ALPHABETICAL, and it replaced a connected-first sort deliberately.
//
// Connected-first reads well the first time and badly afterwards: the order
// changes as an app connects or fails, so a row moves out from under the
// cursor of somebody who came back to the same screen expecting it where it
// was. A name does not move.
test("the catalogue is ordered by name, whatever is connected", () => {
  const names = [...CATALOG].sort((a, b) => a.name.localeCompare(b.name)).map((e) => e.name);
  expect(names).toEqual([...names].sort((a, b) => a.localeCompare(b)));
  // And the order does not depend on state: the same list, connected or not.
  expect(names[0]).toBe("Atlassian");
  expect(names.at(-1)).toBe("Slack");
});

// WHICH PHASES THE SCREEN KEEPS WATCHING, and the rule tying that to what it
// paints. A connect starts work that takes seconds, and the row is the only
// place saying how it went: polled at a minute's cadence, a card said
// "Waiting for the provider" long after it had settled, which reads as the
// connect having done nothing.
//
// AMBER AND IN-FLIGHT ARE NEARLY THE SAME SET, and the exception is the
// point: `degraded` is amber because something is wrong, and it is NOT
// in-flight because nothing resolves it but a person. A phase added to one
// list and not the other is either watched forever or never watched at all.
test("every in-flight phase is amber, and the only amber phase that settles is degraded", () => {
  for (const phase of IN_FLIGHT) {
    expect(phaseTone(phase)).toBe("caution");
  }
  const amber = ["awaiting_admin", "provisioning", "activating", "disconnecting", "degraded"];
  for (const phase of amber) {
    expect(phaseTone(phase)).toBe("caution");
    expect(IN_FLIGHT.has(phase)).toBe(phase !== "degraded");
  }
  expect(phaseTone("ready")).toBe("positive");
  expect(IN_FLIGHT.has("ready")).toBe(false);
});

// THE SURFACE THAT DELETES THE ACCOUNTS RECEIVES NO DELIVERIES.
//
// Atlassian's organization has no inbound route: it is where accounts are
// made, not somewhere Atlassian posts to. This filtered on the TRAFFIC rows,
// so the organization was dropped from every disconnect, its teardown never
// ran, and "remove the accounts Crewlet created" removed nothing while
// reporting success. The service accounts stayed in the admin console.
test("disconnect includes a configured surface that has no traffic row", () => {
  // Only the products have rows, which is what the engine reports.
  const rows = rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true });
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
  ];
  const order = disconnectOrder(atlassian, new Map(), sections);
  expect(order).toContain("atlassian");
  // AND STILL LAST: the organization's credential is what removes the
  // accounts, so taking it first would strand them.
  expect(order[order.length - 1]).toBe("atlassian");
});

// AND A SURFACE NOBODY CONFIGURED IS STILL LEFT ALONE. A delete against one
// is a request with nothing behind it.
test("disconnect skips a surface this company never configured", () => {
  const rows = rowsOf({ key: "jira", configured: true });
  const sections = [
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
    // Declared by the catalogue, never configured by this company.
    { name: "Forge relay", tool: toolState({ key: "forge", configured: false }) },
  ];
  expect(disconnectOrder(atlassian, new Map(), sections)).toEqual(["jira"]);
});

// A CARD CANNOT BE CONNECTED AND OFFER TO CONNECT.
//
// The two halves of this screen arrive separately and the socket's rows are
// quicker. Straight after a connect the rows already say the block exists
// while the setup listing is still the pre-connect one, so the card drew
// "Connect" beside a tag reading Connected: two controls describing the same
// tool, disagreeing, with the button the wrong one.
test("a stale setup listing offers nothing rather than contradicting the tag", () => {
  const connected = rollUp(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready" } }),
  );
  const behind = [
    toolState({ key: "atlassian", configured: false }),
    toolState({ key: "jira", configured: false }),
  ];
  // present: the rows say this company has the tool.
  expect(actionFor(connected, behind, true)).toBeNull();
  // AND A TOOL NOBODY HAS CONNECTED STILL OFFERS CONNECT. With no rows there
  // is no tag to contradict, and the button is the only thing on the card
  // that says anything.
  expect(actionFor(connected, behind, false)?.label).toBe("Connect");
});

// --- an agent's own app, in the two acts a person performs ---------------- //

import { waitFor } from "@testing-library/react";
import { beforeEach, vi } from "vitest";
import type { SetupSeatState } from "~/protocol/types.ts";

const github = CATALOG.find((e) => e.key === "github")!;

/** One GitHub card with this roster, opened. */
function roster(...seats: SetupSeatState[]): { container: HTMLElement } {
  const rendered = render(
    <EntryRow
      entry={github}
      rows={rowsOf({ key: "github", configured: true })}
      sections={[{ name: "GitHub", tool: toolState({ key: "github", seats }) }]}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show GitHub details/ }));
  return rendered;
}

function seatOf(over: Partial<SetupSeatState>): SetupSeatState {
  return { handle: "cto", name: "CTO", requirements: [], satisfied: false, ...over };
}

type Sent = { method: string; path: string; body: unknown };

function stubFetch(sent: Sent[], answer: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        path: new URL(String(input), "http://engine.test").pathname,
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      return new Response(JSON.stringify(answer), { status });
    }),
  );
}

/**
 * The form the page submits, kept after it is taken out of the document.
 *
 * jsdom implements no submission, so the spy IS the assertion point: what
 * reaches the code host is exactly this element's action, target and fields.
 */
function capturedForm(): { form: HTMLFormElement | null } {
  const seen: { form: HTMLFormElement | null } = { form: null };
  vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (
    this: HTMLFormElement,
  ) {
    seen.form = this;
  });
  return seen;
}

function fieldsOf(form: HTMLFormElement): Record<string, string> {
  return Object.fromEntries([...form.querySelectorAll("input")].map((i) => [i.name, i.value]));
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
});

// AN AGENT WITH NO APP OF ITS OWN CANNOT ACT AS ITSELF, and the roster is
// where that is answered: one app is one bot identity, so a card that says
// Connected over a seat with nothing is reporting the half that cannot be
// acted on. The button is what the engine's own step asks for.
test("a seat that needs an app of its own gets the button that creates one", () => {
  roster(
    seatOf({
      step: "create_app",
      tier: "read_only",
      // THE LABEL COMES WITH THE TIER, always: the dashboard is embedded in
      // the binary that serves it, so the two are never a version apart and
      // the screen never has to restate a vocabulary Go owns.
      tier_label: "Read-only",
      detail: "no app of its own yet",
    }),
  );

  expect(screen.getByRole("button", { name: "Create app on GitHub" })).toBeTruthy();
  // AND NOT THE OTHER STEP. They are two acts, and an operator shown both at
  // once has no way to know which one they are on.
  expect(screen.queryByRole("link", { name: "Install on GitHub" })).toBeNull();
  // THE TIER, in the engine's own words: two agents on one card can hold
  // apps with different permissions, and "not set up" says the same word
  // over both.
  expect(screen.getByText("Read-only")).toBeTruthy();
});

// A TIER SITS BESIDE THE NAME, NOT OUT WITH THE STATUS.
//
// It is a standing fact about the agent rather than something needing
// attention, which is where the console puts it and why. As a chip out on
// the right beside "ready", two chips on one row read as two verdicts and a
// reader scanning a roster for what is wrong stopped on "read-only" every
// time.
//
// The words are the engine's: the tiers are a closed set whose permissions
// live in Go beside the code that mints the tokens, so a screen prettifying
// the raw id would be free to drift from what a token actually carries.
test("a seat's tier is a tag on its name, not a second status chip", () => {
  const { container } = roster(
    seatOf({
      satisfied: true,
      tier: "full_access",
      tier_label: "Full access",
      tier_hint: "Branches, commits, pull requests, issues and checks. No administration.",
      detail: "acme/api, acme/web",
    }),
  );

  const tier = container.querySelector(".int-seat-tier");
  if (!tier) throw new Error("the seat carries no tier tag");
  expect(tier.textContent).toBe("Full access");
  // THE TIER'S OWN CLASS carries the weight: full access takes the accent,
  // review a lighter share, read-only stays neutral.
  expect(tier.className).toContain("int-seat-tier--full_access");
  // BESIDE THE NAME, which is what makes it read as an attribute of the
  // agent rather than a verdict on it.
  expect(tier.closest(".int-row-name")).not.toBeNull();
  // AND NOT AMONG THE BADGES, where the status is the only answer.
  expect(tier.closest(".int-row-badges")).toBeNull();
  // AND WHAT IT GRANTS IS REACHABLE. Three words on a pill cannot say what
  // full access does and does not include; the sentence that can is the
  // engine's own.
  expect(tier.getAttribute("title")).toContain("No administration");

  // WHAT IT REACHES is the line underneath, which is the one thing two
  // finished seats on one card differ by.
  expect(screen.getByText("acme/api, acme/web")).toBeTruthy();
  expect(screen.getByText("ready")).toBeTruthy();
});

// A TOOL WITH NO TIERS SHOWS NO TAG. Slack gives an agent an app and no
// notion of how much of one, so a tag there would name a scope the app does
// not have.
test("a seat on a tierless app carries no tier tag", () => {
  const { container } = roster(seatOf({ satisfied: true, detail: "its own app" }));
  expect(container.querySelector(".int-seat-tier")).toBeNull();
});

// AN APP CAN GRADE AN AGENT IN A VOCABULARY THIS ENGINE HAS NO SCALE FOR.
//
// A Datadog role an organization made is its own name and nothing else: the
// tag says it, and the weight stays neutral, because drawing it in the accent
// reserved for full access would be this screen claiming how much a role it
// has never seen grants.
test("a graded seat with no known tier is drawn neutral", () => {
  const { container } = roster(
    seatOf({ satisfied: true, tier_label: "Acme On-Call", detail: "its own account" }),
  );
  const tag = container.querySelector(".int-seat-tier");
  if (!tag) throw new Error("the seat carries no tag for the role it holds");
  expect(tag.textContent).toBe("Acme On-Call");
  expect(tag.className).toBe("int-seat-tier");
});

// THE MANIFEST GOES AS A FORM POST, NEVER AS A FETCH.
//
// The request carries the operator's OWN session at the code host, which is
// what decides whether they may create an app on that organization, and the
// answer is a page the host renders for them to confirm. A fetch has neither:
// it would arrive as the dashboard rather than as the person, and the
// confirmation page would come back as a string with nowhere to display it.
test("the manifest reaches the code host as a form the operator's browser sends", async () => {
  const sent: Sent[] = [];
  const manifest = { name: "Crewlet CTO", url: "https://crewlet.ai", public: false };
  stubFetch(sent, {
    seat: "cto",
    tier: "review",
    action_url: "https://github.com/organizations/acme/settings/apps/new",
    manifest,
    state: "st-1",
  });
  const seen = capturedForm();
  roster(seatOf({ step: "create_app" }));

  fireEvent.click(screen.getByRole("button", { name: "Create app on GitHub" }));
  await waitFor(() => expect(seen.form).not.toBeNull());

  // ONE REQUEST, and it is the engine's. Nothing this dashboard sends reaches
  // the code host: a second entry here would be the fetch this must not be.
  expect(sent).toEqual([
    { method: "POST", path: "/setup/integrations/github/app", body: { seat: "cto" } },
  ]);

  const form = seen.form!;
  expect(form.method).toBe("post");
  expect(form.getAttribute("action")).toBe(
    "https://github.com/organizations/acme/settings/apps/new",
  );
  // A NEW TAB, so the flow's landing page does not take the dashboard away.
  expect(form.target).toBe("_blank");

  const fields = fieldsOf(form);
  // STRINGIFIED. A manifest handed to a form field as an object is the text
  // "[object Object]", which the host refuses with no clue why.
  expect(JSON.parse(fields.manifest!)).toEqual(manifest);
  expect(fields.state).toBe("st-1");
});

// THE SECOND ACT IS A LINK, because there is an address for it: the install
// page is built from the slug the host returned, which is a thing this engine
// can name and send somebody to.
test("an app nobody has installed is a link to the page that installs it", () => {
  roster(
    seatOf({
      step: "install_app",
      action_url: "https://github.com/apps/crewlet-cto/installations/new",
    }),
  );

  const link = screen.getByRole("link", { name: "Install on GitHub" });
  expect(link.getAttribute("href")).toBe("https://github.com/apps/crewlet-cto/installations/new");
  // A NEW TAB, so the card an operator started from is still behind it.
  expect(link.getAttribute("target")).toBe("_blank");
  expect(screen.queryByRole("button", { name: /Create app/ })).toBeNull();
});

// A CONTROL IS NEVER INVENTED. A step a newer node wrote is one this build
// has no act for, and a seat with nothing outstanding needs no control at
// all: guessing at either is worse than drawing nothing.
test("a step this build cannot perform draws no control", () => {
  roster(
    seatOf({ handle: "cto", step: "authorize_app" }),
    seatOf({ handle: "swe", name: "SWE", satisfied: true }),
    // INSTALL WITH NO ADDRESS. The install page is built from the slug the
    // host returned, so a seat whose app the engine cannot name has nowhere
    // to send anybody, and an anchor to nothing is a control that lies.
    seatOf({ handle: "sre", name: "SRE", step: "install_app" }),
  );

  expect(screen.queryByRole("button", { name: /Create app/ })).toBeNull();
  expect(screen.queryByRole("link", { name: /Install on/ })).toBeNull();
});

// A REFUSAL LANDS ON THE ROW THAT ASKED. The engine refuses this one for
// reasons an operator can fix (no public address is set, so an app created
// now would carry a delivery address that cannot be changed afterwards), and
// a button that quietly did nothing would leave them pressing it again.
test("the engine's refusal is reported beside the agent it was refused for", async () => {
  const sent: Sent[] = [];
  stubFetch(sent, { error: "no_public_url", hint: "set integrations.public_base_url first" }, 409);
  roster(seatOf({ step: "create_app" }));

  fireEvent.click(screen.getByRole("button", { name: "Create app on GitHub" }));
  expect(await screen.findByText("set integrations.public_base_url first")).toBeTruthy();
});

// --- the listing, and when it is worth reading again ---------------------- //

import { act } from "@testing-library/react";
import { useSetup } from "./Integrations.tsx";

/** The hook alone: it reaches for `rest` and for no context at all. */
function Probe() {
  const setup = useSetup();
  return (
    <span data-testid="probe">
      {setup.loading ? "loading" : "ready"}:{[...setup.byKey.keys()].join(",")}
    </span>
  );
}

// AN AGENT'S APP IS SET UP AT THE CODE HOST, IN ANOTHER TAB.
//
// This listing carries the roster and nothing pushes it, so a seat that had a
// Create button kept offering it after the app existed, and an operator who
// had done exactly what the card asked was told to do it again. The tab
// coming back is the one moment this screen can know something may have
// happened while it was not being read.
test("the roster is read again when the tab comes back", async () => {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      calls.push(new URL(String(input), "http://engine.test").pathname);
      return new Response(JSON.stringify({ tools: [toolState({ key: "github" })] }), {
        status: 200,
      });
    }),
  );
  render(<Probe />);
  await waitFor(() => expect(calls).toEqual(["/setup/integrations"]));

  // A TAB GOING AWAY IS NOT A REASON TO READ ANYTHING. Nobody is looking.
  Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  expect(calls.length).toBe(1);

  Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  // QUIETLY: there is already an answer on screen, and re-reading with the
  // skeleton would blank every card each time somebody switched tabs.
  expect(screen.getByTestId("probe").textContent).toBe("ready:github");
  await waitFor(() => expect(calls.length).toBe(2));
});

// A TOOL NOTHING CONVERGES IS NOT PERPETUALLY CONNECTING.
//
// "The loop has not reported yet" is a window for a surface with a pass and a
// permanent claim for one without. Slack's apps are created by hand, so the
// loop registers it for teardown alone and writes no status row for it ever:
// the card sat on Connecting in amber for as long as it was configured, beside
// its own roster reporting every agent ready. One screen, two answers, and the
// wrong one was the louder.
test("a tool with no provisioning pass reports from what can be seen", () => {
  const noPass: SetupToolState = {
    key: "slack",
    configured: true,
    enabled: true,
    satisfied: true,
    can_provision: false,
    requirements: [],
  };
  const healthy = rollUp(
    slack,
    rowsOf({ key: "slack", configured: true, enabled: true, routes: true }),
    [noPass],
  );
  expect(healthy.tag).toBe("Connected");
  expect(healthy.tone).toBe("positive");

  // AND AN INGRESS FAULT STILL OUTRANKS IT. A route that turns no delivery
  // into work for a seat is not connected, whoever converges it.
  const deaf = rollUp(
    slack,
    rowsOf({ key: "slack", configured: true, enabled: true, routes: false }),
    [noPass],
  );
  expect(deaf.tag).toBe("Action needed");
  expect(deaf.tone).toBe("caution");

  // A TOOL THAT DOES HAVE A PASS KEEPS THE WINDOW, because for it the loop
  // really is about to report.
  const withPass = rollUp(
    slack,
    rowsOf({ key: "slack", configured: true, enabled: true, routes: true }),
    [{ ...noPass, can_provision: true }],
  );
  expect(withPass.tag).toBe("Connecting");
});

// ONE AGENT, ONE ROW.
//
// A per-seat app contributes one section per agent and they all carry the same
// tool, so walking the sections listed the whole roster once per section: a
// company with one agent saw it twice in the disconnect dialog, and a company
// with ten would have seen a hundred rows.
test("the disconnect roster lists each agent once", () => {
  const tool: SetupToolState = {
    key: "github",
    configured: true,
    enabled: true,
    satisfied: true,
    seats_required: true,
    manage_path: "Delete GitHub App",
    // A COMPANY BLOCK AS WELL AS SEATS, which is GitHub's real shape and
    // what makes the sections outnumber the agents.
    requirements: [
      {
        field: "org",
        label: "Organization",
        kind: "id",
        config_path: "integrations.github.provisioning.org",
        required: true,
        present: true,
      },
    ],
    seats: [
      {
        handle: "sre-lead",
        name: "SRE Lead",
        satisfied: true,
        requirements: [],
        manage_url: "https://github.com/settings/apps/acme-sre/advanced",
      },
    ],
  };
  const entry: Entry = {
    key: "github",
    capability: "code",
    name: "GitHub",
    description: "Code and pull requests",
    vendor: "github",
    surfaces: [{ key: "github", name: "GitHub" }],
  };
  const sections = sectionsFor(entry, new Map([["github", tool]]));
  // The sections themselves are one per agent plus the company block, which
  // is the shape the dialog used to walk.
  expect(sections.length).toBeGreaterThan(1);

  const seats = [...new Map(sections.map((s) => [s.tool.key, s.tool])).values()].flatMap(
    (t) => t.seats ?? [],
  );
  expect(seats.map((s) => s.handle)).toEqual(["sre-lead"]);
});

// A REGISTRATION POINTING AT AN ADDRESS THIS DEPLOYMENT NO LONGER HAS.
//
// A company's public base moves: a tunnel restarts, a deployment is renamed,
// a proxy goes in front. Where a pass registers the hook the next tick moves
// it; where nothing does, the app goes on delivering to somewhere that no
// longer answers while the surface reports ready, because nothing it can see
// is wrong. The first symptom is an agent that stopped replying.
test("a surface registered at a moved address needs action", () => {
  const moved = rollUp(
    slack,
    rowsOf({
      key: "slack",
      configured: true,
      enabled: true,
      routes: true,
      endpoint: "https://old.example.com",
      endpoint_current: false,
      reconcile: { phase: "ready" },
    }),
  );
  expect(moved.tag).toBe("Action needed");
  expect(moved.tone).toBe("caution");

  // AND A SURFACE NOBODY HAS RECORDED AN ADDRESS FOR IS NOT A FAULT. Null
  // is "nothing here can say", which is what every row said before this
  // existed and what a surface says before it is ever set up.
  const unknown = rollUp(
    slack,
    rowsOf({
      key: "slack",
      configured: true,
      enabled: true,
      routes: true,
      endpoint_current: null,
      reconcile: { phase: "ready" },
    }),
  );
  expect(unknown.tag).not.toBe("Action needed");
});

// A ROW CARRYING ONLY AN ADDRESS IS NOT A REPORT.
//
// The engine's setup write stamps the address a surface was set up against on
// a surface no pass converges, which leaves a row with no phase in it. Read
// as a report, the empty phase became the card's tag: the Slack card showed
// NO STATE AT ALL, on precisely the surface whose moved address only a person
// can put right.
test("a phaseless row does not become an empty tag", () => {
  const slackEntry = CATALOG.find((e) => e.key === "slack")!;
  const state = rollUp(
    slackEntry,
    rowsOf({
      key: "slack",
      configured: true,
      enabled: true,
      endpoint: "https://old.example.com",
      endpoint_current: false,
      reconcile: { phase: "" },
    }),
    [{ key: "slack", configured: true, can_provision: false } as never],
  );
  expect(state.tag).toBe("Action needed");
  expect(state.tone).toBe("caution");
});

// AN AGENT THE LOOP HAS A FINDING ABOUT IS NOT BADGED READY.
//
// The roster and the reconcile rows come from two endpoints and meet on this
// card, so this is the only place the contradiction could be resolved.
// `satisfied` answers "a credential is sealed where this app looks for it",
// which stays true of a seat the app is refusing — and the card said an agent
// had no Jira account, that Atlassian was still setting it up, and that the
// same agent was **ready**, all at once.
test("an agent a surface reports on is not badged ready", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        { key: "atlassian", configured: true },
        {
          key: "jira",
          configured: true,
          reconcile: {
            phase: "activating",
            actor: "provider",
            findings: [
              {
                kind: "grant_pending",
                subject: "sre-lead",
                detail: "Atlassian is still giving SRE Lead's new account access to Jira",
              },
            ],
          },
        },
      )}
      sections={sectionsFor(
        atlassian,
        new Map([
          [
            "jira",
            toolState({
              key: "jira",
              configured: true,
              requirements: [],
              can_provision: true,
              seats: [
                {
                  handle: "sre-lead",
                  name: "SRE Lead",
                  requirements: [],
                  satisfied: true,
                  detail: "sre-lead-...@serviceaccount.atlassian.com",
                },
              ],
            }),
          ],
        ]),
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.queryByText("ready")).toBeNull();
  expect(screen.getByText("not ready")).toBeTruthy();
  // AND THE REASON IS PRINTED ONCE. The surface's own band carries it; the
  // row keeps who this agent is at the app, which is the question the roster
  // exists to answer and stays true of a seat the app is refusing.
  expect(screen.getAllByText(/still giving SRE Lead's new account access/).length).toBe(1);
  expect(screen.getByText(/serviceaccount\.atlassian\.com/)).toBeTruthy();
});

// AND AN AGENT NOBODY HAS A FINDING ABOUT KEEPS ITS BADGE, so this does not
// turn every roster amber.
test("an agent no surface reports on is still badged ready", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf({ key: "atlassian", configured: true }, { key: "jira", configured: true })}
      sections={sectionsFor(
        atlassian,
        new Map([
          [
            "jira",
            toolState({
              key: "jira",
              configured: true,
              requirements: [],
              can_provision: true,
              seats: [
                { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
              ],
            }),
          ],
        ]),
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getByText("ready")).toBeTruthy();
});

// DISCONNECT IS NEVER A SILENT NO-OP, AND THE ROWS ARE WHAT KEEPS IT FROM
// BEING ONE.
//
// The Disconnect button is rendered from the ROWS; `sections` comes from a
// separate `GET /setup/integrations` that can 401 for want of an operator
// token. With the key set taken from `sections` alone, that window rendered a
// button whose dialog computed an empty list, issued no DELETE, and then
// closed exactly as it does after a real teardown — an operator told the
// integration was being removed while nothing had been asked of anything.
test("a configured surface is disconnectable even when the setup listing is unreadable", () => {
  const rows = rowsOf(
    { key: "atlassian", configured: true },
    { key: "jira", configured: true },
  );
  expect(disconnectOrder(atlassian, rows, [])).toEqual(["jira", "atlassian"]);
});

// AND A SURFACE NEITHER SOURCE KNOWS IS STILL NOT DISCONNECTED, so the
// fallback does not turn the whole catalogue into a teardown list.
test("an unconfigured surface stays out of the disconnect order", () => {
  expect(disconnectOrder(atlassian, rowsOf({ key: "jira", configured: true }), [])).toEqual([
    "jira",
  ]);
});

// AN ADVISORY FINDING DOES NOT UN-READY A WORKING AGENT.
//
// Two of the engine's kinds carry a verdict of `ready` — a permission wider
// than the role asked for, a registration it no longer manages but which is
// still delivering. Both are notes on a healthy integration, and Classify
// ranks them beneath every real problem for that reason. Badging the agent
// amber contradicted the card's own Connected tag, on a seat with nothing
// wrong with it.
test("an advisory finding leaves the agent badged ready", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        { key: "atlassian", configured: true },
        {
          key: "jira",
          configured: true,
          reconcile: {
            phase: "ready",
            actor: "admin",
            findings: [
              {
                kind: "grant_excess",
                subject: "sre-lead",
                detail: "sre-lead holds more access than its role asks for",
                phase: "ready",
                actor: "admin",
              },
            ],
          },
        },
      )}
      sections={sectionsFor(
        atlassian,
        new Map([
          [
            "jira",
            toolState({
              key: "jira",
              configured: true,
              requirements: [],
              can_provision: true,
              seats: [
                { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
              ],
            }),
          ],
        ]),
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  // "not ready" is the discriminating half: with the bug the roster row
  // carried it. getAllByText for the positive half because a card whose
  // surfaces are ready has more than one badge saying so.
  expect(screen.queryByText("not ready")).toBeNull();
  expect(screen.getAllByText("ready").length).toBeGreaterThan(0);
});

// AND A FINDING WHOSE VERDICT THIS BUILD CANNOT READ IS A FAULT, not an
// advisory. A node older than the phase field sends none, and a kind a newer
// peer wrote is one this build has never heard of — read as advisory, either
// would hide a broken agent behind a green badge.
test("a finding with no verdict still un-readies the agent", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        { key: "atlassian", configured: true },
        {
          key: "jira",
          configured: true,
          reconcile: {
            phase: "degraded",
            actor: "admin",
            findings: [{ kind: "something_newer", subject: "sre-lead", detail: "unknown" }],
          },
        },
      )}
      sections={sectionsFor(
        atlassian,
        new Map([
          [
            "jira",
            toolState({
              key: "jira",
              configured: true,
              requirements: [],
              can_provision: true,
              seats: [
                { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
              ],
            }),
          ],
        ]),
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getByText("not ready")).toBeTruthy();
});
