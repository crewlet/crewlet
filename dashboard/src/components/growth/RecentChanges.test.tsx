/**
 * A product's recent agent changes read as what happened: the step in words
 * (who allowed or denied a held act, by name; why a failed one did not run),
 * money in the account's currency and never as bare micros, published text by
 * its size alone, and older pages on request, each read for that product,
 * every record drawn once however many reads carried it.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { GrowthChangeRow } from "~/contract/growth.ts";
import { RecentChanges, stepWords } from "./RecentChanges.tsx";

const answers = vi.hoisted(() => ({
  asked: [] as unknown[],
  pages: new Map<string, unknown>(),
  errors: new Map<string, string>(),
}));
vi.mock("~/lib/seats.ts", async (original) => ({
  ...(await original<typeof import("~/lib/seats.ts")>()),
  useOrgIndex: () => ({
    byHandle: new Map([
      ["growth", { name: "Growth Lead" }],
      ["avery", { name: "Avery Admin" }],
    ]),
  }),
}));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (_what: string, params?: { product?: string; cursor?: string }) => {
    answers.asked.push(params);
    const cursor = params?.cursor ?? "";
    return {
      data: answers.pages.get(cursor) ?? null,
      loading: false,
      error: answers.errors.get(cursor) ?? null,
    };
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
  via: "",
  operation: "ab",
  before: "cd",
  changes: [],
  money: null,
  failure: "",
  at: new Date(Date.now() - 3_600_000).toISOString(),
  ...extra,
});

const page = (rows: GrowthChangeRow[], next = "", complete = true) => ({ rows, next, complete });
/** The tab's accounts, each ID to its currency, and to its name where one is given. */
const accountsOf = (currencies: Map<string, string>, names = new Map<string, string>()) =>
  new Map(
    [...currencies].map(([id, currency]) => [id, { id, currency, name: names.get(id) ?? "" }]),
  );
const draw = (currencies = new Map<string, string>(), names = new Map<string, string>()) =>
  render(<RecentChanges product="google_ads" accounts={accountsOf(currencies, names)} />);

beforeEach(() => {
  answers.asked = [];
  answers.pages = new Map();
  answers.errors = new Map();
});
afterEach(cleanup);

test("a step says who answered a held act, by name, and why a failed one did not run", () => {
  const names = (handle: string) => (handle === "avery" ? "Avery Admin" : handle);
  expect(stepWords(row({ step: "allowed", answerer: "avery" }), names)).toBe(
    "Allowed by Avery Admin",
  );
  expect(stepWords(row({ step: "denied", answerer: "" }), names)).toBe("Denied");
  expect(stepWords(row({ step: "failed", failure: "cap_exceeded" }), names)).toBe(
    "Failed: it would pass a spending cap",
  );
  expect(stepWords(row({ step: "void" }), names)).toBe("Not run: what it changes moved");
});

