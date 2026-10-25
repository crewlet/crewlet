/**
 * The assistant's home: a greeting and the one sentence that says what it
 * may do, what is waiting on the reader, and questions it answers anywhere.
 *
 * NEEDS YOU LEADS, because in Crewlet the costliest thing a person can miss is
 * a decision or a parked coding run: an agent seat is stopped until they
 * answer. Its rows are real items read with the reader's own session (the
 * decisions the Inbox counts, and the company's budget), each drawn only when
 * there is something in it, and the card only when one row is.
 *
 * EVERY ROW LEADS SOMEWHERE THE READER MAY GO: a question is offered only
 * where the reader holds what answering it reads, and a change only where
 * the engine would make it for them (`viewer.acts`), so a row never leads to
 * a refusal. A question row asks itself; a change row ends in an ellipsis
 * and fills the composer with its start, because a change needs the
 * person's own words, and nothing is sent until they press Send.
 */

import { type ReactNode } from "react";
import { Card, Eyebrow, Stack, SuggestionList, SuggestionRow, Tag, Text } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  BotGlyph,
  CoinsGlyph,
  GaugeGlyph,
  MessageCircleQuestionMarkGlyph,
  PackageGlyph,
  PauseGlyph,
  SquareTerminalGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useAgents, useOrg, useOrgBudget } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { decisionSubjects, seatConditionsOf } from "~/lib/decisions.ts";
import { indexOrg, nameOfIn } from "~/lib/seats.ts";
import { PERIOD_ADJECTIVE } from "~/lib/budget.ts";
import { fmtCount, fmtDate, greeting, plural } from "~/lib/format.ts";

/** The share of a ceiling from which the budget row is drawn. */
const BUDGET_FROM = 0.75;

/** One row of Anywhere in Crewlet: what it asks, and what answering it needs. */
interface Anywhere {
  key: string;
  icon: ReactNode;
  words: string;
  /** Whether it fills the composer instead of asking: a change, in the person's words. */
  fills?: string;
  /** Whether the reader may ask it. */
  offered: (v: { tracker: boolean; knowledge: boolean; acts: readonly string[] }) => boolean;
}

const ANYWHERE: readonly Anywhere[] = [
  {
    key: "spend",
    icon: <CoinsGlyph size="sm" />,
    words: "Which agents spent the most tokens this week?",
    offered: () => true,
  },
  {
    key: "agents",
    icon: <BotGlyph size="sm" />,
    words: "What are my agents doing now?",
    offered: () => true,
  },
  {
    key: "failed",
    icon: <TriangleAlertGlyph size="sm" />,
    words: "Which agents failed a turn today?",
    offered: () => true,
  },
  {
    key: "shipped",
    icon: <PackageGlyph size="sm" />,
    words: "What shipped this week?",
    offered: (v) => v.tracker,
  },
  {
    key: "runbook",
    icon: <BookOpenGlyph size="sm" />,
    words: "Where is the on-call runbook?",
    offered: (v) => v.knowledge,
  },
  {
    key: "pause",
    icon: <PauseGlyph size="sm" />,
    words: "Pause an agent…",
    fills: "Pause Agent ",
    offered: (v) => v.acts.includes("pause_seat"),
  },
];

export interface HomeProps {
  /** The reader's own name, for the greeting. */
  name: string;
  /** Ask a question as written. */
  onAsk: (text: string) => void;
  /** Put a change's start in the composer for the reader to finish. */
  onFill: (text: string) => void;
  now: number;
}

