/**
 * A product's recent agent changes read as what happened: the step in words
 * (who allowed or denied a held act, why a failed one did not run), money in
 * the account's currency and never as bare micros, published text by its
 * length alone, and older pages on request, each read for that product.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { GrowthChangeRow } from "~/contract/growth.ts";
import { RecentChanges, stepWords } from "./RecentChanges.tsx";

const answers = vi.hoisted(() => ({ asked: [] as unknown[], pages: new Map<string, unknown>() }));
vi.mock("~/lib/store-hooks.ts", async (original) => ({
  ...(await original<typeof import("~/lib/store-hooks.ts")>()),
  useOrg: () => ({ roles: [], units: [] }),
}));
vi.mock("~/lib/seats.ts", async (original) => ({
  ...(await original<typeof import("~/lib/seats.ts")>()),
  indexOrg: () => ({ byHandle: new Map([["growth", { name: "Growth Lead" }]]) }),
}));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (_what: string, params?: { product?: string; cursor?: string }) => {
    answers.asked.push(params);
    return { data: answers.pages.get(params?.cursor ?? ""), loading: false, error: null };
  },
}));

const row = (extra: Partial<GrowthChangeRow>): GrowthChangeRow => ({
  seat: "growth",
  turn: "t",
  act: "h-1",
  held: false,
  product: "google_ads",
  connection: "main",
  account: "123-456-7890",
  target: "Spring sale",
  tool: "google_ads_lower_budget",
  class: "operate",
  step: "made",
  answered_by: "",
  answerer: "",
  operation: "ab",
  before: "cd",
  changes: [],
  money: null,
  failure: "",
  at: new Date(Date.now() - 3_600_000).toISOString(),
  ...extra,
});

beforeEach(() => {
  answers.asked = [];
  answers.pages = new Map();
});
afterEach(cleanup);

test("a step says who answered a held act and why a failed one did not run", () => {
  expect(stepWords(row({ step: "allowed", answerer: "Avery Admin" }))).toBe(
    "Allowed by Avery Admin",
  );
  expect(stepWords(row({ step: "failed", failure: "cap_exceeded" }))).toBe(
    "Failed: it would pass a spending cap",
  );
  expect(stepWords(row({ step: "void" }))).toBe("Not run: what it changes moved");
});

test("a lowered budget reads in the account's currency, and published text by its length alone", () => {
  answers.pages.set("", {
    rows: [
      row({
        changes: [
          {
            field: "amount",
            label: "Daily budget",
            money: true,
            text: false,
            before: "60000000",
            after: "40000000",
          },
        ],
      }),
      row({
        tool: "google_play_reply",
        target: "review-9",
        changes: [
          {
            field: "reply",
            label: "Reply",
            money: false,
            text: true,
            after_sha256: "cd",
            after_bytes: 212,
          },
        ],
      }),
    ],
    next: "",
    complete: true,
  });
  render(<RecentChanges product="google_ads" accounts={new Map([["123-456-7890", "EUR"]])} />);
  expect(screen.getAllByText(/by Growth Lead/)).toHaveLength(2);
  expect(screen.getByText("€60.00")).toBeTruthy();
  expect(screen.getByText("€40.00")).toBeTruthy();
  expect(screen.getByText(/Reply: nothing → published text, 212 bytes/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Show older changes" })).toBeNull();
});

test("older changes are read on request, from the cursor the page gave", () => {
  answers.pages.set("", { rows: [row({ target: "newest" })], next: "41", complete: false });
  answers.pages.set("41", { rows: [row({ target: "older" })], next: "", complete: true });
  render(<RecentChanges product="google_ads" accounts={new Map()} />);
  expect(answers.asked[0]).toEqual({ product: "google_ads" });
  fireEvent.click(screen.getByRole("button", { name: "Show older changes" }));
  expect(screen.getByText(/newest/)).toBeTruthy();
  expect(screen.getByText(/older/)).toBeTruthy();
  expect(answers.asked.at(-1)).toEqual({ product: "google_ads", cursor: "41" });
});

test("with nothing recorded, it says so", () => {
  answers.pages.set("", { rows: [], next: "", complete: true });
  render(<RecentChanges product="google_ads" accounts={new Map()} />);
  expect(screen.getByText("No agent has changed anything here yet.")).toBeTruthy();
});

test("money whose currency nobody named is never drawn as bare micros", () => {
  answers.pages.set("", {
    rows: [
      row({
        changes: [
          {
            field: "amount",
            label: "Daily budget",
            money: true,
            text: false,
            before: "60000000",
            after: "40000000",
          },
        ],
      }),
    ],
    next: "",
    complete: true,
  });
  render(<RecentChanges product="google_ads" accounts={new Map([["123-456-7890", ""]])} />);
  expect(screen.queryByText(/60000000|40000000/)).toBeNull();
  expect(
    screen.getByText(/an amount in an unnamed currency → an amount in an unnamed currency/),
  ).toBeTruthy();
});

test("a spend step's own currency wins over the account's", () => {
  answers.pages.set("", {
    rows: [
      row({
        money: {
          currency: "JPY",
          delta_micros: "0",
          daily_micros: "0",
          daily_after_micros: "0",
          per_change_cap_micros: "0",
          daily_cap_micros: "0",
        },
        changes: [
          {
            field: "amount",
            label: "Daily budget",
            money: true,
            text: false,
            before: "",
            after: "3400000000",
          },
        ],
      }),
    ],
    next: "",
    complete: true,
  });
  render(<RecentChanges product="google_ads" accounts={new Map([["123-456-7890", "EUR"]])} />);
  expect(screen.getByText(/Daily budget: nothing →/)).toBeTruthy();
  expect(screen.getByText("¥3,400")).toBeTruthy();
});

test("before the first answer it says it is reading, never that nothing changed", () => {
  render(<RecentChanges product="google_ads" accounts={new Map()} />);
  expect(screen.getByText("Reading the agents’ changes.")).toBeTruthy();
});
