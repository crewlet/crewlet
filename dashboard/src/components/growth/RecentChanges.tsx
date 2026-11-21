/**
 * Recent agent changes: every step of the agents' growth acts on one product,
 * newest first, as the access log keeps it. A change made at operate (a pause,
 * a lowered budget) is listed here rather than asked; an act held for a
 * decision shows who answered it and what came of it. An administrator's read,
 * since the access log is (`growth_changes`).
 *
 * Published text is never on the wire, only its length; money is drawn in the
 * account's currency through the kit's `Money`, and never as bare micros.
 */

import { useMemo, useState } from "react";
import { Button, Money, Text } from "@crewlethq/ui";
import type {
  GrowthChangeField,
  GrowthChangeRow,
  GrowthFailure,
  GrowthStep,
} from "~/contract/growth.ts";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
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

/** What a step came to, in words: who answered a held act, and why a failed one did not run. */
export function stepWords(row: GrowthChangeRow): string {
  const step = STEP_WORDS[row.step] ?? row.step;
  if ((row.step === "allowed" || row.step === "denied") && row.answerer)
    return `${step} by ${row.answerer}`;
  if (row.step === "failed" && row.failure)
    return `${step}: ${FAILURE_WORDS[row.failure] ?? row.failure}`;
  return step;
}

/** A changed field's value: money in its currency, published text by its length, else as written. */
function value(change: GrowthChangeField, side: "before" | "after", currency: string | undefined) {
  if (change.text) {
    const bytes = side === "before" ? change.before_bytes : change.after_bytes;
    return bytes === undefined ? "nothing" : `published text, ${bytes.toLocaleString()} bytes`;
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
  accounts: ReadonlyMap<string, string>,
): string | undefined {
  const code = (row.money?.currency || accounts.get(row.account) || "").trim().toUpperCase();
  return /^[A-Z]{3}$/.test(code) ? code : undefined;
}

export function RecentChanges({
  product,
  accounts,
}: {
  /** The product whose changes are listed (`google_ads`). */
  product: string;
  /** Each account the product reaches, by ID, to its currency. */
  accounts: ReadonlyMap<string, string>;
}) {
  const now = useNow();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  // The agent by the name the chart gives it, or its handle for a seat no longer there.
  const nameOf = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const [cursor, setCursor] = useState("");
  const [seen, setSeen] = useState<GrowthChangeRow[]>([]);
  const read = useQuery("growth_changes", cursor ? { product, cursor } : { product }, {
    // The newest page is read again; an older one the reader asked for holds still.
    ...(cursor ? {} : { pollMs: CHANGES_POLL_MS }),
  });
  const rows = [...seen, ...(read.data?.rows ?? [])];
  if (read.error && rows.length === 0)
    return (
      <Text as="p" tone="secondary">
        The agents&rsquo; changes could not be read. They are read again within a minute.
      </Text>
    );
  if (!read.data && rows.length === 0)
    return (
      <Text as="p" tone="secondary">
        Reading the agents&rsquo; changes.
      </Text>
    );
  if (rows.length === 0)
    return (
      <Text as="p" tone="secondary">
        No agent has changed anything here yet.
      </Text>
    );
  return (
    <div className="growth-changes">
      <ol className="growth-changes__list">
        {rows.map((row, at) => (
          <li key={`${row.act}/${row.step}/${row.at}/${at}`} className="growth-change">
            <span className="growth-change__head">
              <span className="growth-change__what">{row.target || row.tool}</span>
              <span className="growth-change__when">{relTime(row.at, now)}</span>
            </span>
            <span className="growth-change__step">{stepWords(row)}</span>
            <span className="growth-change__where">
              {row.account} · by {nameOf(row.seat)}
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
      {read.data && !read.data.complete && read.data.next ? (
        <Button
          size="small"
          variant="secondary"
          onClick={() => {
            setSeen(rows);
            setCursor(read.data!.next);
          }}
        >
          Show older changes
        </Button>
      ) : null}
    </div>
  );
}
