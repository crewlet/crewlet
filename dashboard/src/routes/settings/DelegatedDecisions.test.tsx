/**
 * Settings › Delegated decisions draws the engine's answer and nothing of its
 * own: whether delegation is on, each rule as the engine numbers it, the
 * limits it enforces, and what the delegates decided, each list narrowed by
 * the one toolbar and saying in its foot how much of it is shown.
 *
 * Its own file because it mocks the screen's query hook.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { Store } from "~/protocol/index.ts";
import { TestClient } from "~/test/client.tsx";
import { withDerived } from "~/test/org.ts";
import type { DelegatedDecisionsAnswer, DelegatedVerdictRow } from "~/contract/delegated.ts";
import { DelegatedDecisions, rowMatches, shiftDay } from "./DelegatedDecisions.tsx";

const answer = vi.hoisted(() => ({ data: null as unknown, asked: [] as unknown[] }));

vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (_what: string, params: unknown) => {
    answer.asked.push(params);
    return {
      data: answer.data,
      loading: false,
      error: null,
      refusal: null,
      detail: null,
      refetch: () => {},
    };
  },
}));

afterEach(() => {
  cleanup();
  answer.data = null;
  answer.asked = [];
  location.hash = "";
});

function verdict(over: Partial<DelegatedVerdictRow>): DelegatedVerdictRow {
  return {
    request: "req-1",
    kind: "ssh_operate",
    rule: 1,
    seat: "sre",
    by: "security-engineer",
    by_kind: "agent",
    through: "delegation",
    asked: "allow",
    confidence: "high",
    verdict: "allow",
    overruled: [],
    sampled: false,
    host: "web-1",
    item: "",
    at: "2026-11-09T10:00:00Z",
    subject: "systemctl restart api",
    reason: "Restarts the API after the config change the item describes.",
    findings: [],
    ...over,
  };
}

const ANSWER: DelegatedDecisionsAnswer = {
  enabled: true,
  rules: [
    {
      rule: 1,
      kinds: ["ssh_operate"],
      delegates: ["security-engineer"],
      escalate_to: ["ops-lead"],
      sample_one_in: 10,
      always_escalate_paths: [],
      required_checks: [],
    },
    {
      rule: 2,
      kinds: ["pr_merge", "deploy"],
      delegates: ["release-reviewer"],
      escalate_to: ["ops-lead"],
      sample_one_in: 5,
      always_escalate_paths: ["billing/**", "services/*/payments/**"],
      required_checks: [{ repo: "acme/api", branch: "main", checks: ["test", "lint"] }],
    },
  ],
  limits: { deadline_minutes: 15, decisions_per_hour: 30, server_allows_per_hour: 10 },
  // TODAY, SO FAR: what the page opens on.
  day: "2026-11-10",
  since: "2026-11-10T00:00:00Z",
  until: "2026-11-11T00:00:00Z",
  complete: false,
  decided: [
    verdict({}),
    verdict({
      request: "req-2",
      seat: "sre",
      subject: "rm -rf /var/lib/app",
      host: "db-1",
      asked: "deny",
      verdict: "deny",
      reason: "Deletes the application's data.",
      at: "2026-11-09T11:00:00Z",
    }),
    verdict({
      request: "req-3",
      by: "",
      by_kind: "engine",
      asked: "",
      confidence: "",
      verdict: "escalate",
      overruled: ["deadline"],
      reason: "",
      subject: "uptime",
      at: "2026-11-09T12:00:00Z",
    }),
  ],
  decided_more: true,
  spot_checks: [verdict({ request: "req-4", sampled: true })],
  spot_checks_more: false,
  flagged: [],
  flagged_more: false,
};

/** A grid's rows of data, never its head or its foot. */
const dataRows = (grid: HTMLElement) => [
  ...grid.querySelectorAll<HTMLElement>(".crewlet-data-grid__row"),
];

function mount(hash = "#/settings/delegated-decisions") {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(
    withDerived({
      name: "Acme",
      roles: [
        { name: "Ops Lead", handle: "ops-lead", kind: "human" },
        { name: "Security Engineer", handle: "security-engineer", kind: "agent" },
        { name: "Release Reviewer", handle: "release-reviewer", kind: "agent" },
        { name: "SRE", handle: "sre", kind: "agent" },
      ],
      units: [],
    }) as never,
  );
  render(
    <TestClient store={store}>
      <ToastProvider>
        <LayerHost>
          <Router>
            <DelegatedDecisions />
          </Router>
        </LayerHost>
      </ToastProvider>
    </TestClient>,
  );
}

