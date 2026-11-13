/**
 * An entry's sheet, beside the map and the catalogue: its identity and its
 * health, what is wrong with it, the entry among its neighbours in the map's
 * own look, its facts and where each came from, its neighbours by name, the
 * open tasks that name it, and its recent changes. The rail draws the way to
 * its page.
 */

import { EmptyState, Link, Section, Skeleton, Stack } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { ID_SHAPE } from "~/app/routes.ts";
import type { AtlasEntryAnswer } from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { Changes } from "./Changes.tsx";
import {
  EntryHead,
  Facts,
  PEEK_RELATED,
  RelatedList,
  Signals,
  Touches,
  dependsOn,
  namesOf,
  selfOf,
  useEntryReads,
  usedBy,
  type EntryReads,
} from "./entry.tsx";
import { EntryActions } from "./EntryActions.tsx";
import { SELECT_KEY } from "./filters.ts";
import { PeekWork, useEntryWork } from "./Work.tsx";

/** At most this many changes a sheet lists before pointing at the page. */
const PEEK_CHANGES = 6;

export function AtlasPeek({ id }: { id: string }) {
  const valid = ID_SHAPE.test(id);
  const reads = useEntryReads(id, valid);
  const { entry } = reads;
  if (!valid) {
    return (
      <EmptyState
        size="compact"
        title="Not an entry"
        description={`“${id}” names no entry: an entry is addressed by its id.`}
      />
    );
  }
  const answer = entry.data;
  if (entry.loading && !answer) return <Skeleton variant="text" rows={6} label="Loading" />;
  if (!answer) {
    return (
      <QueryState error={entry.error} refusal={entry.refusal} detail={entry.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return <EntryPeek answer={answer} reads={reads} />;
}

function EntryPeek({ answer, reads }: { answer: AtlasEntryAnswer; reads: EntryReads }) {
  const now = useNow();
  const { entry } = answer;
  const self = selfOf(reads);
  const below = dependsOn(entry, reads.next.data);
  const above = usedBy(answer);
  const work = useEntryWork(entry.id);
  return (
    <>
      <EntryHead answer={answer} self={self} size="peek" now={now} />
      <EntryActions answer={answer} names={namesOf(reads)} />
      <Stack gap={4}>
        <Signals entry={entry} now={now} />
        <Touches self={self} reads={reads} stacked headingLevel={3} />
        <Section spacing="tight" headingLevel={3} title="Facts">
          <Facts entry={entry} now={now} />
        </Section>
        <Section
          spacing="tight"
          headingLevel={3}
          title={`Depends on (${below.length.toLocaleString()})`}
        >
          <RelatedList related={below} limit={PEEK_RELATED} none="It depends on no other entry." />
        </Section>
        <Section
          spacing="tight"
          headingLevel={3}
          title={`Used by (${above.length.toLocaleString()})`}
        >
          <RelatedList related={above} limit={PEEK_RELATED} none="No other entry depends on it." />
        </Section>
        <PeekWork id={entry.id} reading={work} />
        <Section spacing="tight" headingLevel={3} title="Recent changes">
          <Changes id={entry.id} limit={PEEK_CHANGES} />
        </Section>
        <Link href={href(["company", "atlas"], { [SELECT_KEY]: entry.id })}>
          Show it on the map
        </Link>
      </Stack>
    </>
  );
}
