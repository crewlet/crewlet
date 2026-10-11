/**
 * A knowledge page as the agents read it (`#/admin/agents/pages/{id}`): who
 * read it, HOW — the turn each read was in, the words each search used — and,
 * for a tool skill, who it reached as one.
 *
 * # Why the console has its own view of a page
 *
 * The page itself is a person's (`#/knowledge/pages/{id}`) and says who read
 * it and how lately (`page.readers`), which every member is served. How each
 * agent read it is `page_reads`: the turn ids and the agents' own search
 * strings, what the machine processed, which only an admin is served
 * (ADR-0031). It was a section of the person's page, and that put an admin's
 * read in a screen every member opens — a refused read on every page a member
 * looked at, and a person shell that asked something it is not served
 * (`app/shell.test.ts`). So the reads moved here, and this is the page's
 * console twin: `g .` and an admin's ⋯ menu on the page come here, and the
 * band's "Open in …" goes back.
 *
 * # A page has no list here
 *
 * Like a trace, it is opened from somewhere that already holds its id — the
 * page, or a tool skill's row in Agent skills — so `#/admin/agents/pages` alone
 * is Not Found rather than a second index of the knowledge base.
 */

import { useMemo } from "react";
import { Card, EmptyState } from "@crewlethq/ui";
import { FileTextGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { usePageLabels } from "~/app/page.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { readCount, readToday, readVia } from "~/lib/pageReads.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { PageReadsAnswer, SkillLoad } from "~/contract/pages.ts";

/** Who read a page moves on the scale of minutes. */
const READS_POLL_MS = 60_000;

type Who = (handle: string) => { name: string; kind: "agent" | "human" };

export function PageReads({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const who: Who = useMemo(() => {
    const look = seatLookup(index);
    return (handle: string) => {
      const seat = look(handle);
      return { name: seat.name, kind: seat.kind === "human" ? "human" : "agent" };
    };
  }, [index]);
  const now = useNow();
  const page = useQuery("page", { id }, { enabled: id !== "" });
  const reads = useQuery("page_reads", { page: id }, { enabled: id !== "", pollMs: READS_POLL_MS });
  const detail = page.data;
  usePageLabels(detail ? { [id]: detail.page.title } : {});

  if (page.error === "not_found") {
    return (
      <EmptyState
        icon={<FileTextGlyph size="xl" />}
        title="There is no such page"
        description="A page's address carries its id, which a rename does not change. It may have been trashed or purged — or this node's copy of the knowledge base has not caught up with it yet."
      />
    );
  }

  return (
    <>
      <PageNote>
        How the agents read this page: the turn each read was in and the words each search used. Who
        read it and how lately is on the page itself, for everybody.
      </PageNote>
      <QueryState error={page.error} loading={page.loading && !detail}>
        {detail && (
          <div className="col gap-4">
            <Reads
              reads={reads.data}
              error={reads.error}
              who={who}
              now={now}
              zone={org?.timezone}
            />
            {detail.skill && <SkillLoads loads={detail.skill_loaded_by} who={who} now={now} />}
          </div>
        )}
      </QueryState>
    </>
  );
}

/**
 * Every agent's reads of the page, one row per agent and way of reading,
 * newest first — and the ENGINE's count of who read it on the company's today.
 */
function Reads({
  reads,
  error,
  who,
  now,
  zone,
}: {
  reads: PageReadsAnswer | null;
  error: string | null;
  who: Who;
  now: number;
  zone: string | undefined;
}) {
  const readers = reads?.readers ?? [];
  const faces = reads ? readToday(readers, now, zone) : [];
  return (
    <Card>
      <Card.Header
        actions={
          reads && reads.distinct_seats_today > 0 ? (
            <span className="t-caption" title={faces.map((h) => who(h).name).join(", ")}>
              {`read by ${plural(reads.distinct_seats_today, "agent")} today`}
            </span>
          ) : undefined
        }
      >
        <Card.Title as="h2">
          {reads ? `Read by agents · last ${plural(reads.days, "day")}` : "Read by agents"}
        </Card.Title>
      </Card.Header>
      <div className="col gap-3">
        {error === "unknown_query" ? (
          // NOT SERVED HERE — a node with no usage domain — is not "nobody
          // read it", and the two send an admin in opposite directions.
          <p className="kpage-rail-note">This node keeps no record of who read what.</p>
        ) : error ? (
          <p className="kpage-rail-note">Who read it could not be read ({error}).</p>
        ) : !reads ? (
          <p className="kpage-rail-note">Reading…</p>
        ) : readers.length === 0 ? (
          <p className="kpage-rail-note">
            No agent has read it in the last {plural(reads.days, "day")}.
          </p>
        ) : (
          <ul className="kpage-readers" role="list">
            {readers.map((r) => {
              const seat = who(r.handle);
              const via = readVia(r);
              const count = readCount(r);
              return (
                <li key={`${r.handle}/${r.via}`} className="kpage-reader">
                  <SeatAvatar name={seat.name} kind={seat.kind} size="sm" decorative />
                  <span className="kpage-reader-text">
                    <span className="kpage-reader-head">
                      <a
                        className="kpage-reader-name"
                        href={href(["admin", "agents", "seats", r.handle])}
                      >
                        {seat.name}
                      </a>
                      {count && <span className="kpage-reader-count">{count}</span>}
                    </span>
                    <span className="kpage-reader-via">
                      {r.last_turn_id ? (
                        <a
                          className="kpage-reader-how kpage-reader-via-link"
                          href={href(["admin", "live", "turns", r.last_turn_id])}
                          title={via}
                        >
                          {via}
                        </a>
                      ) : (
                        <span className="kpage-reader-how" title={via}>
                          {via}
                        </span>
                      )}
                      <span className="kpage-reader-when">
                        {" · "}
                        <time dateTime={r.last_at} title={fmtDateTime(r.last_at)}>
                          {relTime(r.last_at, now)}
                        </time>
                      </span>
                    </span>
                  </span>
                </li>
              );
            })}
          </ul>
        )}
        {reads && reads.readers_total > readers.length && (
          <p className="kpage-rail-note">
            The {readers.length} most recent of {reads.readers_total}.
          </p>
        )}
        {reads && reads.elided > 0 && (
          // THE CAP IS STATED, never silent: a seat's day keeps a bounded
          // number of (page, way) entries, and a reader whose entry was dropped
          // is not on this list. COMPANY-WIDE, and worded so: the engine counts
          // what the cap dropped across every page, not this one's, so the
          // note says the list MAY be short rather than that this page lost a
          // read.
          <p className="kpage-rail-note">
            Some agents&rsquo; days were too busy to record every read, so this list may be missing
            some.
          </p>
        )}
      </div>
    </Card>
  );
}

/**
 * A tool skill's reach: each agent it was loaded by or offered to, over the
 * thirty company days the engine counts, most recent first.
 */
function SkillLoads({
  loads,
  who,
  now,
}: {
  loads: SkillLoad[] | undefined;
  who: Who;
  now: number;
}) {
  return (
    <Card>
      <Card.Header>
        <Card.Title as="h2">Reached as a skill · last 30 days</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        {!loads || loads.length === 0 ? (
          <p className="kpage-rail-note">No agent has loaded it or been offered it.</p>
        ) : (
          <ul className="kpage-readers" role="list">
            {loads.map((l) => {
              const seat = who(l.handle);
              return (
                <li key={l.handle} className="kpage-reader">
                  <SeatAvatar name={seat.name} kind={seat.kind} size="sm" decorative />
                  <span className="kpage-reader-text">
                    <span className="kpage-reader-head">
                      <a
                        className="kpage-reader-name"
                        href={href(["admin", "agents", "seats", l.handle])}
                      >
                        {seat.name}
                      </a>
                    </span>
                    <span className="kpage-reader-via">
                      <span className="kpage-reader-how">
                        {`loaded ${plural(l.loaded, "time")} · offered ${plural(l.offered, "time")}`}
                      </span>
                      <span className="kpage-reader-when">
                        {" · "}
                        <time dateTime={l.last_at} title={fmtDateTime(l.last_at)}>
                          {relTime(l.last_at, now)}
                        </time>
                      </span>
                    </span>
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </Card>
  );
}
