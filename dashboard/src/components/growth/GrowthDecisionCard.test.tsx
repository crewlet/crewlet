/**
 * A person allows exactly what the card shows: money in the account's own
 * currency from exact micros, every published text whole, the agent's and
 * the vendor's words as plain text, and an irreversible admin act held for a
 * confirmation.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import type { GrowthCard } from "~/contract/growth.ts";
import { GrowthDecisionCard, accountLine, growthCardRows } from "./GrowthDecisionCard.tsx";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const later = () => new Date(Date.now() + 23 * 3_600_000).toISOString();

function spend(extra: Partial<GrowthCard> = {}): GrowthCard {
  return {
    act: "ad_spend",
    product: "google_ads",
    tool: "google_ads_raise_budget",
    account: {
      product: "google_ads",
      account: "123-456-7890",
      account_name: "Nimbus Ads",
      currency: "EUR",
      time_zone: "Europe/Berlin",
      state: "serving",
    },
    acts: [
      {
        target: "Spring sale",
        summary: "Raise the daily budget",
        changes: [
          {
            field: "amount_micros",
            label: "Daily budget",
            before: "40000000",
            after: "60000000",
            money: true,
          },
        ],
      },
    ],
    money: {
      currency: "EUR",
      delta_micros: "20000000",
      daily_micros: "140000000",
      daily_after_micros: "160000000",
      per_change_cap_micros: "50000000",
      daily_cap_micros: "200000000",
    },
    findings: [],
    reason: "It spent its **whole** budget by 14:00 on each of the last 5 days.",
    lapses_at: later(),
    moved: false,
    other_open_asks: 0,
    ...extra,
  };
}

function draw(card: GrowthCard, extra: Partial<Parameters<typeof GrowthDecisionCard>[0]> = {}) {
  return render(
    <GrowthDecisionCard
      card={card}
      question="Raise the daily budget of Spring sale?"
      state="pending"
      note=""
      onNote={() => {}}
      onAllow={() => {}}
      onDeny={() => {}}
      {...extra}
    />,
  );
}

test("a spend card says the budget before and after, and the account's capacity against both caps, in the account's currency", () => {
  draw(spend());
  expect(screen.getByText("Raise the daily budget of Spring sale?")).toBeTruthy();
  expect(screen.getByText(/Nimbus Ads \(123-456-7890\) · EUR · Europe\/Berlin/)).toBeTruthy();
  expect(screen.getByText("€40.00")).toBeTruthy();
  expect(screen.getByText("€60.00")).toBeTruthy();
  expect(
    screen.getByLabelText(
      "€140.00 set, +€20.00 with this change: €160.00 of the €200.00 daily cap",
    ),
  ).toBeTruthy();
  expect(
    screen.getByText(
      /This change adds €20\.00 of the €50\.00 one change may add\. Caps limit the budgets agents may set\./,
    ),
  ).toBeTruthy();
  expect(screen.getByRole("button", { name: "Allow once" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Deny" })).toBeTruthy();
});

test("money keeps every digit an int64 of micros can hold, and each currency its own digits", () => {
  const card = spend({
    account: {
      product: "google_ads",
      account: "act_1",
      account_name: "Big",
      currency: "JPY",
      time_zone: "Asia/Tokyo",
      state: "serving",
    },
    acts: [
      {
        target: "Brand",
        summary: "Raise the daily budget",
        changes: [
          {
            field: "amount",
            label: "Daily budget",
            before: "9007199254740993000000",
            after: "9007199254740994000000",
            money: true,
          },
        ],
      },
    ],
    money: null,
  });
  draw(card);
  expect(screen.getByText("¥9,007,199,254,740,993")).toBeTruthy();
  expect(screen.getByText("¥9,007,199,254,740,994")).toBeTruthy();
});

test("the agent's reason and the vendor's findings are plain text, never Markdown", () => {
  draw(spend({ findings: ["Destination not working: **https://example.com**"] }));
  expect(
    screen.getByText("It spent its **whole** budget by 14:00 on each of the last 5 days."),
  ).toBeTruthy();
  expect(screen.getByText("Destination not working: **https://example.com**")).toBeTruthy();
  expect(document.querySelector("strong")).toBeNull();
});

test("another open ask on the same thing is said, since allowing one voids the other", () => {
  draw(spend({ other_open_asks: 1 }));
  expect(
    screen.getByText(/Another open ask changes the same thing: allowing this one voids it\./),
  ).toBeTruthy();
});

test("a batch of ten replies shows every reply whole, under the review it answers, with its length against the limit", () => {
  const reply = "x".repeat(349) + "!";
  const card: GrowthCard = {
    ...spend(),
    act: "public_post",
    product: "google_play",
    tool: "google_play_reply_to_reviews",
    account: {
      product: "google_ads",
      account: "com.example.notes",
      account_name: "Nimbus Notes",
      currency: "USD",
      time_zone: "America/Los_Angeles",
      state: "serving",
    },
    money: null,
    acts: Array.from({ length: 10 }, (_, index) => ({
      target: `review-${index}`,
      summary: "Reply to a review",
      changes: [],
      text: reply,
      text_limit: 350,
      target_excerpt: `Review ${index} says the app logs out.`,
      target_rating: (index % 5) + 1,
      target_author: `Reader ${index}`,
    })),
  };
  draw(card, { question: "Reply to 10 reviews of Nimbus Notes?" });
  expect(screen.getAllByText(reply)).toHaveLength(10);
  expect(screen.getAllByText("350 of 350 characters")).toHaveLength(10);
  expect(screen.getAllByRole("img", { name: "Rated 3 out of 5" })).toHaveLength(2);
  expect(screen.getByText("Reader 9")).toBeTruthy();
  expect(screen.getByText("Reply to a review, 10 of 10")).toBeTruthy();
  expect(screen.getByText(/All 10 are published on/)).toBeTruthy();
});

test("an irreversible admin act is drawn in danger and held until the person confirms it", () => {
  const onAllow = vi.fn();
  const card: GrowthCard = {
    ...spend(),
    act: "account_admin",
    money: null,
    acts: [
      {
        target: "Old campaign",
        summary: "Delete the campaign",
        changes: [
          { field: "status", label: "Status", before: "PAUSED", after: "REMOVED", money: false },
        ],
        danger: true,
      },
    ],
  };
  draw(card, { question: "Delete the campaign Old campaign?", onAllow });
  const allow = screen.getByRole("button", { name: "Allow once" });
  expect(allow.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(screen.getByRole("checkbox", { name: /cannot be undone/ }));
  expect(screen.getByRole("button", { name: "Allow once" }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
});

test("an ask past its lapse is expired and offers no answer", () => {
  draw(spend({ lapses_at: new Date(Date.now() - 1000).toISOString() }));
  expect(screen.queryByRole("button", { name: "Allow once" })).toBeNull();
  expect(screen.getAllByText(/expired/i).length).toBeGreaterThan(0);
  expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
});

test("the note to the agent is the person's own, sent with either answer", () => {
  const onNote = vi.fn();
  draw(spend(), { onNote });
  fireEvent.change(screen.getByRole("textbox", { name: /Note to the agent/ }), {
    target: { value: "Keep it under €50." },
  });
  expect(onNote).toHaveBeenCalledWith("Keep it under €50.");
});

test("an account named as its own ID is said once, and the rows end with the reason", () => {
  expect(
    accountLine(
      spend({
        account: {
          product: "google_ads",
          account: "act_9",
          account_name: "act_9",
          currency: "KWD",
          time_zone: "Asia/Kuwait",
          state: "serving",
        },
      }),
    ),
  ).toBe("act_9 · KWD · Asia/Kuwait");
  const rows = growthCardRows(spend());
  expect(rows[0]).toMatchObject({ kind: "fact", label: "Account" });
  expect(rows.at(-1)).toMatchObject({ kind: "fact", label: "The agent's reason" });
  expect(within(document.body).queryByText("act_9")).toBeNull();
});

test("an amount whose currency nobody named is never drawn as bare micros, and cannot be allowed here", () => {
  const card = spend({ money: null });
  card.account = { ...card.account, currency: "" };
  draw(card);
  expect(screen.queryByText(/40000000|60000000/)).toBeNull();
  expect(screen.getAllByText("An amount in an unnamed currency")).toHaveLength(2);
  expect(
    screen.getAllByText(
      /did not say which currency the amounts are in, so this cannot be allowed here/,
    ).length,
  ).toBeGreaterThan(0);
  expect(screen.getByRole("button", { name: "Allow once" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
  expect(screen.getByRole("button", { name: "Deny" }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
});

test("a change that lowers the daily budgets says so, and draws only what stays set", () => {
  draw(
    spend({
      money: {
        currency: "EUR",
        delta_micros: "-5000000",
        daily_micros: "140000000",
        daily_after_micros: "135000000",
        per_change_cap_micros: "50000000",
        daily_cap_micros: "200000000",
      },
    }),
  );
  expect(screen.getByText(/This change lowers the daily budgets by €5\.00\./)).toBeTruthy();
  expect(screen.queryByText(/adds -€5\.00|adds −€5\.00/)).toBeNull();
});

test("Allow once held for a denial on its way says why beside it", () => {
  draw(spend(), { held: "Sending your denial." });
  expect(screen.getAllByText("Sending your denial.").length).toBeGreaterThan(0);
  expect(screen.queryByText("Applying")).toBeNull();
});

test("a public act without text is drawn as the fields it changes, and a live delete waits for a tick", () => {
  const live: GrowthCard = {
    ...spend(),
    act: "public_post",
    product: "app_store_connect",
    money: null,
    acts: [
      {
        target: "Nimbus Notes, version 4.2",
        summary: "change AppInfoLocalizations",
        changes: [
          {
            field: "subtitle",
            label: "Subtitle",
            before: "Notes that sync",
            after: "Notes for teams",
          },
        ],
      },
      { target: "Spring event", summary: "delete AppEvents", changes: [], danger: true },
    ],
  };
  draw(live);
  expect(screen.getByText("Notes that sync")).toBeTruthy();
  expect(screen.getByText("Notes for teams")).toBeTruthy();
  expect(screen.getByText(/Customers see all 2 on/)).toBeTruthy();
  expect(screen.queryByText(/where anyone can read/)).toBeNull();
  expect(screen.getByText("What will change (2)")).toBeTruthy();
  expect(screen.getByRole("checkbox", { name: /cannot be undone/ })).toBeTruthy();
});

test("a reply that replaces one shows the words it takes down", () => {
  const card: GrowthCard = {
    ...spend(),
    act: "public_post",
    money: null,
    acts: [
      {
        target: "review 1",
        summary: "reply to a review",
        text: "Fixed in 4.2, thank you.",
        target_author: "A reader",
        changes: [
          {
            field: "responseBody",
            label: "Reply",
            before: "We are looking into it.",
            after: "Fixed in 4.2, thank you.",
          },
        ],
      },
    ],
  };
  draw(card);
  expect(screen.getByText("We are looking into it.")).toBeTruthy();
  expect(screen.getByText("Fixed in 4.2, thank you.")).toBeTruthy();
});
