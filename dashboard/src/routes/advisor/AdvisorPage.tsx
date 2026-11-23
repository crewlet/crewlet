/**
 * The Advisor's page (`#/advisor`): the company's numbers, what is worth
 * changing with the change that fixes each, its weekly reports, and what was
 * found before.
 *
 * # Anatomy
 *
 * THE HEAD says when every check last ran, and while a run is going how long
 * it has run and what it is doing (`Run.tsx`), with Run and the range the
 * numbers cover.
 *
 * FINDINGS, the first tab, opens on the NUMBERS: a strip of figures, each
 * against the previous range and linked to the finding that explains it, and
 * charts of the same range (`Numbers.tsx`, `view.ts`). Then WHAT TO CHANGE,
 * most costly first (Blocks, Costs, Worth knowing; within each, the largest
 * measured cost, then the newest): each row one line of what is wrong, one of
 * why, its numbers, and ONE press that says what it does (`rows.tsx`). Apply
 * re-checks the finding and confirms in place, under its row
 * (`ApplyPanel.tsx`). Then what the engine is handling, what a person
 * snoozed, and, while anything blocks, the review of what each agent can do,
 * folded.
 *
 * REPORTS, the second tab: each week's report, newest first, a page at a
 * time. HISTORY, the third: what was found and closed, and why it closed.
 */

import { useCallback, useEffect, useState } from "react";
import {
  AttentionList,
  Button,
  Callout,
  EmptyState,
  List,
  ListItem,
  Skeleton,
  Tag,
  Text,
  type Tone,
} from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtDate, readerDay, relTime } from "~/lib/format.ts";
import { dateTimeFormat } from "~/lib/formatters.ts";
import { zone } from "~/lib/prefs.ts";
import { shiftDay } from "~/lib/timeline.ts";
import { GoLiveReview } from "./GoLiveReview.tsx";
import { ApplyPanel } from "./ApplyPanel.tsx";
import { useFindings } from "./api.ts";
import { useAskAbout, useDecisions } from "./decisions.tsx";
import { Charts, RangePicker, StatsStrip, useSnapshot } from "./Numbers.tsx";
import { RunHead } from "./Run.tsx";
import { pagedOn } from "./paged.ts";

export { pagedOn };
import { Reports } from "./Reports.tsx";
import { AtlasFix, listItems, snoozed, type OpenApply, type RowActions } from "./rows.tsx";
import type { AdvisorKeptFinding } from "~/protocol/advisorFindings.ts";
import { CLOSE_REASONS, SEVERITIES, STATE_WORDS, ranked } from "./words.ts";
import type { RangeDays } from "./view.ts";

/** The Advisor page's tabs, as its address names them. */
export type AdvisorSection = "findings" | "reports" | "history";

export function AdvisorPage({ section }: { section: AdvisorSection }) {
  // A RUN THAT ENDED is read by the list under it: its key moves, so it reads again.
  const [runs, setRuns] = useState(0);
  const ended = useCallback(() => setRuns((n) => n + 1), []);
  const [days, setDays] = useState<RangeDays>(7);
  return (
    <div className="col gap-4">
      <RunHead onEnded={ended}>
        {section === "findings" ? <RangePicker days={days} onChange={setDays} /> : null}
      </RunHead>
      {section === "findings" ? <Findings key={runs} days={days} /> : null}
      {section === "reports" ? <Reports /> : null}
      {section === "history" ? <History /> : null}
    </div>
  );
}

/** The row actions every list on the page shares, the Apply open in place, and the atlas dialog. */
function useRowActions(reload: () => void) {
  const askAbout = useAskAbout();
  const decisions = useDecisions(reload);
  const [open, setOpen] = useState<OpenApply | null>(null);
  const [reviewing, setReviewing] = useState<AdvisorKeptFinding | null>(null);
  const actions: RowActions = {
    apply: (f, option) => setOpen({ id: f.id, option }),
    review: setReviewing,
    ask: askAbout,
    snooze: decisions.snooze,
    wake: decisions.wake,
    dismiss: decisions.dismiss,
  };
  const applyFor = (f: AdvisorKeptFinding, option: number) => (
    <ApplyPanel
      key={`${f.id}:${option}`}
      finding={f}
      option={option}
      onClose={() => setOpen(null)}
      onSettled={reload}
    />
  );
  const dialog = (
    <AtlasFix
      finding={reviewing}
      onClose={() => {
        setReviewing(null);
        reload();
      }}
    />
  );
  return { actions, open, applyFor, dialog };
}

