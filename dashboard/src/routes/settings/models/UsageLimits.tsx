/**
 * A model's usage limits, as the engine reads its sign-in's windows (`limits`):
 * the list's cell and the model page's card, drawn with the kit's `LimitMeters`.
 */

import { useId } from "react";
import { Card, DescriptionList, LimitMeters, Text, Tooltip, type LimitWindow } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { ago } from "~/lib/ago.ts";
import { fmtAt, fmtClock, parseUTC } from "~/lib/format.ts";
import { dateTimeFormat } from "~/lib/formatters.ts";
import { zone } from "~/lib/prefs.ts";
import type { ModelLimit } from "~/protocol/models.ts";
import type { ModelView } from "./data.ts";
import { LIMIT_METER, LIMIT_STATE, LIMIT_WINDOW, NO_LIMITS } from "./words.ts";

const HOURS: Record<number, string> = { 5: "5-hour", 24: "Daily", 168: "Weekly" };

/** A window's name: its length where the vendor gives one, then its model ("Weekly Opus"). */
export function limitLabel(l: ModelLimit): string {
  const family = l.model ? l.model.charAt(0).toUpperCase() + l.model.slice(1) : "";
  const span = l.hours > 0 ? (HOURS[l.hours] ?? `${l.hours}-hour`) : "";
  if (span) return family ? `${span} ${family}` : span;
  return family || LIMIT_WINDOW[l.window];
}

/** An instant with its zone, for the exact figures: "Wed, Nov 18, 05:00 UTC". */
function exactAt(ts: string, now: number): string {
  const d = parseUTC(ts);
  const name = d
    ? dateTimeFormat(undefined, zone(), { timeZoneName: "short" })
        .formatToParts(d)
        .find((p) => p.type === "timeZoneName")?.value
    : undefined;
  return name ? `${fmtAt(ts, now)} ${name}` : fmtAt(ts, now);
}

/** When a window turns over, in a few words, or with the exact instant. */
function whenWords(l: ModelLimit, now: number, exact: boolean): string | undefined {
  const at = (ts: string) => (exact ? exactAt(ts, now) : fmtClock(ts, now));
  if (l.until) return `${exact ? "sitting out " : ""}until ${at(l.until)}`;
  if (!l.resets_at) return undefined;
  return (parseUTC(l.resets_at)?.getTime() ?? 0) > now
    ? `resets ${at(l.resets_at)}`
    : `reset at ${at(l.resets_at)}`;
}

const pct = (used: number) => `${Math.round(used * 100)}%`;

const STATE_WORDS = { ok: "OK", warning: "near its limit", reached: "reached" } as const;

/** The figure in words: the percentage used, or that the window turned over; "" where neither is known. */
function figureWords(l: ModelLimit): string {
  if (l.used !== null) return `${pct(l.used)} used`;
  return l.resets_at && !l.until && l.state === "ok" ? "turned over since it was last heard" : "";
}

/** How much of a window is used, in words: its figure, else its state. */
function usedWords(l: ModelLimit): string {
  return (
    figureWords(l) ||
    { ok: "within its limit", warning: "near its limit", reached: "reached" }[l.state]
  );
}

/** One window as the kit draws it, named by its whole sentence; no reset note (card only). */
export function limitWindow(l: ModelLimit, now: number): LimitWindow {
  const label = limitLabel(l);
  const when = whenWords(l, now, true);
  return {
    id: `${l.window}/${l.model ?? ""}`,
    label,
    state: LIMIT_METER[l.state],
    used: l.used ?? undefined,
    figure: l.used === null ? LIMIT_STATE[l.state] : undefined,
    name: [
      `${label} limit`,
      STATE_WORDS[l.state],
      figureWords(l),
      when,
      l.stale ? `as of ${ago(l.seen, now, { since: true })}` : "",
    ]
      .filter(Boolean)
      .join(", "),
  };
}

/** "as of 2 hours ago" for the oldest stale window, where any is stale. */
function staleCaption(limits: readonly ModelLimit[], now: number): string | undefined {
  const stale = limits.filter((l) => l.stale).map((l) => l.seen);
  if (stale.length === 0) return undefined;
  return `as of ${ago(stale.sort()[0], now, { since: true })}`;
}

/** Each window's use and exact reset on a line (the tooltip's `lines`), then when heard. */
function LimitDetail({ limits, now }: { limits: readonly ModelLimit[]; now: number }) {
  const heard = new Set(limits.map((l) => `Heard ${ago(l.seen, now)}`));
  const each = heard.size > 1;
  return (
    <>
      {limits.map((l) => (
        <span key={`${l.window}/${l.model ?? ""}`}>
          <strong>{limitLabel(l)}</strong>:{" "}
          {[usedWords(l), whenWords(l, now, true), each ? `heard ${ago(l.seen, now)}` : ""]
            .filter(Boolean)
            .join(", ")}
        </span>
      ))}
      {each ? null : <span>{[...heard][0]}</span>}
    </>
  );
}

/**
 * The list's cell: one line per window, the exact figures, reset times and
 * staleness on hover and as the link's description, with no caption under the
 * bars. A raised link to the model's page, as Used by's is.
 */
export function UsageLimitsCell({ model }: { model: ModelView }) {
  const now = useNow();
  const describedBy = useId();
  const limits = model.entry.limits;
  if (limits.length === 0)
    return <LimitMeters label={`Usage limits of ${model.key}`} windows={[]} empty={NO_LIMITS} />;
  return (
    <Tooltip lines content={<LimitDetail limits={limits} now={now} />} placement="top">
      <a
        className="models-limits"
        href={href(["settings", "models", model.key])}
        aria-describedby={describedBy}
      >
        <LimitMeters
          label={`Usage limits of ${model.key}`}
          windows={limits.map((l) => limitWindow(l, now))}
        />
        <span id={describedBy} hidden>
          {limits.map((l) => limitWindow(l, now).name).join("; ")}
        </span>
      </a>
    </Tooltip>
  );
}

/** The model page's card: each window's bar with its reset beneath, then every exact figure. */
export function UsageLimitsCard({ model }: { model: ModelView }) {
  const now = useNow();
  const limits = model.entry.limits;
  return (
    <Card padding="none">
      <Card.Header>
        <Card.Title>Usage limits</Card.Title>
      </Card.Header>
      <Card.Body>
        {limits.length === 0 ? (
          <Text as="p" variant="body" tone="secondary">
            {NO_LIMITS}. The harness has not said where its plan's windows stand: an API key, a
            local model or a harness that writes no limit report says none.
          </Text>
        ) : (
          <div className="models-limits-card">
            <LimitMeters
              label={`Usage limits of ${model.key}`}
              windows={limits.map((l) => ({
                ...limitWindow(l, now),
                note: whenWords(l, now, false),
              }))}
              caption={staleCaption(limits, now)}
            />
            <DescriptionList
              dense
              items={limits.map((l) => [
                limitLabel(l),
                [usedWords(l), whenWords(l, now, true), `heard ${ago(l.seen, now)}`]
                  .filter(Boolean)
                  .join(", "),
              ])}
            />
          </div>
        )}
      </Card.Body>
    </Card>
  );
}