test("draws whether delegation is on, each rule and the limits the engine enforces", () => {
  answer.data = ANSWER;
  mount();
  const rules = screen.getByRole("table", { name: "Rules" });
  expect(within(rules).getByText("Commands on servers")).toBeTruthy();
  expect(within(rules).getByText("Security Engineer")).toBeTruthy();
  expect(within(rules).getAllByText("Ops Lead")).toHaveLength(2);
  expect(within(rules).getByText("1 in 10")).toBeTruthy();
  expect(screen.getByText("On")).toBeTruthy();
  const limits = screen.getByRole("table", { name: "What Crewlet enforces" });
  expect(within(limits).getByText("15 minutes")).toBeTruthy();
  expect(within(limits).getByText("30")).toBeTruthy();
  expect(within(limits).getByText("10")).toBeTruthy();
});

// A MERGE AND DEPLOY RULE NAMES ITS KINDS AND ITS OWN PATHS, each path as
// written; a command rule has none to name, and says so rather than a blank.
test("a rule's own always-escalate paths are listed beside its kinds", () => {
  answer.data = ANSWER;
  mount();
  const rules = screen.getByRole("table", { name: "Rules" });
  const [ssh, release] = dataRows(rules);
  expect(within(release!).getByText("Merges")).toBeTruthy();
  expect(within(release!).getByText("Deploys")).toBeTruthy();
  expect(within(release!).getByText("billing/**")).toBeTruthy();
  expect(within(release!).getByText("services/*/payments/**")).toBeTruthy();
  // NEITHER PATHS NOR CHECKS on a command rule.
  expect(within(ssh!).getAllByText("Not for commands")).toHaveLength(2);
});

test("a rule with more paths than its row shows counts the rest", () => {
  const paths = ["a/**", "b/**", "c/**", "d/**", "e/**", "f/**"];
  answer.data = {
    ...ANSWER,
    rules: [{ ...ANSWER.rules[1]!, always_escalate_paths: paths }],
  };
  mount();
  const rules = screen.getByRole("table", { name: "Rules" });
  expect(within(rules).getByText("d/**")).toBeTruthy();
  expect(within(rules).queryByText("e/**")).toBeNull();
  expect(within(rules).getByText("2 more")).toBeTruthy();
});

// A RULE'S REQUIRED CHECKS read per repository and branch, the whole list on
// hover, and a release rule naming none relies on what the branch requires.
test("a rule lists the checks it requires on each repository's branch", () => {
  answer.data = ANSWER;
  mount();
  const rules = screen.getByRole("table", { name: "Rules" });
  const release = dataRows(rules)[1];
  expect(within(release!).getByText("acme/api main")).toBeTruthy();
  expect(within(release!).getByText("test, lint")).toBeTruthy();
});

test("a rule with more required-check entries than its row shows counts the rest", () => {
  const required_checks = ["api", "web", "jobs"].map((name) => ({
    repo: `acme/${name}`,
    branch: "main",
    checks: ["test"],
  }));
  answer.data = {
    ...ANSWER,
    rules: [{ ...ANSWER.rules[1]!, always_escalate_paths: [], required_checks }],
  };
  mount();
  const rules = screen.getByRole("table", { name: "Rules" });
  expect(within(rules).getByText("acme/web main")).toBeTruthy();
  expect(within(rules).queryByText("acme/jobs main")).toBeNull();
  expect(within(rules).getByText("1 more")).toBeTruthy();
});

test("a merge or deploy rule with no checks of its own says the branch decides", () => {
  answer.data = { ...ANSWER, rules: [{ ...ANSWER.rules[1]!, required_checks: [] }] };
  mount();
  const rules = screen.getByRole("table", { name: "Rules" });
  expect(within(rules).getByText("What the branch requires")).toBeTruthy();
});

test("a merge or deploy rule with no paths of its own says it keeps the built-in ones", () => {
  answer.data = { ...ANSWER, rules: [{ ...ANSWER.rules[1]!, always_escalate_paths: [] }] };
  mount();
  const rules = screen.getByRole("table", { name: "Rules" });
  expect(within(rules).getByText("The built-in paths only")).toBeTruthy();
});

// A MERGE'S VERDICT names no server, and says the absence as its own.
test("a merge's verdict reads its subject and names no server", () => {
  answer.data = {
    ...ANSWER,
    decided: [
      verdict({
        kind: "pr_merge",
        rule: 2,
        by: "release-reviewer",
        host: "",
        subject: "merge web!42 at 4f1c2a9b7e3d5f60718293a4b5c6d7e8f9012345 (merge)",
        overruled: [],
      }),
    ],
  };
  mount();
  const digest = screen.getByRole("table", { name: "Decided today, so far" });
  const [row] = dataRows(digest);
  expect(within(row!).getByText(/^merge web!42 at/)).toBeTruthy();
  expect(within(row!).getByText("Not on a server")).toBeTruthy();
  expect(within(row!).getByText(/Release Reviewer · rule 2/)).toBeTruthy();
});

