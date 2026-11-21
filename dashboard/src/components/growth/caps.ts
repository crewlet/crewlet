/**
 * A spending product's caps, read from the company document and written back
 * as one merge patch: the rows a person edits, what each row says about the
 * caps it holds, and the change a save makes.
 *
 * AMOUNTS STAY TEXT. A cap is a decimal string in the account's currency, read
 * by the kit's `parseMoney` into exact micros to check and compare, and
 * written back as the decimal `moneyDecimal` makes, so nothing passes through
 * a float.
 */

import { moneyDecimal, moneyFieldProblem, parseMoney } from "@crewlethq/ui";
import type { GrowthSpend, GrowthSpendAccount } from "~/contract/growth.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import type { SettingPlan } from "~/lib/useSettingWrite.ts";

/** One account's caps as a person edits them. */
export interface CapRow {
  /** Stable while the form is open, so a row keeps its fields as rows above it are removed. */
  key: string;
  account: string;
  currency: string;
  perChange: string;
  daily: string;
  /** Whether the row was in the document when the form opened. */
  stored: boolean;
  /** Whether its account is fixed: stored, or picked from the vendor's accounts. */
  fixed: boolean;
  /** Whether its currency is fixed: a code, and the account's own where the vendor reports one. */
  currencyFixed: boolean;
}

/** An account the product's connections reach, as the vendor reports it. */
export interface CapAccount {
  id: string;
  name: string;
  currency: string;
}

const CODE = /^[A-Za-z]{3}$/;

/** The caps block of `kind` in `document`, or an empty one. */
export function spendOf(document: CompanyDocument, kind: string): GrowthSpend {
  const block = document.integrations?.[kind];
  if (block === null || typeof block !== "object") return {};
  const spend = (block as { spend?: unknown }).spend;
  return spend !== null && typeof spend === "object" ? (spend as GrowthSpend) : {};
}

/**
 * The rows a form opens with: every account the document caps, in its order.
 * A stored currency that is not a code, or not the account's own, stays
 * editable, so the row can be put right in place.
 */
export function rowsOf(spend: GrowthSpend, known: readonly CapAccount[]): CapRow[] {
  return (spend.accounts ?? []).map((row, index) => {
    const currency = (row.currency ?? "").trim().toUpperCase();
    const live = liveCurrency(known, row.account);
    return {
      key: `stored-${index}`,
      account: row.account,
      currency: row.currency ?? "",
      perChange: row.per_change ?? "",
      daily: row.daily ?? "",
      stored: true,
      fixed: true,
      currencyFixed: CODE.test(currency) && (live === "" || live === currency),
    };
  });
}

/** The currency the vendor reports for an account, upper case; empty where it reports none. */
export function liveCurrency(known: readonly CapAccount[], account: string): string {
  return (known.find((candidate) => candidate.id === account.trim())?.currency ?? "")
    .trim()
    .toUpperCase();
}

/** A problem with one field of a row, or a note about the row as a whole. */
export interface CapProblems {
  account?: string;
  currency?: string;
  perChange?: string;
  daily?: string;
  /** Not a refusal: what the caps mean as they stand. */
  note?: string;
}

/**
 * What is wrong with each row, in the order given, and whether the form can be
 * saved. A row may leave both caps empty (spending stays refused on it) but
 * not just one: half a pair is a cap a person believes is set and is not.
 */
export function checkRows(
  rows: readonly CapRow[],
  known: readonly CapAccount[],
): { problems: CapProblems[]; ok: boolean } {
  const seen = new Map<string, number>();
  const problems = rows.map((row, index): CapProblems => {
    const out: CapProblems = {};
    const account = row.account.trim();
    if (!account) out.account = "Name the account by its ID at the vendor.";
    else if (seen.has(account)) out.account = "This account already has a row.";
    else seen.set(account, index);
    const currency = row.currency.trim().toUpperCase();
    if (!CODE.test(currency))
      out.currency = "Type the account's currency as its three-letter code.";
    if (out.currency) return out;
    for (const [field, text] of [
      ["perChange", row.perChange],
      ["daily", row.daily],
    ] as const) {
      if (text.trim() === "") continue;
      const problem = moneyFieldProblem(text, currency);
      if (problem) out[field] = problem;
    }
    if (out.perChange || out.daily) return out;
    const per = row.perChange.trim();
    const daily = row.daily.trim();
    if ((per === "") !== (daily === "")) {
      out[per === "" ? "perChange" : "daily"] =
        "Set both caps, or neither: agents may ask to spend only with both.";
      return out;
    }
    // AN ACCOUNT THE VENDOR REPORTS NO CURRENCY FOR is checked by the engine once it does.
    const live = liveCurrency(known, account);
    if (live !== "" && live !== currency) {
      out.currency = `The account is in ${live}. Caps in another currency refuse every request to spend.`;
      return out;
    }
    if (per === "") {
      out.note = "No caps: agents cannot ask to spend on this account.";
    } else if (micros(per, currency) > micros(daily, currency)) {
      // The engine refuses it too: one change could never pass.
      out.perChange = "More than the daily cap: one change could never pass.";
    }
    return out;
  });
  const ok = problems.every((p) => !p.account && !p.currency && !p.perChange && !p.daily);
  return { problems, ok };
}

function micros(text: string, currency: string): bigint {
  const read = parseMoney(text, currency);
  return "micros" in read ? read.micros : 0n;
}

/** A cap at the currency's own digits ("50" is "50.00" in EUR), or as written where it is not an amount. */
function canonicalAmount(text: string, currency: string): string {
  // A row written without its currency keeps its amount as written: it cannot be read.
  if (!CODE.test(currency)) return text;
  const read = parseMoney(text.trim(), currency);
  return "micros" in read ? moneyDecimal(read.micros, currency) : text;
}

/** A document row as the document would store it, so two spellings of one amount compare equal. */
function canonical(row: GrowthSpendAccount): GrowthSpendAccount {
  const currency = (row.currency ?? "").trim().toUpperCase();
  const out: GrowthSpendAccount = { account: row.account.trim(), currency };
  if (row.per_change !== undefined && row.per_change.trim() !== "")
    out.per_change = canonicalAmount(row.per_change, currency);
  if (row.daily !== undefined && row.daily.trim() !== "")
    out.daily = canonicalAmount(row.daily, currency);
  return out;
}

/** A row as the document stores it: the code upper case, each cap at the currency's own digits. */
export function storedRow(row: CapRow): GrowthSpendAccount {
  return canonical({
    account: row.account,
    currency: row.currency,
    per_change: row.perChange,
    daily: row.daily,
  });
}

/**
 * The change a save makes: the product's `spend.accounts` replaced whole by
 * the rows. Nothing where the document holds them already, and a conflict
 * where the product's block is gone or its caps are no longer the `opened`
 * ones the form was filled from: another admin's save is reloaded, never
 * written over.
 */
export function capsPlan(
  kind: string,
  product: string,
  opened: readonly GrowthSpendAccount[],
  rows: readonly CapRow[],
): SettingPlan {
  const accounts = rows.map(storedRow);
  const want = JSON.stringify(accounts);
  const was = JSON.stringify(opened.map(canonical));
  return (document) => {
    const block = document.integrations?.[kind];
    if (block === null || typeof block !== "object") return "conflict";
    const now = JSON.stringify((spendOf(document, kind).accounts ?? []).map(canonical));
    if (now === want) return null;
    if (now !== was) return "conflict";
    return {
      patch: { integrations: { [kind]: { spend: { accounts } } } },
      summary: `Set the spending caps of ${product}`,
    };
  };
}
