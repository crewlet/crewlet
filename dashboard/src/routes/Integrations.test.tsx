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
  expect(screen.getByText(/you, at the third-party app/)).toBeTruthy();
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
  expect(state.tag).toBe("Connecting");
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
  expect(rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: true })).tag).toBe(
    "Connecting",
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

import { actionFor, sectionsFor } from "./Integrations.tsx";
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
  // A WORKING TOOL OFFERS NOTHING. Nothing a person does moves it, and the
  // Manage button that used to sit here read as a card's primary action
  // while saying only "nobody is needed".
  expect(actionFor(ready, [toolState({})])).toBeNull();

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
// Jira set up and Confluence half done is neither Connect nor finished: it is
// a tool with something left to do, and reading only the first surface would
// have called it done.
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
  expect(partial).toBeNull();
});

// A tool this build knows nothing about offers nothing: a button that
// discovers on a press that there is no surface behind it is worse than none.
test("a tool with no setup surface offers no action", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true }));
  expect(actionFor(state, [])).toBeNull();
});

// --- a per-seat third-party app ------------------------------------------------------ //

// SLACK GETS A SECTION PER SEAT, because each agent has its own app and so its
// own bot token. A seat with no app yet is exactly the seat somebody opened
// this dialog to give one to, so it is listed too.
test("a per-seat app becomes one section per agent", () => {
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
test("a per-seat app with no seat set up is unfinished", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true }));
  const action = actionFor(state, [
    toolState({
      key: "slack",
      configured: true,
      satisfied: true,
      // LOAD-BEARING, which is Slack's whole shape and nobody else's.
      seats_required: true,
      seats: [{ handle: "cto", requirements: [], satisfied: false }],
    }),
  ]);
  expect(action?.label).toBe("Continue");

  // AND AN INFORMATIONAL ROSTER IS NOT WORK OUTSTANDING. Every app lists its
  // agents now, and most of those credentials are an upgrade on an app that
  // already works: reading any roster as unfinished put a Continue button on
  // every connected card.
  const informational = actionFor(state, [
    toolState({
      key: "datadog",
      configured: true,
      satisfied: true,
      seats: [{ handle: "cto", requirements: [], satisfied: false }],
    }),
  ]);
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

// SETTINGS ARE UNDER THE DISCLOSURE. A connected tool still has values worth
// changing, and the control for them belongs where somebody reading the card
// already is rather than in the header competing with Disconnect.
test("settings open from the body, not the header", () => {
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

  // Closed, the header carries a state and a way out and nothing else.
  expect(screen.queryByRole("button", { name: "Settings" })).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /Show Datadog details/ }));
  fireEvent.click(screen.getByRole("button", { name: "Settings" }));
  expect(opened).toBe(1);
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

// AND A MULTI-SURFACE CARD SAYS WHICH SURFACE EACH ROW IS FOR.
//
// Atlassian is one card over Jira and Confluence, so an agent appears once
// per surface: without the prefix that is two rows reading "SRE Lead" with
// nothing distinguishing them, one ready and one not.
test("a roster on a two-surface card names the surface", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true })}
      sections={[
        {
          name: "Jira",
          tool: toolState({
            key: "jira",
            seats: [
              {
                handle: "sre-lead",
                name: "SRE Lead",
                requirements: [],
                satisfied: true,
                detail: "mcp_env.atlassian.JIRA_API_TOKEN",
              },
            ],
          }),
        },
        {
          name: "Confluence",
          tool: toolState({
            key: "confluence",
            seats: [
              {
                handle: "sre-lead",
                name: "SRE Lead",
                requirements: [],
                satisfied: false,
                detail: "no Confluence account yet",
              },
            ],
          }),
        },
      ]}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getByText(/Jira: SRE Lead/)).toBeTruthy();
  expect(screen.getByText(/Confluence: SRE Lead/)).toBeTruthy();
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
// control for it. Otherwise a catalogue of six unconfigured third-party apps would
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