test("the digest lists each decision: the seat, what and where, the verdict, who and why", () => {
  answer.data = ANSWER;
  mount();
  const digest = screen.getByRole("table", { name: "Decided today, so far" });
  const rows = dataRows(digest);
  expect(rows).toHaveLength(3);
  const deny = rows.find((r) => r.textContent?.includes("rm -rf /var/lib/app"))!;
  expect(within(deny).getByText("db-1")).toBeTruthy();
  expect(within(deny).getByText("Denied")).toBeTruthy();
  expect(within(deny).getByText(/Security Engineer · rule 1/)).toBeTruthy();
  expect(within(deny).getByText("Deletes the application's data.")).toBeTruthy();
  // THE ENGINE'S OWN VERDICT names Crewlet, never an empty giver.
  const engine = rows.find((r) => r.textContent?.includes("uptime"))!;
  expect(within(engine).getByText(/Crewlet · rule 1/)).toBeTruthy();
  // THE FOOT SAYS HOW MUCH IS SHOWN, and that the log held more.
  expect(screen.getByText("Showing 3 of 3. The log holds more than this page.")).toBeTruthy();
  expect(screen.getByRole("table", { name: "Spot-checks" })).toBeTruthy();
  expect(screen.getByText("No delegate flagged a request as harmful this week.")).toBeTruthy();
});

test("the toolbar narrows every list, and the foot counts what it shows", () => {
  answer.data = ANSWER;
  mount("#/settings/delegated-decisions?verdict=deny");
  const digest = screen.getByRole("table", { name: "Decided today, so far" });
  expect(dataRows(digest)).toHaveLength(1);
  expect(screen.getByText("Showing 1 of 3. The log holds more than this page.")).toBeTruthy();
  expect(screen.getByText("Nothing matches")).toBeTruthy();
});

test("a day named in the address is the day asked for and headed", () => {
  answer.data = { ...ANSWER, day: "2026-11-05", complete: true };
  mount("#/settings/delegated-decisions?day=2026-11-05");
  expect(answer.asked).toContainEqual({ day: "2026-11-05" });
  expect(screen.getByRole("table", { name: "Decided on 2026-11-05" })).toBeTruthy();
  // A WHOLE DAY PAGES FORWARD, towards today.
  fireEvent.click(screen.getByRole("button", { name: "The day after" }));
  expect(location.hash).toContain("day=2026-11-06");
});

// THE PAGE OPENS ON TODAY, SO FAR: the running day is asked for, headed and
// said to be still running; the pager goes back to yesterday and no further
// forward than today.
// Mutation: open on the engine's default, the last whole day, and a verdict
// given this morning is not on the page until tomorrow.
test("the page opens on today so far, and pages back but not past today", () => {
  answer.data = ANSWER;
  mount();
  expect(answer.asked).toContainEqual({ day: "today" });
  expect(screen.getByRole("table", { name: "Decided today, so far" })).toBeTruthy();
  expect(screen.getByText(/The company day is still running/)).toBeTruthy();
  const after = screen.getByRole("button", { name: "The day after" });
  expect(after.hasAttribute("disabled") || after.getAttribute("aria-disabled") === "true").toBe(
    true,
  );
  fireEvent.click(screen.getByRole("button", { name: "The day before" }));
  expect(location.hash).toContain("day=2026-11-09");
});

test("a running day nothing was decided on yet says so", () => {
  answer.data = { ...ANSWER, decided: [], decided_more: false };
  mount();
  expect(screen.getByText("No request has been decided yet today.")).toBeTruthy();
});

test("off, it says every request goes to people and keeps the rules", () => {
  answer.data = { ...ANSWER, enabled: false };
  mount();
  expect(screen.getByText("Off")).toBeTruthy();
  expect(screen.getByText(/every request goes to people at once/)).toBeTruthy();
});

test("a search reads the seat, the command, the server and the reason", () => {
  const name = (h: string) => ({ sre: "SRE" })[h] ?? h;
  const row = verdict({});
  expect(rowMatches(row, "restart", "", "", name)).toBe(true);
  expect(rowMatches(row, "WEB-1", "", "", name)).toBe(true);
  expect(rowMatches(row, "sre", "", "", name)).toBe(true);
  expect(rowMatches(row, "config change", "", "", name)).toBe(true);
  expect(rowMatches(row, "nowhere", "", "", name)).toBe(false);
  expect(rowMatches(row, "", "deny", "", name)).toBe(false);
  expect(rowMatches(row, "", "", "engine", name)).toBe(false);
});

test("a company day moves by whole days", () => {
  expect(shiftDay("2026-11-01", -1)).toBe("2026-10-31");
  expect(shiftDay("2026-12-31", 1)).toBe("2027-01-01");
});
