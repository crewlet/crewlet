/**
 * An atlas finding of the setup advisor, fixed (ADR-0141): the entries the
 * finding names, each linked to its page, with the one control its fix asks
 * for. The engine names the atlas tool and what it already knows; the reader
 * adds the rest and sends each call from their own session, exactly as the
 * entry's own page would, so the tool's own gate decides it and the entry's
 * history records it.
 */

import { useMemo, useState } from "react";
import { Button, Callout, Link, List, ListItem, Modal, Text } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import type { AtlasKind, AtlasReviewItem } from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { AdvisorAtlasEntry, AdvisorFinding } from "~/protocol/advisor.ts";
import { ClaimList, DecisionButtons } from "./Decisions.tsx";
import { entryPath } from "./entry.tsx";
import { EntryPicker, OwnerField, type OwnerChoice } from "./pickers.tsx";
import { subjectOf } from "./Review.tsx";
import { KINDS, sourceName } from "./words.ts";
import { entryArg, reviewDecisions, undecided } from "./writes.ts";

/** What may watch a service: a monitor, or the account of the vendor that monitors it. */
const MONITOR_KINDS: readonly AtlasKind[] = ["monitor", "vendor_account"];

/** The most disagreements one read lists: the engine's largest page. */
const REVIEW_ROWS = 500;

/** Where a finding's own screen of the atlas is, and what its link is called. */
export function atlasPlace(finding: Pick<AdvisorFinding, "class">): {
  path: string[];
  label: string;
} {
  switch (finding.class) {
    case "atlas.disagreement":
      return { path: ["company", "atlas", "review"], label: "Open the atlas's review" };
    case "atlas.source_stale":
      return { path: ["company", "atlas", "sources"], label: "Open the atlas's sources" };
    default:
      return { path: ["company", "atlas"], label: "Open the atlas" };
  }
}

/** How many entries a finding is about, whatever it names. */
function counted(finding: AdvisorFinding): number {
  return finding.evidence.find((fact) => fact.key === "entries")?.value ?? 0;
}

/** The fix of one atlas finding: its entries, and the control each needs. */
export function AdvisorAtlasFix({
  finding,
  onClose,
}: {
  finding: AdvisorFinding;
  onClose: () => void;
}) {
  const entries = finding.entries ?? [];
  const asks = finding.fix.act?.asks ?? "";
  const place = atlasPlace(finding);
  const more = counted(finding) - entries.length;
  return (
    <Modal
      open
      onClose={onClose}
      title={finding.title}
      subtitle={finding.fix.sentence}
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <div className="col gap-3">
        <Text as="p" tone="secondary">
          {finding.why}
        </Text>
        {finding.fix.act?.tool === "atlas_refresh" ? (
          <RefreshRow source={finding.fix.act.args?.integration ?? ""} />
        ) : asks === "value" ? (
          <Disagreements entries={entries} />
        ) : (
          <List variant="divided">
            {entries.map((entry) => (
              <ListItem key={entry.id}>
                <div className="col gap-2">
                  <EntryLine entry={entry} />
                  {asks === "monitor" ? <MonitorRow entry={entry} /> : <OwnerRow entry={entry} />}
                </div>
              </ListItem>
            ))}
          </List>
        )}
        {more > 0 && (
          <Text as="p" tone="secondary">
            {plural(more, "more entry", "more entries")} {more === 1 ? "is" : "are"} listed here
            once these are fixed.
          </Text>
        )}
        <div>
          <Link href={href(place.path)} size="caption">
            {place.label}
          </Link>
        </div>
      </div>
    </Modal>
  );
}

/** One entry by name, linked to its page, with what it is and who was named for it. */
function EntryLine({ entry }: { entry: AdvisorAtlasEntry }) {
  return (
    <div className="col">
      <div>
        <Link href={href(entryPath(entry.id))}>{entry.name}</Link>
      </div>
      <Text variant="caption" tone="tertiary">
        {KINDS[entry.kind]?.word ?? entry.kind} <span className="mono">{entry.key}</span>
        {entry.owner ? `. Owner on record: ${entry.owner.name}, no longer in the chart` : ""}
      </Text>
    </div>
  );
}

