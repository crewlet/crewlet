/**
 * Home's notices about delegated decisions: the digest and the flagged
 * denials, for whoever the engine admits, each put away per day.
 *
 * Its own file because it mocks the notices' query hook.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { DelegatedDecisionsAnswer, DelegatedVerdictRow } from "~/contract/delegated.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { DelegatedNotices } from "./DelegatedNotices.tsx";

const read = vi.hoisted(() => ({
  data: null as unknown,
  error: null as string | null,
  enabled: [] as boolean[],
}));

vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (_what: string, _params: unknown, options: { enabled?: boolean }) => {
    read.enabled.push(options.enabled ?? true);
    return {
      data: read.data,
      loading: false,
      error: read.error,
      refusal: null,
      refetch: () => {},
    };
  },
}));

afterEach(() => {
  cleanup();
  read.data = null;
  read.error = null;
  read.enabled = [];
  localStorage.removeItem(STORAGE_KEYS.delegatedNotices);
});

const row = (at: string): DelegatedVerdictRow => ({
  request: at,
  kind: "ssh_operate",
  rule: 1,
  seat: "sre",
  by: "security-engineer",
  by_kind: "agent",
  through: "delegation",
  asked: "deny",
  confidence: "high",
  verdict: "deny",
  overruled: [],
  sampled: false,
  host: "web-1",
  item: "",
  at,
});

const ANSWER: DelegatedDecisionsAnswer = {
  enabled: true,
  rules: [],
  limits: { deadline_minutes: 15, decisions_per_hour: 30, server_allows_per_hour: 10 },
  day: "2026-11-09",
  since: "2026-11-09T00:00:00Z",
  until: "2026-11-10T00:00:00Z",
  decided: [row("2026-11-09T10:00:00Z"), row("2026-11-09T11:00:00Z")],
  decided_more: false,
  spot_checks: [],
  spot_checks_more: false,
  flagged: [row("2026-11-08T09:00:00Z")],
  flagged_more: false,
};

const READER = { owner: "ops-lead", loading: false };

test("the digest and the flagged denials each link to the Settings view", () => {
  read.data = ANSWER;
  render(<DelegatedNotices viewer={READER} />);
  expect(screen.getByText("Delegates decided 2 requests yesterday")).toBeTruthy();
  expect(screen.getByText("A delegate flagged 1 request as harmful")).toBeTruthy();
  const links = screen.getAllByRole("link").map((a) => a.getAttribute("href"));
  expect(links).toEqual([
    "#/settings/delegated-decisions?verdict=deny",
    "#/settings/delegated-decisions",
  ]);
});

// A REFUSAL IS NOT A NOTICE: the read is an administrator's and the people a
// rule escalates to, and a reader the engine refuses is shown nothing at all.
// Mutation: draw the notices off a stale answer beside the refusal.
test("a reader the engine refuses sees no notice", () => {
  read.data = ANSWER;
  read.error = "unauthorized";
  const { container } = render(<DelegatedNotices viewer={READER} />);
  expect(container.textContent).toBe("");
});

test("nothing decided and nothing flagged is no notice", () => {
  read.data = { ...ANSWER, decided: [], flagged: [] };
  const { container } = render(<DelegatedNotices viewer={READER} />);
  expect(container.textContent).toBe("");
});

test("a reader signed in as nobody is not asked for", () => {
  render(<DelegatedNotices viewer={{ owner: "", loading: false }} />);
  expect(read.enabled).toEqual([false]);
});

// PUT AWAY PER DAY: the digest stays away for the day it covers and comes
// back with the next one. Mutation: key it by nothing, and tomorrow's digest
// never shows.
test("a digest put away stays away for its day and returns with the next", () => {
  read.data = ANSWER;
  render(<DelegatedNotices viewer={READER} />);
  fireEvent.click(screen.getByRole("button", { name: "Dismiss for today" }));
  expect(screen.queryByText(/Delegates decided/)).toBeNull();
  expect(screen.getByText(/A delegate flagged/)).toBeTruthy();
  cleanup();
  render(<DelegatedNotices viewer={READER} />);
  expect(screen.queryByText(/Delegates decided/)).toBeNull();
  cleanup();
  read.data = { ...ANSWER, day: "2026-11-10" };
  render(<DelegatedNotices viewer={READER} />);
  expect(screen.getByText("Delegates decided 2 requests yesterday")).toBeTruthy();
});
