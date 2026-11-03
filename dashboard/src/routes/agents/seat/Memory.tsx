/**
 * What a seat remembers and — its Sessions, last (`Sessions.tsx`) — what its
 * harness wrote on each turn and how each turn ended, which is also what a
 * conversation's next turn continues and is told.
 *
 * # Read from the node that holds the seat
 *
 * A seat's memory is written to the store of the node running it, and a
 * compacted changelog carries it to whichever node holds the seat next — so
 * the holder's copy is the one kept current, and any other node's may be a
 * placement behind. The memory here is the HOLDER'S (`held_by`), asked across
 * the fleet; a seat no node holds answers `none`, with nothing in it, and the
 * tab says why rather than drawing a seat that remembers nothing. The Sessions
 * are not: their rows are the replicated estate's and their bytes the fleet's
 * object store's, so any node answers.
 *
 * # A header counts the whole, a list shows the newest
 *
 * Every header is the TOTAL the holder counted — never the length of the page
 * it sent, which is fifty at most — and every list cut short says "Latest 50
 * of 142" in its header, the same words on all four (`components/memory.tsx`,
 * whose diary and episode cards Doclet › Agent diaries draws too), so the
 * two cannot silently disagree.
 */

import { Card, EmptyState, Skeleton, Tag } from "@crewlethq/ui";
import { UsersGlyph, ZapGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { cutThen, DiaryCard, EpisodesCard, heldWords, pageWords } from "~/components/memory.tsx";
import { fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { Seat } from "~/lib/seats.ts";
import { Sessions } from "./Sessions.tsx";
import type { CounterpartyProfile } from "~/contract/memory.ts";

export function Memory({ seat, now }: { seat: Seat; now: number }) {
  const handle = seat.handle;
  const memory = useQuery("agent_memory", { id: handle }, { enabled: handle !== "" });
  const data = memory.data;
  return (
    <div className="col gap-4">
      {memory.loading && !data && (
        <Skeleton variant="text" rows={5} label="Loading this seat's memory" />
      )}
      <QueryState error={memory.error} refusal={memory.refusal} loading={memory.loading && !data}>
        {data && (
          <>
            <p className="t-caption">{heldWords(data.held_by)}</p>
            <DiaryCard memory={data} />
            <EpisodesCard memory={data} now={now} />

            <Card padding="none">
              <Card.Header
                icon={<ZapGlyph size="sm" />}
                count={data.skills_total}
                subtitle={cutThen(
                  pageWords(data.skills.length, data.skills_total),
                  "learned from repeated work",
                )}
              >
                <Card.Title as="h3">Skills it taught itself</Card.Title>
              </Card.Header>
              {data.skills.length ? (
                <div className="list">
                  {data.skills.map((s) => (
                    <div key={s.id || s.key} className="thread-entry">
                      <div className="row gap-1">
                        <strong className="t-body">{s.title}</strong>
                        <Tag appearance="outline">v{s.version}</Tag>
                        {s.uses > 0 && (
                          <span className="t-caption">used {plural(s.uses, "time")}</span>
                        )}
                        <span className="spacer" />
                        {s.updated_at && (
                          <span className="t-caption">{fmtDateTime(s.updated_at)}</span>
                        )}
                      </div>
                      {s.summary && <p className="t-caption">{s.summary}</p>}
                    </div>
                  ))}
                </div>
              ) : (
                <EmptyState
                  size="compact"
                  icon={<ZapGlyph size={32} />}
                  title="No learned skills"
                  description="Skills learned from repeated work appear here."
                />
              )}
            </Card>

            <Card padding="none">
              <Card.Header
                icon={<UsersGlyph size="sm" />}
                count={data.counterparties_total}
                subtitle={
                  pageWords(data.counterparties.length, data.counterparties_total) ?? undefined
                }
              >
                <Card.Title as="h3">Who it has worked with</Card.Title>
              </Card.Header>
              {data.counterparties.length ? (
                <div className="list">
                  {data.counterparties.map((c, i) => (
                    <CounterpartyRow key={`${counterpartyKey(c)}-${i}`} profile={c} now={now} />
                  ))}
                </div>
              ) : (
                <EmptyState
                  size="compact"
                  icon={<UsersGlyph size={32} />}
                  title="No counterparty profiles"
                  description="They build up as it works with others."
                />
              )}
            </Card>
          </>
        )}
      </QueryState>
      <Sessions seat={seat} now={now} />
    </div>
  );
}

/** A counterparty's stable identity, for a key and for a link.
 *
 *  THE NAME IS NOT IT. A profile's identity is the seat handle, or the
 *  platform and external id for somebody this company has not mapped — the
 *  display name is deliberately excluded, because a person renaming
 *  themselves on a chat surface must not look like a different colleague.
 */
export function counterpartyKey(profile: CounterpartyProfile): string {
  const { handle, platform, external_id } = profile.subject;
  return handle || `${platform ?? "?"}:${external_id ?? "?"}`;
}

/** How far `last_updated_at` may run ahead of `last_corroborated_at` before
 *  the profile is called stale.
 *
 *  THIRTY DAYS, which is the shortest inbox retention this engine allows and
 *  therefore the shortest span over which "still working together" is a fact
 *  the company still holds evidence for. Shorter and every colleague seen
 *  twice in a week reads as stale; longer and a profile nobody has
 *  corroborated since last quarter looks current. */
const STALE_TRAIT_MS = 30 * 24 * 60 * 60 * 1000;

/** One colleague this seat has learned about.
 *
 *  BOTH INSTANTS: `last_updated_at` moves on every interaction and
 *  `last_corroborated_at` only when the traits changed. A colleague seen daily
 *  whose profile has not moved in months is one this seat has STOPPED
 *  learning about — the gap the prompt's own prefetch demotes stale traits on.
 *
 *  THE TRAITS ARE A BAG: the model invents the keys, so they are listed as
 *  they come rather than drawn as whichever three fields appeared first.
 */
export function CounterpartyRow({ profile, now }: { profile: CounterpartyProfile; now: number }) {
  const traits = Object.entries(profile.traits ?? {});
  const stale =
    profile.last_corroborated_at &&
    profile.last_updated_at &&
    tsKey(profile.last_updated_at) - tsKey(profile.last_corroborated_at) > STALE_TRAIT_MS;
  return (
    <div className="thread-entry">
      <div className="row gap-1">
        {profile.subject.handle ? (
          <a className="t-cell" href={href(["company", "org", "seats", profile.subject.handle])}>
            <strong>{profile.subject.name || profile.subject.handle}</strong>
          </a>
        ) : (
          <strong className="t-cell">{profile.subject.name || counterpartyKey(profile)}</strong>
        )}
        {!profile.resolved && (
          <Tag appearance="outline" title="not mapped to a seat in this company">
            {profile.subject.platform || "external"}
          </Tag>
        )}
        <span className="spacer" />
        <span className="t-caption">{plural(profile.interactions, "interaction")}</span>
        {/* RELATIVE, like every other list on the profile (Episodes,
            Sessions), the instant on hover: a full timestamp with seconds was
            the one row that read differently. */}
        {profile.last_updated_at && (
          <span className="t-caption" title={fmtDateTime(profile.last_updated_at)}>
            {relTime(profile.last_updated_at, now)}
          </span>
        )}
      </div>
      {traits.length > 0 ? (
        <div className="row gap-1 wrap">
          {traits.map(([key, value]) => (
            <Tag key={key} appearance="outline" title={key}>
              {key}: {typeof value === "string" ? value : JSON.stringify(value)}
            </Tag>
          ))}
        </div>
      ) : (
        <p className="t-caption">Seen, and nothing believed about them yet.</p>
      )}
      {stale && (
        <p className="t-caption">
          Last confirmed {fmtDateTime(profile.last_corroborated_at)}. No longer learning about them.
        </p>
      )}
    </div>
  );
}
