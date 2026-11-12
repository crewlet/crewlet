/**
 * An entry beside the map or the list: what it is, what is wrong with it, its
 * facts and where each came from, and its neighbours on either side. The rail
 * draws the way to its page.
 */

import { EmptyState, Link, Section, Skeleton, Stack } from "@crewlethq/ui";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { ID_SHAPE } from "~/app/routes.ts";
import type { AtlasEntryAnswer, AtlasTraceAnswer } from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import {
  Facts,
  PEEK_RELATED,
  RelatedList,
  Signals,
  dependsOn,
  useEntryReads,
  usedBy,
} from "./entry.tsx";
import { HealthTag, VendorLead } from "./parts.tsx";
import { KINDS, STATES, VENDORS, WORDS } from "./words.ts";
import "~/styles/atlas.css";

export function AtlasPeek({ id }: { id: string }) {
  const valid = ID_SHAPE.test(id);
  const { entry, next } = useEntryReads(id, valid);
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
  return <EntryPeek answer={answer} next={next.data} />;
}

function EntryPeek({ answer, next }: { answer: AtlasEntryAnswer; next: AtlasTraceAnswer | null }) {
  const now = useNow();
  const { entry } = answer;
  const below = dependsOn(entry, next);
  const above = usedBy(answer);
  return (
    <>
      <ObjectHeader
        size="peek"
        kind={KINDS[entry.ref.kind].word}
        icon={KINDS[entry.ref.kind].glyph}
        identifier={entry.ref.key}
        title={entry.name}
        status={<PeekStatus answer={answer} />}
        facts={[
          ...(answer.vendor
            ? [
                {
                  label: "Vendor",
                  value: (
                    <span className="atlas-vendor">
                      <VendorLead vendor={answer.vendor} />
                      {VENDORS[answer.vendor].name}
                    </span>
                  ),
                },
              ]
            : []),
          { label: "Confidence", value: WORDS[entry.word] },
          { label: "Last seen", value: relTime(entry.last_seen, now) },
          { label: "Sources", value: entry.sources.length.toLocaleString() },
        ]}
      />
      <Stack gap={4}>
        <Signals entry={entry} now={now} />
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
        {above.length > 0 && (
          <Link href={href(["company", "atlas"], { peek: `atlas:${entry.id}`, trace: "up" })}>
            Show what depends on it on the map
          </Link>
        )}
      </Stack>
    </>
  );
}

/** The health word, and the entry's state where it is worth a word. */
function PeekStatus({ answer }: { answer: AtlasEntryAnswer }) {
  const { state } = answer.entry;
  return (
    <>
      <HealthTag health={answer.health} />
      {(state === "stale" || state === "retired") && <span className="muted">{STATES[state]}</span>}
    </>
  );
}
