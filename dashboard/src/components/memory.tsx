/**
 * A seat's diary and its episodes, as two cards — drawn on the seat's Memory
 * tab and on its page under Knowledge › Agent diaries.
 *
 * ONE DRAWING FOR ONE ANSWER. Both screens render the same `agent_memory`
 * answer, and two copies of the cards would disagree about the first thing a
 * reader compares — how many there are, and what "Latest 50 of 142" means —
 * the day one of them was edited.
 *
 * # A header counts the whole, a list shows the newest
 *
 * Every header is the TOTAL the holder counted — never the length of the page
 * it sent, which is fifty at most — and every list cut short says "Latest 50
 * of 142" in its header, the same words on every card, so the two cannot
 * silently disagree.
 */

import { Card, EmptyState, EmptyValue, Tag } from "@crewlethq/ui";
import { BookOpenGlyph, LayersGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, DurationCell, KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { conversationLabel, fmtDateTime, fmtExact, plural, tsKey } from "~/lib/format.ts";
import { decisionLabel, decisionTone } from "~/lib/phases.ts";
import { uiletTone } from "~/ui/primitives.tsx";
import type { AgentMemory } from "~/contract/memory.ts";

/**
 * The header's line for a list the holder cut at its page — "Latest 50 of
 * 142" — or null where the page holds every row, which the header's count
 * already says.
 */
export function pageWords(shown: number, total: number): string | null {
  if (total <= shown) return null;
  return `Latest ${fmtExact(shown)} of ${fmtExact(total)}`;
}

/** A card's subtitle: the cut, where there is one, then what the list is. */
export function cutThen(cut: string | null, what: string): string {
  return cut ? `${cut} — ${what}` : what;
}

/** Who answered, as the sentence under a memory screen's head. */
export function heldWords(heldBy: string | undefined): string {
  if (!heldBy) return "";
  if (heldBy === "none") {
    return "No node holds this seat, so no copy of its memory is current and nothing is shown.";
  }
  return `Read from ${heldBy}, the node holding this seat — the copy kept current.`;
}

/** The heading level a card's title takes on the screen drawing it. */
type Heading = "h2" | "h3";

/** What the seat chose to remember, newest first. */
export function DiaryCard({ memory, heading = "h3" }: { memory: AgentMemory; heading?: Heading }) {
  return (
    <Card padding="none">
      <Card.Header
        icon={<BookOpenGlyph size="sm" />}
        count={memory.diary_total}
        subtitle={cutThen(
          pageWords(memory.diary.length, memory.diary_total),
          "what it chose to remember",
        )}
      >
        <Card.Title as={heading}>Diary</Card.Title>
      </Card.Header>
      {memory.diary.length ? (
        <div className="list">
          {memory.diary.map((d) => (
            <div key={d.id} className="thread-entry">
              <div className="row gap-1">
                <Tag appearance="outline">
                  {d.retention === "diary_short" ? "short-lived" : "kept"}
                </Tag>
                {d.retrievals > 0 && (
                  <span className="t-caption">recalled {plural(d.retrievals, "time")}</span>
                )}
                <span className="spacer" />
                <span className="t-caption">{fmtDateTime(d.created_at)}</span>
              </div>
              <p className="t-body">{d.content}</p>
            </div>
          ))}
        </div>
      ) : (
        <EmptyState
          size="compact"
          icon={<BookOpenGlyph size={32} />}
          title="Nothing written yet"
          description="A seat keeps a note by calling reflect_and_persist during a turn, and the learning loop keeps what it judges worth remembering."
        />
      )}
    </Card>
  );
}

/** One episode, as the grid below draws it. */
type EpisodeRow = AgentMemory["episodes"][number];

/**
 * The episodes grid's columns — a MODULE CONSTANT, because a list built in the
 * render is a new list every render, and the grid takes a new list as new
 * columns: every row measured and drawn again for a card nothing changed.
 * Nothing here reads the clock; the age is read by the cell that shows it.
 */
const EPISODE_COLUMNS: GridColumn<EpisodeRow>[] = [
  {
    key: "at",
    header: "When",
    shrink: true,
    // THROUGH `tsKey`, never `<` on the string: the engine
    // trims trailing zeros, so a raw compare puts `:07Z`
    // before `:07.42Z`.
    sortValue: (e) => tsKey(e.created_at),
    cell: (e) => <DateCell at={e.created_at} />,
  },
  {
    key: "task",
    header: "What it did",
    cell: (e) =>
      e.task_summary ? (
        <TextCell>{e.task_summary}</TextCell>
      ) : (
        <EmptyValue label="The episode recorded no summary" />
      ),
  },
  {
    key: "outcome",
    header: "Outcome",
    shrink: true,
    sortValue: (e) => e.review_outcome || null,
    // THE REVIEWER'S DECISION, in the one table that tones
    // it (`decisionTone`): `failed` is red here as it is on
    // the turn it came from, where a two-way done-or-warning
    // drew it amber.
    cell: (e) =>
      e.review_outcome ? (
        <Tag
          variant={uiletTone(decisionTone("review", e.review_outcome))}
          title={decisionLabel("review", e.review_outcome)}
        >
          {e.review_outcome}
        </Tag>
      ) : (
        <EmptyValue label="The turn ended without a review outcome" />
      ),
  },
  {
    key: "dur",
    header: "Took",
    align: "right",
    shrink: true,
    sortValue: (e) => e.duration_ms ?? null,
    cell: (e) => <DurationCell ms={e.duration_ms} />,
  },
  {
    key: "conv",
    header: "Conversation",
    // CONTENT-SIZED AND CAPPED, with a uuid cut to its head:
    // flexible, a native task's `work:task:<uuid>` took the
    // width and "What it did" — the column a reader scans —
    // was cut at a third of the grid.
    shrink: true,
    cell: (e) =>
      e.conversation_key ? (
        <KeyCell value={e.conversation_key} text={conversationLabel(e.conversation_key)} />
      ) : (
        <EmptyValue label="Not part of a conversation" />
      ),
  },
];

/** An episode's row key: its own id, else the turn it closed, else when. */
const episodeKey = (e: EpisodeRow) => e.id || e.turn_id || e.created_at;

/** One row per completed turn, newest first. */
export function EpisodesCard({
  memory,
  heading = "h3",
}: {
  memory: AgentMemory;
  heading?: Heading;
}) {
  return (
    <Card padding="none">
      <Card.Header
        icon={<LayersGlyph size="sm" />}
        count={memory.episodes_total}
        subtitle={cutThen(
          pageWords(memory.episodes.length, memory.episodes_total),
          "one per completed turn, recalled by similarity at turn start",
        )}
      >
        <Card.Title as={heading}>Episodes</Card.Title>
      </Card.Header>
      <DataGrid
        name="episodes"
        rows={memory.episodes}
        rowKey={episodeKey}
        defaultSort="-at"
        empty={{
          title: "No episodes recorded",
          hint: "An episode is written when a turn completes.",
        }}
        columns={EPISODE_COLUMNS}
      />
    </Card>
  );
}
