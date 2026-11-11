/**
 * Settings › Delegated decisions draws the engine's answer and nothing of its
 * own: whether delegation is on, each rule as the engine numbers it, the
 * limits it enforces, and what the delegates decided, each list narrowed by
 * the one toolbar and saying in its foot how much of it is shown.
 *
 * Its own file because it mocks the screen's query hook.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
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
    },
  ],
  limits: { deadline_minutes: 15, decisions_per_hour: 30, server_allows_per_hour: 10 },
  day: "2026-11-09",
  since: "2026-11-09T00:00:00Z",
  until: "2026-11-10T00:00:00Z",
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
  expect(within(rules).getByText("Ops Lead")).toBeTruthy();
  expect(within(rules).getByText("1 in 10")).toBeTruthy();
  expect(screen.getByText("On")).toBeTruthy();
  const limits = screen.getByRole("table", { name: "What Crewlet enforces" });
  expect(within(limits).getByText("15 minutes")).toBeTruthy();
  expect(within(limits).getByText("30")).toBeTruthy();
  expect(within(limits).getByText("10")).toBeTruthy();
});

test("the digest lists each decision: the seat, what and where, the verdict, who and why", () => {
  answer.data = ANSWER;
  mount();
  const digest = screen.getByRole("table", { name: "Decided yesterday" });
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
  const digest = screen.getByRole("table", { name: "Decided yesterday" });
  expect(dataRows(digest)).toHaveLength(1);
  expect(screen.getByText("Showing 1 of 3. The log holds more than this page.")).toBeTruthy();
  expect(screen.getByText("Nothing matches")).toBeTruthy();
});

test("a day named in the address is the day asked for and headed", () => {
  answer.data = { ...ANSWER, day: "2026-11-05" };
  mount("#/settings/delegated-decisions?day=2026-11-05");
  expect(answer.asked).toContainEqual({ day: "2026-11-05" });
  expect(screen.getByRole("table", { name: "Decided on 2026-11-05" })).toBeTruthy();
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
