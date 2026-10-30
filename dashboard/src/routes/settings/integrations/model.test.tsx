/**
 * What an app's page says about it, derived from the engine's two answers.
 *
 * The page never invents health: an idle integration and a broken one look
 * identical in the traffic counters, so every issue it raises is a finding
 * the reconcile loop made or a fact a surface's row states about its own
 * edge, and every agent's standing is the setup listing's. These cases hold
 * the derivation (`deriveTool`) and the two parts that lay an issue out.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import type {
  IntegrationRow,
  IntegrationTool,
  IntegrationToolState,
  ReconcileFinding,
} from "~/contract/integrations.ts";
import type { SetupSeatState, SetupToolState } from "~/protocol/types.ts";
import { CATALOG, FindingSubjects } from "./catalogue.tsx";
import { deriveTool, type ToolInputs } from "./model.ts";
import { IssueRow } from "./parts.tsx";

afterEach(cleanup);

const entry = (key: string) => CATALOG.find((e) => e.key === key)!;

function rowsOf(...rows: IntegrationRow[]): Map<string, IntegrationRow> {
  return new Map(rows.map((r) => [r.key, r]));
}

function rolled(
  key: string,
  state: IntegrationToolState,
  label: string,
  reason = "",
): IntegrationTool[] {
  return [{ key, surfaces: [key], state, label, reason }];
}

function listing(key: string, over: Partial<SetupToolState> = {}): SetupToolState {
  return {
    key,
    configured: true,
    enabled: true,
    satisfied: true,
    form_complete: true,
    requirements: [],
    ...over,
  };
}

function inputs(over: Partial<ToolInputs>): ToolInputs {
  return {
    rows: new Map(),
    rollups: undefined,
    setup: new Map(),
    guarded: false,
    trafficKnown: true,
    isSeat: () => false,
    avatarOf: () => null,
    ...over,
  };
}

// A WORKING TOOL RAISES NOTHING. Two surfaces at ready, the roll-up connected:
// a page that found something to say here would be reporting the weather.
test("a working tool raises no issue and reads as connected", () => {
  const tool = deriveTool(
    entry("atlassian"),
    inputs({
      rows: rowsOf(
        { key: "jira", configured: true, inbound: 4, reconcile: { phase: "ready" } },
        { key: "confluence", configured: true, inbound: 1, reconcile: { phase: "ready" } },
      ),
      rollups: rolled("atlassian", "connected", "Connected"),
    }),
  );
  expect(tool.issues).toEqual([]);
  expect(tool.bucket).toBe("ready");
});

// A FAULTED SURFACE SAYS WHAT IS WRONG AND WHICH SURFACE IT IS. The roll-up
// is the least ready surface, so a tool with two has two answers, and the
// row's own fact (a secret that did not resolve) is said whether or not the
// loop raised a finding about it. Mutation: drop `surfaceIssues` and the
// issue list is empty.
test("a surface's own fault is an issue naming the surface", () => {
  const tool = deriveTool(
    entry("atlassian"),
    inputs({
      rows: rowsOf(
        { key: "jira", configured: true, reconcile: { phase: "ready" } },
        {
          key: "confluence",
          configured: true,
          secret_usable: false,
          reconcile: { phase: "degraded", detail: "the webhook secret did not resolve" },
        },
      ),
      rollups: rolled("atlassian", "attention", "Action required"),
    }),
  );
  expect(tool.issues.map((i) => [i.surface, i.tone, i.owner, i.fix?.intent])).toEqual([
    ["Confluence", "danger", "you", "continue"],
  ]);
  expect(tool.issues[0]!.problem).toMatch(/^A secret Confluence needs is not set/);
  expect(tool.bucket).toBe("attention");
});

// AND NOT TWICE: where the loop raised the finding itself, its words stand.
test("a fault the loop already raised is said once, in the loop's words", () => {
  const tool = deriveTool(
    entry("datadog"),
    inputs({
      rows: rowsOf({
        key: "datadog",
        configured: true,
        secret_usable: false,
        reconcile: {
          phase: "unconfigured",
          findings: [
            { kind: "credential_missing", detail: "no API key is set", phase: "unconfigured" },
          ],
        },
      }),
    }),
  );
  expect(tool.issues.map((i) => i.problem)).toEqual(["No API key is set."]);
});

// AN ADDRESS THAT MOVED AND A ROUTE TO NOBODY are faults of the edge the row
// states: the first with what to do, the second as the fact it is, since no
// setting moves whether this engine routes an app's deliveries.
test("a moved address says what to do, and a delivery that reaches nobody is said", () => {
  const tool = deriveTool(
    entry("datadog"),
    inputs({
      rows: rowsOf({
        key: "datadog",
        configured: true,
        endpoint_current: false,
        routes: false,
        reconcile: { phase: "ready" },
      }),
    }),
  );
  expect(tool.issues.map((i) => [i.problem, i.remedy, i.fix])).toEqual([
    [
      "Datadog delivers to an old address, so its deliveries are lost.",
      "Set the address at Datadog to the engine's, at the foot of the list.",
      undefined,
    ],
    ["Datadog deliveries arrive but reach no agent.", undefined, undefined],
  ]);
});

// A PASS THAT FAILED WITH NOTHING TO SHOW FOR IT is still a failed pass, and
// the next move is the loop's own retry.
test("a failed pass is reported as a failed pass, owed by Crewlet", () => {
  const tool = deriveTool(
    entry("gitlab"),
    inputs({
      rows: rowsOf({
        key: "gitlab",
        configured: true,
        reconcile: { phase: "degraded", last_error: "the GitLab API timed out" },
      }),
    }),
  );
  expect(tool.issues.map((i) => [i.problem, i.owner])).toEqual([
    ["The last check of GitLab failed: the GitLab API timed out.", "crewlet"],
  ]);
});

// A FINDING THE ENGINE WILL NOT ACT ON LINKS TO THE APP BY NAME, and one the
// operator owns with no address carries no place to go.
test("a finding with an address at the app links there by the app's name", () => {
  const tool = deriveTool(
    entry("atlassian"),
    inputs({
      rows: rowsOf({
        key: "jira",
        configured: true,
        reconcile: {
          phase: "awaiting_admin",
          actor: "admin",
          findings: [
            {
              kind: "grant_pending",
              detail: "the app is waiting on a site admin",
              action_url: "https://acme.atlassian.net/admin",
              phase: "awaiting_admin",
            },
            {
              kind: "approval_required",
              detail: "a scope is not granted",
              phase: "awaiting_admin",
            },
          ],
        },
      }),
    }),
  );
  expect(tool.issues.map((i) => i.external)).toEqual([
    { label: "Open Atlassian", href: "https://acme.atlassian.net/admin" },
    undefined,
  ]);
});

// AN ADVISORY IS SAID AND NEVER RAISED: it leaves the agent ready and the
// tool working. A kind or phase this build does not know is the opposite, a
// fault, since "cannot say" must never read as fine.
test("an advisory is a note, and a finding this build does not know un-readies its agent", () => {
  const seat: SetupSeatState = { handle: "cto", name: "CTO", requirements: [], satisfied: true };
  const at = (findings: ReconcileFinding[]) =>
    deriveTool(
      entry("github"),
      inputs({
        rows: rowsOf({ key: "github", configured: true, reconcile: { phase: "ready", findings } }),
        setup: new Map([["github", listing("github", { seats: [seat] })]]),
        isSeat: (h) => h === "cto",
      }),
    );

  const advised = at([{ kind: "grant_wider", subject: "cto", detail: "wider", phase: "ready" }]);
  expect(advised.issues).toEqual([]);
  expect(advised.notes.map((n) => n.kind)).toEqual(["grant_wider"]);
  expect(advised.seats.map((s) => s.status)).toEqual(["ready"]);

  const unknown = at([
    { kind: "warp_misaligned", subject: "cto", detail: "odd", phase: "warping" },
  ]);
  expect(unknown.issues.map((i) => i.subjects)).toEqual([["cto"]]);
  expect(unknown.seats.map((s) => s.status)).not.toEqual(["ready"]);
});

// ONE AGENT, ONE LINE, however many surfaces report on it.
test("an agent is listed once however many surfaces report it", () => {
  const seat: SetupSeatState = { handle: "ana", name: "Ana", requirements: [], satisfied: true };
  const tool = deriveTool(
    entry("atlassian"),
    inputs({
      rows: rowsOf(
        { key: "jira", configured: true, reconcile: { phase: "ready" } },
        { key: "confluence", configured: true, reconcile: { phase: "ready" } },
      ),
      setup: new Map([
        ["jira", listing("jira", { seats: [seat] })],
        ["confluence", listing("confluence", { seats: [seat] })],
      ]),
    }),
  );
  expect(tool.seats.map((s) => s.handle)).toEqual(["ana"]);
});

// A TOOL'S DELIVERIES ARE ITS SURFACES' SUM, each once, and none at all where
// the node could not read its event log.
test("a tool's deliveries sum its surfaces once each, and only where counted", () => {
  const rows = rowsOf(
    { key: "jira", configured: true, inbound: 4 },
    { key: "confluence", configured: true, inbound: 2 },
    { key: "forge", configured: true, inbound: 1 },
  );
  expect(deriveTool(entry("atlassian"), inputs({ rows })).delivered).toBe(7);
  expect(deriveTool(entry("atlassian"), inputs({ rows, trafficKnown: false })).delivered).toBe(
    null,
  );
});

// --- laying an issue out ----------------------------------------------------- //

const issueTool = () =>
  deriveTool(
    entry("github"),
    inputs({
      rows: rowsOf({
        key: "github",
        configured: true,
        reconcile: {
          phase: "awaiting_admin",
          actor: "admin",
          findings: [
            {
              kind: "approval_required",
              subject: "agents without an app",
              detail: "4 agents have no GitHub App of their own",
              remedy: "Create one per agent, GitHub offers no API for it.",
              subjects: ["sre-lead", "cs-lead", "swe", "pm"],
              phase: "awaiting_admin",
            },
          ],
        },
      }),
      setup: new Map([["github", listing("github")]]),
    }),
  );

// THE PROBLEM, WHAT IT IS ABOUT AND THE REMEDY ARE SEPARATE PARTS, never one
// paragraph: measured as one line, a problem, a name, an instruction and a
// disclosure summary ran together at one weight.
test("an issue lays out its problem, subjects and remedy apart, with its fix", () => {
  const tool = issueTool();
  const onFix = vi.fn();
  const { container } = render(<IssueRow tool={tool} issue={tool.issues[0]!} onFix={onFix} />);
  const problem = container.querySelector(".int-issue-problem")!;
  expect(problem.textContent).toBe("4 agents have no GitHub App of their own.");
  const subjects = screen.getByRole("list", { name: "what this is about, 4" });
  expect(
    within(subjects)
      .getAllByRole("listitem")
      .map((li) => li.textContent),
  ).toEqual(["sre-lead", "cs-lead", "swe", "pm"]);
  expect(screen.getByText("Create one per agent, GitHub offers no API for it.")).toBeTruthy();
  expect(screen.getByText("Waiting on you")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Create apps" }));
  expect(onFix).toHaveBeenCalledWith("agents");
  // AND HELD, with its reason, where the form behind it cannot open.
  cleanup();
  const held = { ...tool, blocked: "Reading the setup state needs Admin." };
  render(<IssueRow tool={held} issue={tool.issues[0]!} onFix={onFix} />);
  fireEvent.click(screen.getByRole("button", { name: "Create apps" }));
  expect(onFix).toHaveBeenCalledTimes(1);
});

// A ROW IN A LIST ACROSS APPS SENDS THE READER TO THE APP rather than fixing
// it in a page that is not the app's.
test("an issue listed across apps names its app and links to its page", () => {
  const tool = issueTool();
  render(
    <Router>
      <IssueRow tool={tool} issue={tool.issues[0]!} showTool go="#/company/integrations/github" />
    </Router>,
  );
  expect(screen.getByText("GitHub:")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open GitHub" }).getAttribute("href")).toBe(
    "#/company/integrations/github",
  );
  expect(screen.queryByRole("button", { name: "Create apps" })).toBeNull();
});

// A LONG LIST EXPANDS IN PLACE: the rest are folded for width rather than
// secrecy, and the long case is a list somebody is working through.
test("a long subject list folds to a count that expands", () => {
  const many = Array.from({ length: 12 }, (_, i) => `agent-${i}@agents.test.invalid`);
  render(<FindingSubjects subjects={many} />);
  const last = "agent-11@agents.test.invalid";
  expect(screen.queryByText(last)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: /\+6 more/ }));
  expect(screen.getByText(last)).toBeTruthy();
});

// AND ONE SUBJECT IS SHOWN, never a count of one behind a click.
test("one subject is shown, not hidden behind a count", () => {
  render(<FindingSubjects subjects={["sre-lead"]} />);
  expect(screen.getByText("sre-lead")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /more/ })).toBeNull();
});
