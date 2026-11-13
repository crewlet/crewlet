/**
 * An entry's recent changes as a timeline, newest first, read from the
 * atlas's own log (`atlas_log`): what was recorded, marked as wrong or no
 * longer reported, when, and in whose voice. The sentence is the engine's;
 * a value and a name beside it are somebody's words, drawn as plain text.
 */

import { useMemo } from "react";
import { Button, Link, Skeleton, Text, Timeline } from "@crewlethq/ui";
import { SetByLine } from "~/app/frame/ObjectHeader.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import type { AtlasLogAnswer } from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { entryPath } from "./entry.tsx";
import { ATLAS_POLL_MS } from "./filters.ts";
import { FactValue, sourceSetBy } from "./parts.tsx";

type Line = AtlasLogAnswer["lines"][number];

const linesOf = (answer: AtlasLogAnswer) => answer.lines;
const idOf = (line: Line) => line.id;
const nextOf = (answer: AtlasLogAnswer) => answer.next_cursor ?? "";

export function Changes({
  id,
  limit,
}: {
  id: string;
  /** At most this many lines, with the way to the rest; unset, every page on request. */
  limit?: number;
}) {
  const now = useNow();
  const { socket } = useClient();
  const seatBadge = useSeatBadgeOf();
  const seatName = (handle: string) => seatBadge(handle).name;
  const asked = useMemo(() => ({ id }), [id]);
  const first = useQuery("atlas_log", asked, { pollMs: ATLAS_POLL_MS });
  const paged = usePaged(
    first,
    asked,
    // AT THIS TAB'S READ FLOOR, as the first page is asked.
    (cursor) =>
      socket.query("atlas_log", withFloor("atlas_log", JSON.stringify({ ...asked, cursor }))),
    linesOf,
    idOf,
    nextOf,
    { restartWhenRefused: true },
  );
  const lines = limit === undefined ? paged.items : paged.items.slice(0, limit);
  const more = paged.more || lines.length < paged.items.length;

  if (paged.loading && lines.length === 0) {
    return <Skeleton variant="text" rows={3} label="Loading the changes" />;
  }
  return (
    <QueryState
      error={paged.error}
      refusal={paged.refusal}
      detail={first.detail}
      loading={paged.loading}
    >
      {lines.length === 0 ? (
        <p className="muted">
          Nothing recorded yet. What the engine reads from the settings and its own records at every
          read leaves no line.
        </p>
      ) : (
        <>
          <Timeline itemLabel="Change" density="compact">
            {lines.map((line) => (
              <Timeline.Item key={line.id}>
                <span className="atlas-change">
                  <Text variant="cell">{line.text}</Text>
                  {line.value && <FactValue value={line.value} />}
                  {line.target &&
                    (line.target.id ? (
                      <Link href={href(entryPath(line.target.id))}>{line.target.name}</Link>
                    ) : (
                      <Text variant="cell">{line.target.name}</Text>
                    ))}
                </span>
                <SetByLine
                  setBy={sourceSetBy({ source: line.source, observed_at: line.at }, seatName, now)}
                  className="t-caption muted"
                />
              </Timeline.Item>
            ))}
          </Timeline>
          {more &&
            (limit === undefined ? (
              <Button variant="ghost" size="small" onClick={paged.next}>
                Show older changes
              </Button>
            ) : (
              <Link href={href(entryPath(id))}>Every change is on the entry&rsquo;s page</Link>
            ))}
        </>
      )}
    </QueryState>
  );
}
