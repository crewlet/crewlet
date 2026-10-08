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

import { useState, type CSSProperties } from "react";
import { Button, Card, EmptyState, EmptyValue, Modal, Tag } from "@crewlethq/ui";
import { BookOpenGlyph, LayersGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, DurationCell, KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  conversationLabel,
  fmtBytes,
  fmtDateTime,
  plural,
  tsKey,
  utf8Bytes,
} from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
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
  return `Latest ${shown.toLocaleString()} of ${total.toLocaleString()}`;
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

/** One episode as the `agent_memory` answer sends it. */
type EpisodeRow = AgentMemory["episodes"][number];

/** A cell's line held to one line, with its whole text on its title. */
const ONE_LINE = { "--clamp-lines": 1 } as CSSProperties;

function OneLine({ text, caption = false }: { text: string; caption?: boolean }) {
  return (
    <span className={caption ? "t-caption clamp" : "clamp"} style={ONE_LINE} title={text}>
      {text}
    </span>
  );
}

/**
 * Whether a listed text is its OPENING rather than all of it.
 *
 * The listing carries an episode's ask and its account as their openings — a
 * page of them whole could be more than the engine's transport takes — with the
 * size of each whole text beside it, so the text is all of it exactly when that
 * size is its own length.
 */
function isOpening(text: string, wholeBytes: number): boolean {
  return wholeBytes > utf8Bytes(text);
}

/**
 * The way from a listed opening to the whole text: a button naming how much
 * there is, which opens the turn read whole (`agent_episode`).
 */
function ReadWhole({ seat, episode, bytes }: { seat: string; episode: EpisodeRow; bytes: number }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button size="small" variant="ghost" onClick={() => setOpen(true)}>
        Read all {fmtBytes(bytes)}
      </Button>
      {open && <WholeEpisode seat={seat} episode={episode} onClose={() => setOpen(false)} />}
    </>
  );
}

/**
 * One turn read whole: what it was asked and what it did, complete, from the
 * node holding the seat. An episode the seat no longer holds — the lifecycle
 * dropped it or folded it since the list was read — is said so, which is an
 * ordinary absence rather than a failure.
 */
function WholeEpisode({
  seat,
  episode,
  onClose,
}: {
  seat: string;
  episode: EpisodeRow;
  onClose: () => void;
}) {
  const whole = useQuery("agent_episode", { id: seat, episode: episode.id });
  const e = whole.data?.episode;
  return (
    <Modal
      open
      size="lg"
      title={episode.task_summary || "A past turn"}
      icon={<LayersGlyph />}
      onClose={onClose}
      footer={
        <Button variant="secondary" onClick={onClose}>
          Close
        </Button>
      }
    >
      <QueryState error={whole.error} detail={whole.detail} loading={whole.loading}>
        {whole.data &&
          (e ? (
            <div className="col gap-3">
              <section className="col gap-1" aria-label="What it was asked">
                <span className="t-caption">Asked</span>
                {e.ask ? <p className="prose">{e.ask}</p> : <EmptyValue label="Not recorded" />}
              </section>
              <section className="col gap-1" aria-label="What it did">
                <span className="t-caption">What it did</span>
                {e.plan_summary ? (
                  <p className="prose">{e.plan_summary}</p>
                ) : (
                  <EmptyValue label="The turn recorded nothing it did" />
                )}
              </section>
            </div>
          ) : (
            <EmptyState
              title="No longer in this seat's memory"
              description="The episode was dropped or folded into a compacted row since the list was read."
            />
          ))}
      </QueryState>
    </Modal>
  );
}

/**
 * What woke a turn, and what it was asked — or, for a compacted row, how many
 * turns it stands for.
 *
 * THE LABEL IS SAID AS WHAT WOKE THE TURN, never as what it did: it names the
 * kind of event ("Message from Ana: Slack message"), and under "What it did"
 * it told an operator that every chat turn had done "Message from Ana". The
 * ask under it is what the turn was actually asked, where the row stored one —
 * its opening, marked, with the way to the whole of it where it is longer.
 */
function EpisodeWokenCell({ seat, episode: e }: { seat: string; episode: EpisodeRow }) {
  if (e.compacted) {
    return <TextCell>{plural(Math.max(e.count, 1), "turn")} like this</TextCell>;
  }
  if (!e.task_summary && !e.ask) return <EmptyValue label="Not recorded" />;
  const opening = isOpening(e.ask, e.ask_bytes);
  return (
    <span className="col" style={{ gap: 2 }}>
      {e.task_summary ? <OneLine text={e.task_summary} /> : <EmptyValue label="Not recorded" />}
      {e.ask && <OneLine text={`Asked: ${e.ask}${opening ? "…" : ""}`} caption />}
      {opening && <ReadWhole seat={seat} episode={e} bytes={e.ask_bytes} />}
    </span>
  );
}

/**
 * What a turn did — its account — or, for a compacted row, what its turns had
 * in common and what varied: a compacted row has no account of its own, and
 * drawn as a turn it read "The episode recorded no summary" in place of the
 * pattern it was folded into.
 */
function EpisodeDidCell({ seat, episode: e }: { seat: string; episode: EpisodeRow }) {
  if (e.compacted) {
    const c = e.compaction;
    if (!c?.common_task_pattern) return <EmptyValue label="The compaction recorded no pattern" />;
    return (
      <span className="col" style={{ gap: 2 }}>
        <OneLine text={c.common_task_pattern} />
        {c.notable_patterns && <OneLine text={`What varied: ${c.notable_patterns}`} caption />}
      </span>
    );
  }
  if (!e.plan_summary) return <EmptyValue label="The turn recorded nothing it did" />;
  const opening = isOpening(e.plan_summary, e.plan_summary_bytes);
  if (!opening) return <OneLine text={e.plan_summary} />;
  return (
    <span className="col" style={{ gap: 2 }}>
      <OneLine text={`${e.plan_summary}…`} />
      <ReadWhole seat={seat} episode={e} bytes={e.plan_summary_bytes} />
    </span>
  );
}

/** One row per completed turn, newest first. */
export function EpisodesCard({
  memory,
  now,
  heading = "h3",
}: {
  memory: AgentMemory;
  now: number;
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
        rowKey={(e) => e.id || e.turn_id || e.created_at}
        defaultSort="-at"
        empty={{
          title: "No episodes recorded",
          hint: "An episode is written when a turn completes.",
        }}
        columns={[
          {
            key: "at",
            header: "When",
            shrink: true,
            // THROUGH `tsKey`, never `<` on the string: the engine
            // trims trailing zeros, so a raw compare puts `:07Z`
            // before `:07.42Z`.
            sortValue: (e) => tsKey(e.created_at),
            cell: (e) => <DateCell at={e.created_at} now={now} />,
          },
          {
            key: "woke",
            header: "Woken by",
            cell: (e) => <EpisodeWokenCell seat={memory.id} episode={e} />,
          },
          {
            key: "did",
            header: "What it did",
            cell: (e) => <EpisodeDidCell seat={memory.id} episode={e} />,
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
                <span className="col" style={{ gap: 2 }}>
                  <Tag
                    variant={uiletTone(decisionTone("review", e.review_outcome))}
                    title={decisionLabel("review", e.review_outcome)}
                  >
                    {e.review_outcome}
                  </Tag>
                  {e.compacted && e.compaction && (
                    <span className="t-caption nowrap">
                      {e.compaction.done.toLocaleString()} of{" "}
                      {Math.max(e.count, 1).toLocaleString()} done
                    </span>
                  )}
                </span>
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
        ]}
      />
    </Card>
  );
}