/** Name the seat or the unit that answers for one entry. */
function OwnerRow({ entry }: { entry: AdvisorAtlasEntry }) {
  const write = useAct("atlas_set_owner");
  const [owner, setOwner] = useState<OwnerChoice>(null);
  return (
    <>
      <OwnerField value={owner} onChange={setOwner} nobody="Pick an owner" />
      <div>
        <WriteButton
          write={write}
          size="small"
          blocked={owner ? undefined : "Pick the seat or the unit that answers for it"}
          onPress={() => {
            if (!owner) return;
            void write.run(
              { atlas_entry: entryArg(entry), [owner.kind]: owner.name },
              { done: `Set the owner of ${entry.name}` },
            );
          }}
        >
          Set owner
        </WriteButton>
      </div>
    </>
  );
}

/** Record the monitor, or the monitoring account, that watches one service. */
function MonitorRow({ entry }: { entry: AdvisorAtlasEntry }) {
  const write = useAct("atlas_relate");
  const [to, setTo] = useState({ id: "", name: "" });
  return (
    <>
      <EntryPicker
        label="Watched by"
        helper="A monitor, or the account of the vendor that monitors it."
        kinds={MONITOR_KINDS}
        exclude={entry.id}
        value={to.id}
        onChange={(id, summary) => setTo({ id, name: summary?.name ?? "" })}
      />
      <div>
        <WriteButton
          write={write}
          size="small"
          blocked={to.id === "" ? "Pick what watches it" : undefined}
          onPress={() =>
            void write.run(
              { atlas_entry: entryArg(entry), relation: "monitored_by", to: to.id },
              { done: `Recorded that ${entry.name} is monitored by ${to.name}` },
            )
          }
        >
          Record it
        </WriteButton>
      </div>
    </>
  );
}

/** Ask for a read of one source now. */
function RefreshRow({ source }: { source: string }) {
  const write = useAct("atlas_refresh");
  const name = sourceName(source);
  return (
    <div>
      <WriteButton
        write={write}
        variant="secondary"
        blocked={source === "" ? "The finding names no source" : undefined}
        onPress={() =>
          void write.run({ integration: source }, { done: `Asked for a read of ${name}` })
        }
      >
        Read {name} now
      </WriteButton>
    </div>
  );
}

/**
 * What the named entries' sources disagree on, as the atlas's review reads it
 * now, with the review's own decisions: the finding says where to look, and
 * the review says what there is to decide.
 */
function Disagreements({ entries }: { entries: readonly AdvisorAtlasEntry[] }) {
  const now = useNow();
  const seatBadge = useSeatBadgeOf();
  const seatName = (handle: string) => seatBadge(handle).name;
  const asked = useMemo(() => ({ kind: "disagreement", limit: REVIEW_ROWS }), []);
  const review = useQuery("atlas_review", asked);
  const items = useMemo(() => {
    const named = new Set(entries.map((entry) => entry.id));
    return (review.data?.items ?? []).filter((item: AtlasReviewItem) => named.has(item.entry.id));
  }, [review.data, entries]);
  return (
    <QueryState
      error={review.error}
      refusal={review.refusal}
      detail={review.detail}
      loading={review.loading && !review.data}
    >
      {review.data && items.length === 0 && (
        <Callout variant="info">Nothing on these entries waits for a decision any more.</Callout>
      )}
      <List variant="divided">
        {items.map((item) => (
          <ListItem key={item.id}>
            <div className="col gap-2">
              <EntryLine entry={item.entry} />
              <Text variant="label">{subjectOf(item)}</Text>
              <ClaimList
                shown={item.fact ?? item.relation!}
                others={(item.fact ?? item.relation!).others ?? []}
                seatName={seatName}
                now={now}
              />
              {undecided(item) && (
                <Text variant="body" tone="secondary">
                  {undecided(item)}
                </Text>
              )}
              <DecisionButtons decisions={reviewDecisions(item)} />
            </div>
          </ListItem>
        ))}
      </List>
    </QueryState>
  );
}
