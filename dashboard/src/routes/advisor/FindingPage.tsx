/**
 * One finding's page (`#/advisor/findings/{id}`): why it was raised, the
 * model's explanation where one was written for these numbers, the facts with
 * the window they were measured over, the records they cite, the fix in
 * words with its Apply, every transition it went through, and the earlier
 * episodes of the same condition.
 *
 * THE MODEL'S WORDS ARE SAID TO BE ITS OWN. The engine writes a rule
 * finding's title, why and fix; the Advisor's model writes an explanation of
 * one, and the whole of a finding its model pass found (`source: model`).
 * Drawn alike, the model's sentences read as the engine's: the explanation
 * is headed as the model's, and a model finding is marked so.
 *
 * WHERE IT STANDS IS ITS TAG: Open, Snoozed until a time (with End the
 * snooze), Applied, or how it closed. A dismissed one offers Reopen, which
 * lifts the dismissal's hold (`decisions.tsx` says what follows).
 */

import { useCallback, useState } from "react";
import { Button, Callout, Link, Tag, Text, Timeline, TimelineItem } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtAt, fmtDateCompact, fmtDateTime } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import { ApplyPanel } from "./ApplyPanel.tsx";
import { useFinding } from "./api.ts";
import { useAskAbout, useDecisions } from "./decisions.tsx";
import {
  ActionButton,
  AtlasFix,
  FindingMenu,
  RecordLinks,
  freshness,
  marks,
  statWords,
  snoozed,
  type OpenApply,
  type RowActions,
} from "./rows.tsx";
import type { AdvisorFindingDetail } from "~/protocol/advisorFindings.ts";
import { CLOSE_REASONS, STATE_WORDS, severityWord, toneOf } from "./words.ts";
import { findingView, withoutSubject } from "./view.ts";

/** What each transition is called on the timeline; one this build does not know is said by its name. */
const EVENTS: Readonly<Record<string, string>> = {
  opened: "Found",
  revised: "Its numbers or its change moved",
  applied: "A change was applied",
  ineffective: "The change did not clear it",
  snoozed: "Snoozed",
  unsnoozed: "Back from a snooze",
  useful: "Marked useful",
  closed: "Closed",
  explained: "Explained",
  // A DISMISSAL'S HOLD LIFTED: the episode stays closed, and the condition
  // comes back as a new finding if it still holds.
  released: "Dismissal lifted",
};

export function FindingPage({ id }: { id: string }) {
  const read = useFinding(id);
  const finding = read.data;
  usePageLabels(finding ? { [id]: finding.title } : {});
  const { refetch } = read;
  const reload = useCallback(() => refetch(), [refetch]);
  return (
    <QueryState
      error={read.error}
      refusal={read.refusal}
      detail={read.detail}
      loading={read.loading && !finding}
    >
      {finding ? <Finding finding={finding} reload={reload} /> : null}
    </QueryState>
  );
}