function Findings({ days }: { days: RangeDays }) {
  const open = useFindings("open");
  const now = useNow();
  const { refetch } = open;
  const reload = useCallback(() => refetch(), [refetch]);
  const row = useRowActions(reload);
  const all = ranked(open.data?.findings ?? []);
  const { snapshot, loading } = useSnapshot(days);
  // THE LISTS WAIT FOR THE NUMBERS' FIRST ANSWER: drawn under charts still
  // loading, they would move when the charts land. A new range keeps them.
  const [numbered, setNumbered] = useState(false);
  useEffect(() => {
    if (!loading) setNumbered(true);
  }, [loading]);
  // A SNOOZED FINDING LEAVES THE LISTS it would stand in, for one of its own
  // that says until when.
  const shown = all.filter((f) => !snoozed(f, now));
  const asleep = all.filter((f) => snoozed(f, now));
  const engine = shown.filter((f) => f.actor === "engine");
  const change = shown.filter((f) => f.actor === "owner");
  const blocked = all.some((f) => f.actor === "owner" && f.severity === "blocks");
  const items = (list: readonly AdvisorKeptFinding[]) =>
    listItems(list, now, row.actions, row.open, row.applyFor);
  return (
    <>
      <StatsStrip snapshot={snapshot} findings={all} loading={loading} />
      <Charts snapshot={snapshot} findings={all} now={now} loading={loading} />
      <QueryState
        error={open.error}
        detail={open.detail}
        refusal={open.refusal}
        loading={!numbered || (open.loading && open.data === null)}
        placeholder={
          numbered ? <Skeleton variant="list" rows={4} label="Loading what to change" /> : null
        }
      >
        {open.data ? (
          <>
            {change.length === 0 ? (
              <EmptyState
                size="compact"
                title="Nothing to change"
                description={
                  asleep.length > 0 ? "Only what you snoozed." : "The checks found nothing to fix."
                }
              />
            ) : (
              <AttentionList
                items={items(change)}
                collapseAfter={6}
                labels={{
                  title: "What to change",
                  tones: Object.fromEntries(SEVERITIES.map((s) => [s.tone, s.label])),
                }}
              />
            )}
            {engine.length > 0 ? (
              <AttentionList
                items={items(engine)}
                labels={{
                  title: "Handled by the engine",
                  tones: { danger: "Engine", warning: "Engine", info: "Engine" },
                }}
              />
            ) : null}
            {asleep.length > 0 ? (
              <AttentionList
                items={items(asleep)}
                labels={{
                  title: "Snoozed",
                  tones: Object.fromEntries(SEVERITIES.map((s) => [s.tone, s.label])),
                }}
              />
            ) : null}
            {blocked ? <GoLiveReview /> : null}
          </>
        ) : null}
        {row.dialog}
      </QueryState>
    </>
  );
}

/** How each closed state reads as a pill: only a resolved finding is good news. */
const OUTCOME_TONES: Readonly<Record<AdvisorKeptFinding["state"], Tone>> = {
  open: "info",
  applied: "info",
  resolved: "success",
  retired: "neutral",
  expired: "neutral",
  dismissed: "neutral",
};

/** A time of day, as the history's first column says it: "16:30". */
const clockOf = (at: number) =>
  dateTimeFormat(undefined, zone(), { hour: "2-digit", minute: "2-digit", hour12: false }).format(
    at,
  );

