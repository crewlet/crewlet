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

import { useCallback, useState } from "react";
import { AttentionList, Button, Callout, EmptyState, Link, Text } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
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
      <Charts snapshot={snapshot} findings={all} now={now} />
      <QueryState
        error={open.error}
        detail={open.detail}
        refusal={open.refusal}
        loading={open.loading && open.data === null}
      >
        {open.data ? (
          <>
            {change.length === 0 ? (
              <EmptyState
                size="compact"
                title="Nothing to change"
                description={
                  asleep.length > 0
                    ? "Nothing to change right now beyond what you snoozed."
                    : "The Advisor's checks found nothing worth changing right now."
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
                  title: "What the engine is handling",
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
    >
      {rows.length === 0 && closed.data ? (
        <EmptyState
          size="compact"
          title="Nothing closed yet"
          description="A finding that clears, is fixed, expires or is dismissed is kept here for 90 days."
        />
      ) : (
        <ul className="advisor-history" aria-label="Closed findings">
          {rows.map((f) => (
            <li key={f.id} className="advisor-history__row">
              <Link href={href(["advisor", "findings", f.id])}>{f.title}</Link>
              <Text as="span" variant="caption" tone="secondary">
                {[
                  f.subject.name,
                  STATE_WORDS[f.state],
                  f.close_reason ? CLOSE_REASONS[f.close_reason] : null,
                  f.closed_at ? relTime(f.closed_at, now) : null,
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </Text>
            </li>
          ))}
        </ul>
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
