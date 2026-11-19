/**
 * One finding's page (`#/advisor/findings/{id}`): why it was raised, the
 * model's explanation where one was written for these numbers, the facts with
 * the window they were measured over, the records they cite, the fix in
 * words with its Apply, every transition it went through, and the earlier
 * episodes of the same condition.
 */

import { useCallback, useState } from "react";
import {
  Button,
  Callout,
  DescriptionList,
  Link,
  Tag,
  Text,
  Timeline,
  TimelineItem,
  useToast,
  type DescriptionListEntry,
} from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { useAssistantControl } from "~/app/assistant/control.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import { ApplyDialog } from "./ApplyDialog.tsx";
import { decide, useFinding } from "./api.ts";
import {
  FindingMenu,
  RecordLinks,
  appliable,
  fixAction,
  freshness,
  type RowActions,
} from "./rows.tsx";
import type { AdvisorFindingDetail, AdvisorHistoryEntry } from "./types.ts";
import { CLOSE_REASONS, STATE_WORDS, factWords, severityWord, toneOf } from "./words.ts";

/** What each transition is called on the timeline. */
const EVENTS: Readonly<Record<AdvisorHistoryEntry["event"], string>> = {
  opened: "Found",
  revised: "Its numbers or its change moved",
  confirmed: "Seen again",
  applied: "A change was applied",
  ineffective: "The change did not clear it",
  snoozed: "Snoozed",
  dismissed: "Dismissed",
  reopened: "Reopened",
  closed: "Closed",
  explained: "Explained",
};

export function FindingPage({ id }: { id: string }) {
  const read = useFinding(id);
  const finding = read.data;
  usePageLabels(finding ? { [id]: finding.title } : {});
  const reload = useCallback(() => void read.reload({ quiet: true }), [read]);
  return (
    <QueryState error={read.code} refusal={read.refusal} loading={read.loading && !finding}>
      {finding ? <Finding finding={finding} reload={reload} /> : null}
    </QueryState>
  );
}

function Finding({ finding: f, reload }: { finding: AdvisorFindingDetail; reload: () => void }) {
  const now = useNow();
  const toast = useToast();
  const assistant = useAssistantControl();
  const [applying, setApplying] = useState(false);
  const answer = (press: () => Promise<unknown>, done: string) =>
    void press().then(
      () => {
        toast.show({ variant: "success", title: done, message: f.title });
        reload();
      },
      () =>
        toast.show({
          variant: "danger",
          title: "Nothing was changed",
          message: "The engine did not take that. Try again in a moment.",
        }),
    );
  const actions: RowActions = {
    apply: () => setApplying(true),
    ask: assistant.unavailable ? null : (question) => assistant.ask(question),
    snooze: () => answer(() => decide(f.id, "snooze"), "Snoozed for 7 days"),
    dismiss: (_, reason) => answer(() => decide(f.id, "dismiss", reason), "Dismissed"),
  };
  const facts: DescriptionListEntry[] = f.evidence.facts.map((fact) => [
    fact.label,
    factWords(fact),
  ]);
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
            {STATE_WORDS[f.state]}
          </Tag>
          {f.subject.name ? (
            <Text as="span" variant="caption" tone="secondary">
              {f.subject.name}
            </Text>
          ) : null}
        </div>
        <Text as="h2" variant="heading">
          {marked(f.title)}
        </Text>
        <Text as="p" variant="caption" tone="secondary">
          {freshness(f, now).join(" · ")}
        </Text>
        {f.close_reason ? (
          <Callout variant="neutral">
            {CLOSE_REASONS[f.close_reason]}
            {f.closed_at ? `, ${fmtDateTime(f.closed_at)}.` : "."}
          </Callout>
        ) : null}
      </header>

      <section className="col gap-1" aria-label="Why">
        <Text as="p">{marked(f.why)}</Text>
        {f.explanation ? <Text as="p">{marked(f.explanation)}</Text> : null}
      </section>

      {facts.length > 0 ? (
        <section className="col gap-1" aria-labelledby="advisor-facts">
          <Text as="h3" variant="label" id="advisor-facts">
            What it rests on, {fmtDateTime(f.evidence.window.from)} to{" "}
            {fmtDateTime(f.evidence.window.to)}
          </Text>
          <DescriptionList items={facts} />
          <RecordLinks records={f.evidence.records} />
        </section>
      ) : (
        <RecordLinks records={f.evidence.records} />
      )}

      <section className="col gap-2" aria-labelledby="advisor-fix">
        <Text as="h3" variant="label" id="advisor-fix">
          The fix
        </Text>
        <Text as="p">{marked(f.fix.sentence)}</Text>
        <div className="advisor-row-actions">
          {appliable(f) ? (
            <Button variant="primary" onClick={() => setApplying(true)}>
              Apply
            </Button>
          ) : (
            fixAction(f, actions)
          )}
          <FindingMenu f={f} actions={actions} />
          {f.state === "dismissed" ? (
            <Button
              size="small"
              variant="secondary"
              onClick={() => answer(() => decide(f.id, "reopen"), "Reopened")}
            >
              Reopen
            </Button>
          ) : null}
        </div>
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
                  {EVENTS[h.event]}
                  {h.by.kind === "person" ? ` by ${h.by.name}` : ""}
                </Text>
                <Text as="p" variant="caption" tone="secondary">
                  {fmtDateTime(h.at)}
                  {h.moved.length > 0
                    ? ` · ${h.moved.map((m) => `${m.label}: ${m.was} to ${m.now}`).join(", ")}`
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

      {applying ? (
        <ApplyDialog finding={f} onClose={() => setApplying(false)} onSettled={reload} />
      ) : null}
    </article>
  );
}