/** Closed findings by the reader's day they closed on, newest first, each day headed. */
export function historyDays(
  rows: readonly AdvisorKeptFinding[],
  now: number,
): { key: string; heading: string; rows: AdvisorKeptFinding[] }[] {
  const today = readerDay(now);
  const yesterday = shiftDay(today, -1);
  const days = new Map<string, AdvisorKeptFinding[]>();
  for (const f of rows) {
    const at = Date.parse(f.closed_at ?? "");
    const key = Number.isNaN(at) ? "" : readerDay(at);
    days.set(key, [...(days.get(key) ?? []), f]);
  }
  return [...days.entries()]
    .sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : b.localeCompare(a)))
    .map(([key, list]) => ({
      key,
      heading:
        key === ""
          ? "Undated"
          : key === today
            ? "Today"
            : key === yesterday
              ? "Yesterday"
              : fmtDate(list[0]!.closed_at),
      rows: [...list].sort((a, b) => (b.closed_at ?? "").localeCompare(a.closed_at ?? "")),
    }));
}

/**
 * What was found and closed, by the day it closed: the time, the title (cut,
 * whole on hover) over what it was about and why it closed, and how it ended.
 */
function History() {
  const [cursor, setCursor] = useState<string | null>(null);
  const [held, setHeld] = useState<AdvisorKeptFinding[]>([]);
  const closed = useFindings("closed", cursor);
  const now = useNow();
  const rows = pagedOn(held, closed.data?.findings, (f) => f.id);
  return (
    <QueryState
      error={closed.error}
      detail={closed.detail}
      refusal={closed.refusal}
      loading={closed.loading && closed.data === null && held.length === 0}
      placeholder={<Skeleton variant="text" rows={8} label="Loading closed findings" />}
    >
      {rows.length === 0 && closed.data ? (
        <EmptyState
          size="compact"
          title="Nothing closed yet"
          description="A finding that clears, is fixed, expires or is dismissed is kept here for 90 days."
        />
      ) : (
        <div className="advisor-closed" aria-label="Closed findings" role="region">
          {historyDays(rows, now).map((day) => (
            <section key={day.key} className="advisor-closed__day" aria-label={day.heading}>
              <Text as="h2" variant="label">
                {day.heading}
                <span className="advisor-closed__count"> · {fmtCount(day.rows.length)}</span>
              </Text>
              <List template="3.5rem minmax(0, 1fr) 8.5rem" density="compact">
                {day.rows.map((f) => {
                  const at = Date.parse(f.closed_at ?? "");
                  const about = [
                    f.subject.name,
                    f.close_reason ? CLOSE_REASONS[f.close_reason] : null,
                  ]
                    .filter(Boolean)
                    .join(" · ");
                  return (
                    <ListItem
                      key={f.id}
                      href={href(["advisor", "findings", f.id])}
                      leading={
                        <time
                          className="advisor-closed__time"
                          dateTime={f.closed_at ?? undefined}
                          title={f.closed_at ? relTime(f.closed_at, now) : undefined}
                        >
                          {Number.isNaN(at) ? "" : clockOf(at)}
                        </time>
                      }
                      trailing={
                        <Tag variant={OUTCOME_TONES[f.state]} size="sm">
                          {STATE_WORDS[f.state]}
                        </Tag>
                      }
                    >
                      <span className="advisor-closed__finding">
                        <Text as="span" variant="cell" truncate>
                          {f.title}
                        </Text>
                        {about ? (
                          <Text as="span" variant="caption" tone="secondary" truncate>
                            {about}
                          </Text>
                        ) : null}
                      </span>
                    </ListItem>
                  );
                })}
              </List>
            </section>
          ))}
        </div>
      )}
      {closed.data?.next_cursor ? (
        <Button
          size="small"
          variant="ghost"
          onClick={() => {
            setHeld(rows);
            setCursor(closed.data!.next_cursor);
          }}
        >
          Show older findings
        </Button>
      ) : null}
      {closed.error && held.length > 0 ? (
        <Callout variant="warning">
          Older findings could not be read. Try again in a moment.
        </Callout>
      ) : null}
    </QueryState>
  );
}