test("a lowered budget reads in the account's currency, and published text by its size alone", () => {
  answers.pages.set(
    "",
    page([
      row({
        act: "h-1",
        step: "allowed",
        answerer: "avery",
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
        act: "h-2",
        tool: "google_play_reply",
        target: "review-9",
        changes: [
          {
            field: "reply",
            label: "Reply",
            money: false,
            text: true,
            before_sha256: "",
            before_bytes: 0,
            after_sha256: "cd",
            after_bytes: 212,
          },
          {
            field: "title",
            label: "Title",
            money: false,
            text: true,
            before_sha256: "ab",
            before_bytes: 1,
            after_sha256: "",
            after_bytes: 0,
          },
        ],
      }),
    ]),
  );
  draw(new Map([["123-456-7890", "EUR"]]));
  expect(screen.getAllByText(/by Growth Lead/)).toHaveLength(2);
  expect(screen.getByText("Allowed by Avery Admin")).toBeTruthy();
  expect(screen.getByText("€60.00")).toBeTruthy();
  expect(screen.getByText("€40.00")).toBeTruthy();
  // The wire sends a size of 0 where there was no text: that is nothing, not "0 bytes".
  expect(screen.getByText(/Reply: nothing → published text, 212 bytes/)).toBeTruthy();
  expect(screen.getByText(/Title: published text, 1 byte → nothing/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Show older changes" })).toBeNull();
});

test("older changes are offered whenever the page names a next one, and read from it for the product", () => {
  // `complete` says every record was readable, not that the list ends: `next` does.
  answers.pages.set("", page([row({ act: "newest", target: "newest" })], "41", true));
  answers.pages.set("41", page([row({ act: "older", target: "older" })], "", true));
  draw();
  expect(answers.asked[0]).toEqual({ product: "google_ads" });
  fireEvent.click(screen.getByRole("button", { name: "Show older changes" }));
  expect(screen.getByText("newest")).toBeTruthy();
  expect(screen.getByText("older")).toBeTruthy();
  expect(answers.asked).toContainEqual({ product: "google_ads", cursor: "41" });
  // At the end the button stays, held with why, so focus is not dropped.
  const button = screen.getByRole("button", { name: "Show older changes" });
  expect(button.getAttribute("aria-disabled")).toBe("true");
});

test("a record read twice is drawn once, and one a newer change pushed off the newest page stays", () => {
  const a = row({ act: "a", target: "first act" });
  const b = row({
    act: "b",
    target: "second act",
    at: new Date(Date.now() - 7_200_000).toISOString(),
  });
  answers.pages.set("", page([a], "41"));
  answers.pages.set("41", page([a, b]));
  const view = draw();
  fireEvent.click(screen.getByRole("button", { name: "Show older changes" }));
  expect(screen.getAllByText("first act")).toHaveLength(1);
  // The newest page is read again with a new change that pushes `a` off it.
  const c = row({ act: "c", target: "newer act", at: new Date().toISOString() });
  answers.pages.set("", page([c], "40"));
  view.rerender(<RecentChanges product="google_ads" accounts={new Map()} />);
  const targets = screen.getAllByText(/act$/).map((el) => el.textContent);
  expect(targets).toEqual(["newer act", "first act", "second act"]);
});

test("a page this node could not read whole says the list may be missing some", () => {
  answers.pages.set("", page([row({})], "", false));
  draw();
  expect(screen.getByText(/may be missing a few/)).toBeTruthy();
});

test("with nothing recorded it says so; before the first answer it says it is reading", () => {
  draw();
  expect(screen.getByText("Reading the agents’ changes.")).toBeTruthy();
  cleanup();
  answers.pages.set("", page([]));
  draw();
  expect(screen.getByText("No agent has changed anything here yet.")).toBeTruthy();
});

test("a refused read says who may read it; any other failure says it is read again", () => {
  answers.errors.set("", "unauthorized");
  draw();
  expect(screen.getByText("Only an administrator reads the agents’ changes.")).toBeTruthy();
  cleanup();
  answers.errors.set("", "timeout");
  draw();
  expect(screen.getByText(/could not be read\. They are read again within a minute/)).toBeTruthy();
});

test("money whose currency nobody named is never drawn as bare micros", () => {
  answers.pages.set(
    "",
    page([
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
    ]),
  );
  draw(new Map([["123-456-7890", ""]]));
  expect(screen.queryByText(/60000000|40000000/)).toBeNull();
  expect(
    screen.getByText(/an amount in an unnamed currency → an amount in an unnamed currency/),
  ).toBeTruthy();
});

test("a spend step's own currency wins over the account's", () => {
  answers.pages.set(
    "",
    page([
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
    ]),
  );
  draw(new Map([["123-456-7890", "EUR"]]));
  expect(screen.getByText(/Daily budget: nothing →/)).toBeTruthy();
  expect(screen.getByText("¥3,400")).toBeTruthy();
});

// AN APP IS NAMED, ITS APPLE ID QUIET BESIDE IT: a bare ID tells a reader
// nothing, and an act on the app itself is titled by its name too. Mutation:
// draw `row.account` as sent, and the line reads the ID alone.
test("a change names its account, the ID beside the name, and an act on the account itself by name", () => {
  answers.pages.set(
    "",
    page([
      row({
        product: "app_store_connect",
        account: "6763222842",
        target: "6763222842",
        tool: "app_store_connect_update_listing",
      }),
    ]),
  );
  draw(new Map([["6763222842", ""]]), new Map([["6763222842", "Field Notes"]]));
  expect(screen.getByText("Field Notes", { selector: ".growth-change__what" })).toBeTruthy();
  expect(screen.getByText(/^Field Notes \(6763222842\) · by /)).toBeTruthy();
  cleanup();
  // NO NAME KNOWN: the ID alone, never an empty name.
  answers.pages.set("", page([row({})]));
  draw(new Map([["123-456-7890", ""]]));
  expect(screen.getByText(/^123-456-7890 · by /)).toBeTruthy();
});
