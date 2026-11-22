/**
 * Spending caps go back to the document exactly as a person set them: both
 * caps or neither on an account, in the account's own currency, at the
 * currency's own digits, and never over a document that moved.
 */

import { expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/types.ts";
import { capsPlan, checkRows, rowsOf, spendOf, storedRow, type CapRow } from "./caps.ts";

const row = (extra: Partial<CapRow> = {}): CapRow => ({
  key: "r",
  account: "123-456-7890",
  currency: "EUR",
  perChange: "50",
  daily: "200",
  stored: false,
  fixed: false,
  currencyFixed: false,
  ...extra,
});

const doc = (spend: unknown): CompanyDocument =>
  ({ integrations: { google_ads: { enabled: true, spend } } }) as unknown as CompanyDocument;

test("a pair of caps is stored at the currency's own digits, its code as ISO writes it", () => {
  expect(storedRow(row())).toEqual({
    account: "123-456-7890",
    currency: "EUR",
    per_change: "50.00",
    daily: "200.00",
  });
  expect(storedRow(row({ currency: "jpy", perChange: "3400", daily: "10000" }))).toEqual({
    account: "123-456-7890",
    currency: "JPY",
    per_change: "3400",
    daily: "10000",
  });
  expect(storedRow(row({ currency: "KWD", perChange: "1.5", daily: "10" }))).toMatchObject({
    per_change: "1.500",
    daily: "10.000",
  });
});

test("half a pair is refused, neither is a row that refuses spending, and a duplicate account is refused", () => {
  const { problems, ok } = checkRows(
    [
      row({ key: "a", account: "111", daily: "" }),
      row({ key: "b", account: "999", perChange: "", daily: "" }),
      row({ key: "c" }),
      row({ key: "d" }),
    ],
    [],
  );
  expect(ok).toBe(false);
  expect(problems[0]!.daily).toMatch(/Set both caps, or neither/);
  expect(problems[1]).toEqual({ note: "No caps: agents cannot ask to spend on this account." });
  expect(problems[2]).toEqual({});
  expect(problems[3]!.account).toBe("This account already has a row.");
});

test("an amount the currency cannot hold, and a currency the account is not in, are refused", () => {
  const { problems } = checkRows(
    [
      row({ key: "a", perChange: "5.123" }),
      row({ key: "b", account: "act_1", currency: "USD" }),
      row({ key: "c", account: "x", currency: "EU" }),
    ],
    [{ id: "act_1", name: "Brand", currency: "EUR" }],
  );
  expect(problems[0]!.perChange).toBe("EUR takes at most 2 decimal places.");
  expect(problems[1]!.currency).toMatch(/The account is in EUR/);
  expect(problems[2]!.currency).toMatch(/three-letter code/);
});

test("a per-change cap above the daily cap is refused, as the engine refuses it", () => {
  const { problems, ok } = checkRows([row({ perChange: "300" })], []);
  expect(ok).toBe(false);
  expect(problems[0]).toEqual({
    perChange: "More than the daily cap: one change could never pass.",
  });
  expect(checkRows([row({ perChange: "200" })], []).ok).toBe(true);
});

test("an account the vendor reports no currency for takes the currency typed", () => {
  const { problems, ok } = checkRows(
    [row({ account: "act_1", currency: "USD" })],
    [{ id: "act_1", name: "Brand", currency: "" }],
  );
  expect(ok).toBe(true);
  expect(problems[0]).toEqual({});
});

test("a stored currency that is missing or not the account's own stays editable; a right one is fixed", () => {
  const known = [
    { id: "a", name: "A", currency: "EUR" },
    { id: "b", name: "B", currency: "EUR" },
    { id: "c", name: "C", currency: "" },
  ];
  const rows = rowsOf(
    {
      accounts: [
        { account: "a", currency: "eur", per_change: "1", daily: "2" },
        { account: "b", currency: "USD", per_change: "1", daily: "2" },
        { account: "c", per_change: "1", daily: "2" },
        { account: "d", currency: "GBP", per_change: "1", daily: "2" },
      ],
    },
    known,
  );
  expect(rows.map((r) => [r.fixed, r.currencyFixed])).toEqual([
    [true, true],
    [true, false],
    [true, false],
    [true, true],
  ]);
});

test("the plan replaces the accounts whole, and does nothing where the document already holds them", () => {
  const spend = {
    accounts: [{ account: "123-456-7890", currency: "EUR", per_change: "50", daily: "200.00" }],
  };
  expect(spendOf(doc(spend), "google_ads")).toEqual(spend);
  const rows = rowsOf(spend, []);
  const opened = spend.accounts;
  expect(capsPlan("google_ads", "Google Ads", opened, rows)(doc(spend))).toBeNull();
  const raised = capsPlan("google_ads", "Google Ads", opened, [{ ...rows[0]!, daily: "250" }])(
    doc(spend),
  );
  expect(raised).toEqual({
    patch: {
      integrations: {
        google_ads: {
          spend: {
            accounts: [
              { account: "123-456-7890", currency: "EUR", per_change: "50.00", daily: "250.00" },
            ],
          },
        },
      },
    },
    summary: "Set the spending caps of Google Ads",
  });
});

test("a save over a document whose block is gone is a conflict, never a write", () => {
  expect(
    capsPlan(
      "google_ads",
      "Google Ads",
      [],
      [row()],
    )({
      integrations: {},
    } as unknown as CompanyDocument),
  ).toBe("conflict");
});

test("a save over caps another admin changed since the form opened is a conflict, never a write over them", () => {
  const opened = [{ account: "111", currency: "EUR", per_change: "50", daily: "200" }];
  const since = {
    accounts: [...opened, { account: "456", currency: "EUR", per_change: "5", daily: "20" }],
  };
  const mine = [{ ...rowsOf({ accounts: opened }, [])[0]!, daily: "300" }];
  expect(capsPlan("google_ads", "Google Ads", opened, mine)(doc(since))).toBe("conflict");
  // The same caps written the same way in another spelling are no change.
  expect(
    capsPlan(
      "google_ads",
      "Google Ads",
      opened,
      mine,
    )(doc({ accounts: [{ account: "111", currency: "eur", per_change: "50.00", daily: "200" }] })),
  ).not.toBe("conflict");
});

test("a document row written without its currency is compared as written, never thrown on", () => {
  const spend = { accounts: [{ account: "123-456-7890", per_change: "50", daily: "200" }] };
  const rows = rowsOf(spend, []);
  expect(rows[0]!.currency).toBe("");
  expect(() =>
    capsPlan("google_ads", "Google Ads", spend.accounts, [{ ...rows[0]!, currency: "EUR" }])(
      doc(spend),
    ),
  ).not.toThrow();
});
