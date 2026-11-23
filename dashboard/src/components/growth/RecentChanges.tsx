/**
 * Recent agent changes: every step of the agents' growth acts on one product,
 * newest first, as the access log keeps it. A change made at operate (a pause,
 * a lowered budget) is listed here rather than asked; an act held for a
 * decision shows who answered it and what came of it. An administrator's read,
 * since the access log is (`growth_changes`).
 *
 * EVERY ROW READ HERE IS KEPT, BY RECORD. The newest page is read again each
 * minute and each older page once, on request; a step is one record (its act
 * and step), so a row read twice is drawn once, and a row a newer change pushed
 * off the newest page stays where the reader saw it.
 *
 * Published text is never on the wire, only its size; money is drawn in the
 * account's currency through the kit's `Money`, and never as bare micros.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { Button, Money, RelativeTime, Text } from "@crewlethq/ui";
import type {
  GrowthChangeField,
  GrowthChangeRow,
  GrowthFailure,
  GrowthStep,
} from "~/contract/growth.ts";
import { useOrgIndex } from "~/lib/seats.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import type { CapAccount } from "./caps.ts";
import "~/styles/growth.css";

/** How often the newest changes are read again: an agent's act lands within a minute of being made. */
const CHANGES_POLL_MS = 60_000;

const STEP_WORDS: Readonly<Record<GrowthStep, string>> = {
  made: "Made",
  asked: "Asked for a decision",
  allowed: "Allowed",
  denied: "Denied",
  lapsed: "Lapsed unanswered",
  applied: "Applied",
  void: "Not run: what it changes moved",
  failed: "Failed",
};

const FAILURE_WORDS: Readonly<Record<Exclude<GrowthFailure, "">, string>> = {
  vendor_refused: "the vendor refused it",
  unauthorized: "the vendor no longer lets Crewlet in",
  rate_limited: "the vendor asked to wait",
  vendor_unavailable: "the vendor did not answer",
  timed_out: "it took too long",
  unknown_outcome: "it may or may not have landed",
  cap_exceeded: "it would pass a spending cap",
  access_lowered: "the agent's access was lowered",
  engine_error: "Crewlet could not finish it",
};

/** What a step came to, in words: who answered a held act (by name), and why a failed one did not run. */
export function stepWords(row: GrowthChangeRow, nameOf: (handle: string) => string): string {
  const step = STEP_WORDS[row.step] ?? row.step;
  if ((row.step === "allowed" || row.step === "denied") && row.answerer)
    return `${step} by ${nameOf(row.answerer)}`;
  if (row.step === "failed" && row.failure)
    return `${step}: ${FAILURE_WORDS[row.failure] ?? row.failure}`;
  return step;
}

/** A changed field's value: money in its currency, published text by its size, else as written. */
function value(change: GrowthChangeField, side: "before" | "after", currency: string | undefined) {
  if (change.text) {
    // THE WIRE SENDS A SIZE FOR EVERY TEXT FIELD, 0 where there was none.
    const bytes = (side === "before" ? change.before_bytes : change.after_bytes) ?? 0;
    return bytes > 0
      ? `published text, ${bytes.toLocaleString()} byte${bytes === 1 ? "" : "s"}`
      : "nothing";
  }
  const written = change[side] ?? "";
  if (written === "") return "nothing";
  if (!change.money) return written;
  // NEVER BARE MICROS: a figure a million times the amount is worse than none.
  return currency ? (
    <Money micros={written} currency={currency} />
  ) : (
    "an amount in an unnamed currency"
  );
}

/** A row's currency: its spend figures', else the one the page knows for its account. */
function currencyOf(
  row: GrowthChangeRow,
  accounts: ReadonlyMap<string, CapAccount>,
): string | undefined {
  const code = (row.money?.currency || accounts.get(row.account)?.currency || "")
    .trim()
    .toUpperCase();
  return /^[A-Z]{3}$/.test(code) ? code : undefined;
}

/**
 * An account by the name the vendor gives it, its ID after it, or its ID
 * alone where the page knows no name: an Apple ID tells a reader nothing.
 */
export function accountWords(id: string, accounts: ReadonlyMap<string, CapAccount>): string {
  const name = accounts.get(id)?.name.trim() ?? "";
  return name && name !== id ? `${name} (${id})` : id;
}

/** One record of the log: an act's step. */
const recordOf = (row: GrowthChangeRow) => `${row.act}/${row.step}`;

/** Where one page's read stands. */
interface PageState {
  read: boolean;
  next: string;
  /** False where this node could not read every record, so the page may be missing some. */
  complete: boolean;
  error: QueryErrorCode | null;
}

/** When a change was made: short, as it stands alone in its row's head. */
function When({ at }: { at: string }) {
  const now = useNow();
  return <RelativeTime value={at} now={now} title={fmtDateTime(at)} />;
}

