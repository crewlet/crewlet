/**
 * The assistant's home: a greeting and the one sentence that says what it
 * may do, a brief of the page the reader is on, and questions it answers
 * anywhere.
 *
 * THIS PAGE LEADS: one row, "Brief me on this page", or on a page with one
 * record open, "Brief me on this decision" (the record's own word, from the
 * page context's selection, `../brief.ts`). It starts a conversation whose
 * first answer is the brief: what the page is about, and whether anything
 * needs the reader, ranked with a press for each, which the reader goes on
 * asking about.
 * What waits on the reader is the brief's to say, read for the page they are
 * on, so the home keeps no list of its own beside it.
 *
 * EVERY ROW LEADS SOMEWHERE THE READER MAY GO: a question is offered only
 * where the reader holds what answering it reads, and a change only where
 * the engine would make it for them (`viewer.acts`), so a row never leads to
 * a refusal. A question row asks itself; a change row ends in an ellipsis
 * and fills the composer with its start, because a change needs the
 * person's own words, and nothing is sent until they press Send.
 */

import { type ReactNode } from "react";
import { Eyebrow, Stack, SuggestionList, SuggestionRow, Text } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  BotGlyph,
  CoinsGlyph,
  PackageGlyph,
  PauseGlyph,
  ScrollTextGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { useViewer } from "~/lib/viewer.ts";
import { greeting } from "~/lib/format.ts";
import type { PageSnapshot } from "../context.ts";
import { briefQuestion, briefSubject } from "../brief.ts";

/** One row of Anywhere in Crewlet: what it asks, and what answering it needs. */
interface Anywhere {
  key: string;
  icon: ReactNode;
  words: string;
  /** Whether it fills the composer instead of asking: a change, in the person's words. */
  fills?: string;
  /** Whether the reader may ask it. */
  offered: (v: { worklet: boolean; doclet: boolean; acts: readonly string[] }) => boolean;
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
    offered: (v) => v.worklet,
  },
  {
    key: "runbook",
    icon: <BookOpenGlyph size="sm" />,
    words: "Where is the on-call runbook?",
    offered: (v) => v.doclet,
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
  /** The page the reader is on, or null where there is none to brief. */
  page: PageSnapshot | null;
  /** Ask for a brief of the page, or of its open record. */
  onBrief: () => void;
  /** Ask a question as written. */
  onAsk: (text: string) => void;
  /** Put a change's start in the composer for the reader to finish. */
  onFill: (text: string) => void;
  now: number;
}

export function Home({ name, page, onBrief, onAsk, onFill, now }: HomeProps) {
  const viewer = useViewer();
  const offered = ANYWHERE.filter((row) =>
    row.offered({ worklet: viewer.worklet, doclet: viewer.doclet, acts: viewer.acts }),
  );
  const subject = page ? briefSubject(page.context) : null;

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
      {page ? (
        <Stack as="section" gap={2} aria-labelledby="assistant-this-page">
          <Eyebrow id="assistant-this-page">This page</Eyebrow>
          <SuggestionList divided>
            <SuggestionRow
              key="brief"
              icon={<ScrollTextGlyph size="sm" />}
              detail={
                subject === "page"
                  ? `What ${page.title} is about, and whether anything needs you`
                  : `What this ${subject} is about, and whether it needs you`
              }
              chevron
              onSelect={onBrief}
            >
              {briefQuestion(page.context)}
            </SuggestionRow>
          </SuggestionList>
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