export function Home({ name, onAsk, onFill, now }: HomeProps) {
  const viewer = useViewer();
  const decisions = useQuery("decisions", undefined, {
    enabled: viewer.owner !== "",
    pollMs: 60_000,
  });
  const agents = useAgents();
  const org = useOrg();
  const budget = useOrgBudget();
  const nameOf = nameOfIn(indexOrg(org));

  const subjects = decisionSubjects(decisions.data ?? null, seatConditionsOf(agents));
  const asks = subjects.flatMap((s) => (s.kind === "ask" ? [s.ask] : []));
  const runs = subjects.flatMap((s) => (s.kind === "run" ? [s.run] : []));
  const window = (budget?.org.windows ?? [])
    .filter((w) => w.limit !== undefined && w.limit > 0)
    .sort((a, b) => b.used / b.limit! - a.used / a.limit!)[0];
  const share = window ? window.used / window.limit! : 0;

  const rows: ReactNode[] = [];
  if (asks.length > 0) {
    rows.push(
      <SuggestionRow
        key="asks"
        icon={<MessageCircleQuestionMarkGlyph size="sm" />}
        detail={asks
          .slice(0, 2)
          .map((a) => `${nameOf(a.asked_by)} on ${a.key}`)
          .join(", ")}
        count={
          <Tag size="sm" variant="warning">
            {String(asks.length)}
          </Tag>
        }
        chevron
        onSelect={() => onAsk("What needs my decision?")}
      >
        {plural(asks.length, "A decision is waiting on you", "Decisions waiting on you")}
      </SuggestionRow>,
    );
  }
  if (runs.length > 0) {
    rows.push(
      <SuggestionRow
        key="runs"
        icon={<SquareTerminalGlyph size="sm" />}
        detail={runs
          .slice(0, 2)
          .map((r) => nameOf(r.agent_handle))
          .join(", ")}
        count={
          <Tag size="sm" variant="warning">
            {String(runs.length)}
          </Tag>
        }
        chevron
        onSelect={() => onAsk("Which coding runs are parked on a question?")}
      >
        {plural(
          runs.length,
          "A coding run is parked on a question",
          "Coding runs are parked on a question",
        )}
      </SuggestionRow>,
    );
  }
  if (window && share >= BUDGET_FROM) {
    const percent = Math.round(share * 100);
    const adjective = PERIOD_ADJECTIVE[window.period];
    rows.push(
      <SuggestionRow
        key="budget"
        icon={<GaugeGlyph size="sm" />}
        detail={`${fmtCount(window.used)} of ${fmtCount(window.limit!)} tokens, resets ${fmtDate(window.resets_at)}`}
        count={<Tag size="sm">{`${percent}%`}</Tag>}
        chevron
        onSelect={() => onAsk("Which budget runs out first?")}
      >
        {`${adjective.charAt(0).toUpperCase()}${adjective.slice(1)} budget is at ${percent}%`}
      </SuggestionRow>,
    );
  }

  const offered = ANYWHERE.filter((row) =>
    row.offered({ tracker: viewer.tracker, knowledge: viewer.knowledge, acts: viewer.acts }),
  );

  return (
    <Stack gap={5}>
      <Stack gap={1}>
        <Text as="h3" variant="heading">
          {greeting(now, name)}
        </Text>
        <Text tone="secondary">
          I read only what you can open, and nothing changes until you approve it.
        </Text>
      </Stack>
      {rows.length > 0 ? (
        <Stack as="section" gap={2} aria-labelledby="assistant-needs-you">
          <Eyebrow id="assistant-needs-you">Needs you</Eyebrow>
          <Card>
            <SuggestionList divided>{rows}</SuggestionList>
          </Card>
        </Stack>
      ) : null}
      <Stack as="section" gap={2} aria-labelledby="assistant-anywhere">
        <Eyebrow id="assistant-anywhere">Anywhere in Crewlet</Eyebrow>
        <SuggestionList>
          {offered.map((row) => (
            <SuggestionRow
              key={row.key}
              icon={row.icon}
              fills={row.fills !== undefined}
              onSelect={() => (row.fills !== undefined ? onFill(row.fills) : onAsk(row.words))}
            >
              {row.words}
            </SuggestionRow>
          ))}
        </SuggestionList>
      </Stack>
    </Stack>
  );
}