/** One page's read, handed up as it lands: the newest page polled, an older one once. */
function PageRead({
  product,
  cursor,
  onRead,
}: {
  product: string;
  cursor: string;
  onRead: (cursor: string, state: PageState, rows: readonly GrowthChangeRow[]) => void;
}) {
  const read = useQuery(
    "growth_changes",
    cursor ? { product, cursor } : { product },
    cursor ? {} : { pollMs: CHANGES_POLL_MS },
  );
  useEffect(() => {
    onRead(
      cursor,
      {
        read: read.data !== null,
        next: read.data?.next ?? "",
        complete: read.data?.complete ?? true,
        error: read.error,
      },
      read.data?.rows ?? [],
    );
  }, [cursor, onRead, read.data, read.error]);
  return null;
}

export function RecentChanges({
  product,
  accounts,
}: {
  /** The product whose changes are listed (`google_ads`). */
  product: string;
  /** Each account the product reaches, by ID: its name and currency. */
  accounts: ReadonlyMap<string, CapAccount>;
}) {
  const index = useOrgIndex();
  // A seat by the name the chart gives it, or its handle for a seat no longer there.
  const nameOf = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const [seen, setSeen] = useState<ReadonlyMap<string, GrowthChangeRow>>(new Map());
  const [pages, setPages] = useState<Readonly<Record<string, PageState>>>({});
  // The older pages asked for, oldest last: each one's cursor is the page before's `next`.
  const [older, setOlder] = useState<readonly string[]>([]);
  const onRead = useCallback(
    (cursor: string, state: PageState, rows: readonly GrowthChangeRow[]) => {
      setPages((current) => ({ ...current, [cursor]: state }));
      if (rows.length === 0) return;
      setSeen((current) => {
        const next = new Map(current);
        for (const row of rows) next.set(recordOf(row), row);
        return next;
      });
    },
    [],
  );
  const rows = useMemo(
    () =>
      [...seen.values()].sort(
        (a, b) => Date.parse(b.at) - Date.parse(a.at) || recordOf(a).localeCompare(recordOf(b)),
      ),
    [seen],
  );
  const newest = pages[""];
  const last = pages[older.at(-1) ?? ""];
  const partial = Object.values(pages).some((page) => page.read && !page.complete);
  const readers = (
    <>
      <PageRead product={product} cursor="" onRead={onRead} />
      {older.map((cursor) => (
        <PageRead key={cursor} product={product} cursor={cursor} onRead={onRead} />
      ))}
    </>
  );
  if (rows.length === 0) {
    const words =
      newest?.error === "unauthorized"
        ? "Only an administrator reads the agents’ changes."
        : newest?.error
          ? "The agents’ changes could not be read. They are read again within a minute."
          : newest?.read
            ? "No agent has changed anything here yet."
            : "Reading the agents’ changes.";
    return (
      <>
        {readers}
        <Text as="p" tone="secondary">
          {words}
        </Text>
      </>
    );
  }
  return (
    <div className="growth-changes">
      {readers}
      {partial ? (
        <Text as="p" tone="secondary">
          Some changes could not be read on this node, so this list may be missing a few.
        </Text>
      ) : null}
      {/* A STYLED LIST KEEPS ITS ROLE: a list without markers loses it in some browsers. */}
      <ol className="growth-changes__list" role="list">
        {rows.map((row) => (
          <li key={recordOf(row)} className="growth-change">
            <span className="growth-change__head">
              {/* AN ACT ON THE ACCOUNT ITSELF (an app) is titled by its name. */}
              <span className="growth-change__what">
                {(row.target === row.account && accounts.get(row.account)?.name.trim()) ||
                  row.target ||
                  row.tool}
              </span>
              <span className="growth-change__when">
                <When at={row.at} />
              </span>
            </span>
            <span className="growth-change__step">{stepWords(row, nameOf)}</span>
            <span className="growth-change__where">
              {accountWords(row.account, accounts)} · by {nameOf(row.seat)}
            </span>
            {row.changes.length > 0 ? (
              <ul className="growth-change__fields">
                {row.changes.map((change) => (
                  <li key={change.field}>
                    {change.label || change.field}:{" "}
                    {value(change, "before", currencyOf(row, accounts))} →{" "}
                    {value(change, "after", currencyOf(row, accounts))}
                  </li>
                ))}
              </ul>
            ) : null}
          </li>
        ))}
      </ol>
      {/* ONCE THE READER PAGES BACK THE BUTTON STAYS, held at the end, so focus is never dropped. */}
      {newest?.next || older.length > 0 ? (
        <Button
          size="small"
          variant="secondary"
          loading={older.length > 0 && (last === undefined || (!last.read && last.error === null))}
          disabledReason={
            last?.error
              ? "The older changes could not be read. They are asked for again shortly."
              : last?.read && !last.next
                ? "These are all the changes."
                : undefined
          }
          tooltip
          onClick={() => {
            const next = last?.next ?? "";
            if (next && !older.includes(next)) setOlder([...older, next]);
          }}
        >
          Show older changes
        </Button>
      ) : null}
    </div>
  );
}