function Finding({ finding: f, reload }: { finding: AdvisorFindingDetail; reload: () => void }) {
  const now = useNow();
  const askAbout = useAskAbout();
  const decisions = useDecisions(reload);
  const [applying, setApplying] = useState<OpenApply | null>(null);
  const [reviewing, setReviewing] = useState(false);
  const view = findingView(f);
  const actions: RowActions = {
    apply: (_, option) => setApplying({ id: f.id, option }),
    review: () => setReviewing(true),
    ask: askAbout,
    snooze: decisions.snooze,
    wake: decisions.wake,
    dismiss: decisions.dismiss,
  };
  const asleep = snoozed(f, now);
  // A DISMISSAL ALREADY LIFTED is not offered again: its last move is the lift.
  const lifted = f.history.at(-1)?.event === "released";
  return (
    <article className="col gap-4" aria-label={f.title}>
      <header className="col gap-2">
        <div className="advisor-finding__tags">
          <Tag size="sm" variant={toneOf(f.severity)}>
            {severityWord(f.severity)}
          </Tag>
          <Tag
            size="sm"
            variant={f.state === "open" || f.state === "applied" ? "neutral" : "success"}
          >
            {asleep ? `Snoozed until ${fmtAt(f.snoozed_until, now)}` : STATE_WORDS[f.state]}
          </Tag>
          {f.source === "model" ? (
            <Tag size="sm" variant="neutral">
              Found by the Advisor's model
            </Tag>
          ) : null}
          {f.subject.name ? (
            <Text as="span" variant="caption" tone="secondary">
              {f.subject.name}
            </Text>
          ) : null}
        </div>
        <Text as="h2" variant="heading">
          {marked(f.title)}
        </Text>
        {/* EVERY NUMBER ONCE, on one line under the headline: the facts it
            rests on and what became of it. */}
        <Text as="p" className="advisor-finding__stats">
          {[
            ...f.stats.map((s) => statWords({ ...s, label: withoutSubject(s.label, f) })),
            ...marks(f, now),
          ].join(" · ")}
        </Text>
        {f.close_reason ? (
          <Callout variant="neutral">
            {CLOSE_REASONS[f.close_reason]}
            {f.closed_at ? `, ${fmtDateTime(f.closed_at)}.` : "."}
            {f.state === "dismissed" && lifted
              ? " Its dismissal was lifted: it comes back on the list at the Advisor's next check, if it still holds."
              : ""}
          </Callout>
        ) : null}
      </header>

      <section className="col gap-1" aria-label="Why">
        <Text as="p">{marked(f.why)}</Text>
      </section>

      {f.explanation ? (
        <section className="col gap-1" aria-labelledby="advisor-explanation">
          <Text as="h3" variant="label" id="advisor-explanation">
            The Advisor's model explains
          </Text>
          <Text as="p">{marked(f.explanation)}</Text>
        </section>
      ) : null}

      {/* WHEN AND OVER WHAT: the window it was measured over, when it was
          found and checked, and the records it cites, in one quiet line. */}
      <Text as="p" variant="caption" tone="secondary">
        Measured {fmtDateCompact(f.evidence.window.from, now)} to{" "}
        {fmtDateCompact(f.evidence.window.to, now)} · {freshness(f, now)}
      </Text>
      <RecordLinks records={f.evidence.records} />

      <section className="col gap-2" aria-labelledby="advisor-fix">
        <Text as="h3" variant="label" id="advisor-fix">
          The fix
        </Text>
        {/* THE FIX'S WORDS AND ITS PRESSES ON ONE LINE: a fix with no press
            of its own left its menu alone on a line under the sentence. */}
        <div className="advisor-fix-line">
          <Text as="p">{marked(f.fix.sentence)}</Text>
          <span className="advisor-row-actions">
            {view.actions.primary ? (
              <ActionButton f={f} action={view.actions.primary} actions={actions} />
            ) : null}
            {asleep ? (
              <Button size="small" variant="secondary" onClick={() => decisions.wake(f)}>
                End the snooze
              </Button>
            ) : null}
            <FindingMenu f={f} actions={actions} now={now} />
            {f.state === "dismissed" && !lifted ? (
              <Button size="small" variant="secondary" onClick={() => decisions.reopen(f)}>
                Reopen
              </Button>
            ) : null}
          </span>
        </div>
        {/* APPLY CONFIRMS IN PLACE, under the fix it was pressed on. */}
        {applying ? (
          <section className="advisor-apply-place" aria-label="Apply">
            <ApplyPanel
              key={applying.option}
              finding={f}
              option={applying.option}
              onClose={() => setApplying(null)}
              onSettled={reload}
            />
          </section>
        ) : null}
      </section>

      {f.history.length > 0 ? (
        <section className="col gap-2" aria-labelledby="advisor-history">
          <Text as="h3" variant="label" id="advisor-history">
            What happened to it
          </Text>
          <Timeline density="compact">
            {f.history.map((h, i) => (
              <TimelineItem key={`${h.at}-${i}`} label={false}>
                <Text as="p">
                  {EVENTS[h.event] ?? h.event}
                  {h.by.kind === "person" && h.by.handle ? ` by ${h.by.handle}` : ""}
                </Text>
                <Text as="p" variant="caption" tone="secondary">
                  {fmtDateTime(h.at)}
                  {h.moved.length > 0
                    ? ` · ${h.moved.map((m) => `${m.label}: ${m.was ?? "none"} to ${m.now ?? "none"}`).join(", ")}`
                    : ""}
                </Text>
              </TimelineItem>
            ))}
          </Timeline>
        </section>
      ) : null}

      {f.episodes.length > 0 ? (
        <section className="col gap-1" aria-labelledby="advisor-episodes">
          <Text as="h3" variant="label" id="advisor-episodes">
            Seen before
          </Text>
          <ul className="advisor-history">
            {f.episodes.map((e) => (
              <li key={e.id} className="advisor-history__row">
                <Link href={href(["advisor", "findings", e.id])}>
                  {fmtDateTime(e.opened_at)} to {fmtDateTime(e.closed_at)}
                </Link>
                <Text as="span" variant="caption" tone="secondary">
                  {CLOSE_REASONS[e.close_reason]}
                </Text>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <AtlasFix
        finding={reviewing ? f : null}
        onClose={() => {
          setReviewing(false);
          reload();
        }}
      />
    </article>
  );
}
