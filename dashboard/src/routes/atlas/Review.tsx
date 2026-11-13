/**
 * Company › Atlas › Review: what waits for a person. The engine computes it
 * on every read and stores none of it: the facts and relations the sources
 * disagree on, then the entries only an agent reported. An agent's record is
 * shown with its evidence: the seat, the turn, the call it cited and what that
 * call is evidence of. Each item carries the decisions it allows; a decision is an ordinary write in the person's own
 * name, and the item is gone from the next read once its cause is.
 *
 * Several items are decided together from the bar over the list. A bulk
 * decision lists what it decides in a dialog and sends only on a second
 * press, one write an item, and says which the engine refused.
 */

import { useMemo, useState } from "react";
import {
  Button,
  Card,
  Checkbox,
  EmptyState,
  Link,
  Modal,
  PageNote,
  SelectionBar,
  Skeleton,
  Tabs,
  Tag,
  Text,
  useToast,
} from "@crewlethq/ui";
import { SetByLine } from "~/app/frame/ObjectHeader.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import type {
  AtlasReport,
  AtlasReviewAnswer,
  AtlasReviewItem,
  AtlasReviewKind,
} from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { andList } from "~/lib/words.ts";
import { WAITING } from "./AddEntry.tsx";
import { ClaimList, DecisionButtons, decide, useDecisionWrites } from "./Decisions.tsx";
import { entryPath } from "./entry.tsx";
import { ATLAS_POLL_MS } from "./filters.ts";
import { Cited, EntryDisc, sourceSetBy } from "./parts.tsx";
import { KINDS, RELATIONS, factLabel } from "./words.ts";
import { reviewDecisions, undecided, type Decision } from "./writes.ts";

/** What each reason to wait is called, one and many, and what it means. */
export const REVIEW_KINDS: Readonly<
  Record<AtlasReviewKind, { one: string; many: string; why: string }>
> = {
  disagreement: {
    one: "Disagreement",
    many: "Disagreements",
    why: "Its sources disagree. Both values are shown, and nothing is settled until a person decides.",
  },
  unconfirmed: {
    one: "Unconfirmed",
    many: "Unconfirmed",
    why: "Only an agent reported it, with nothing the engine could check. It stays off the map until somebody confirms it.",
  },
  likely: {
    one: "Likely",
    many: "Likely",
    why: "An agent reported it, backed by a call or a file it cites. No person, integration or setting stands behind it yet.",
  },
};

const KIND_ORDER = Object.keys(REVIEW_KINDS) as AtlasReviewKind[];
const ALL = "all";

const itemsOf = (answer: AtlasReviewAnswer) => answer.items;
const idOf = (item: AtlasReviewItem) => item.id;
const nextOf = (answer: AtlasReviewAnswer) => answer.next_cursor ?? "";

/** What a waiting item is about, in a few words: the fact, the relation, or the entry itself. */
function subjectOf(item: AtlasReviewItem): string {
  if (item.fact) return factLabel(item.fact);
  if (item.relation)
    return `${RELATIONS[item.relation.relation]} ${item.target?.name ?? ""}`.trim();
  return KINDS[item.entry.kind].word;
}

export function Review() {
  const now = useNow();
  const { socket } = useClient();
  const seatBadge = useSeatBadgeOf();
  const seatName = (handle: string) => seatBadge(handle).name;
  const [kind, setKind] = useParam("kind", ALL);
  const asked = useMemo(() => (kind === ALL ? {} : { kind }), [kind]);
  const first = useQuery("atlas_review", asked, { pollMs: ATLAS_POLL_MS });
  const paged = usePaged(
    first,
    asked,
    (cursor) =>
      socket.query("atlas_review", withFloor("atlas_review", JSON.stringify({ ...asked, cursor }))),
    itemsOf,
    idOf,
    nextOf,
    { restartWhenRefused: true },
  );
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const [bulk, setBulk] = useState<"accept" | "retire" | null>(null);
  const items = paged.items;
  // A PICK OUTLIVES ITS ITEM NOWHERE: one decided since is no longer picked.
  const chosen = items.filter((item) => picked.has(item.id));
  const counts = first.data?.counts ?? {};
  const all = KIND_ORDER.reduce((sum, one) => sum + (counts[one] ?? 0), 0);
  const whole = chosen.every((item) => !item.fact && !item.relation);
  const toggle = (id: string, on: boolean) =>
    setPicked((held) => {
      const next = new Set(held);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });

  return (
    <div className="atlas-screen">
      <PageNote>
        What waits for a person. The atlas works it out on every read and shows both sides: it never
        settles a disagreement or confirms an entry on its own.
      </PageNote>
      <PageToolbar>
        <Tabs
          variant="chip"
          ariaLabel="What waits"
          value={kind}
          onValueChange={(next) => {
            setKind(next);
            setPicked(new Set());
          }}
          items={[
            { value: ALL, label: "Everything", count: first.data ? all : null },
            ...KIND_ORDER.map((one) => ({
              value: one,
              label: REVIEW_KINDS[one].many,
              count: first.data ? (counts[one] ?? 0) : null,
            })),
          ]}
        />
      </PageToolbar>
      <SelectionBar
        count={chosen.length}
        onClear={() => setPicked(new Set())}
        labels={{ label: "Decide the picked items" }}
      >
        <Button size="small" variant="primary" onClick={() => setBulk("accept")}>
          Accept as shown
        </Button>
        <Button
          size="small"
          variant="secondary"
          disabledReason={
            whole ? undefined : "Only an entry is retired. Unpick the disagreements first."
          }
          onClick={() => setBulk("retire")}
        >
          Retire
        </Button>
      </SelectionBar>
      {paged.loading && items.length === 0 ? (
        <Skeleton variant="text" rows={6} label="Reading what waits" />
      ) : (
        <QueryState
          error={paged.error}
          refusal={paged.refusal}
          detail={first.detail}
          loading={paged.loading}
        >
          {items.length === 0 ? (
            <EmptyState
              title="Nothing waits for a person"
              description={
                kind === ALL
                  ? "The sources agree on every fact, and somebody stands behind every entry. What an integration or an agent reports that differs from a person’s value shows up here."
                  : "Nothing of this kind waits. The other tabs say what does."
              }
            />
          ) : (
            <div className="atlas-review">
              {items.map((item) => {
                const words = REVIEW_KINDS[item.kind];
                return (
                  <Card key={item.id} variant="outlined" padding="md">
                    <div className="atlas-review__item">
                      <Checkbox
                        label={`Pick ${item.entry.name}, ${subjectOf(item)}`}
                        hideLabel
                        checked={picked.has(item.id)}
                        onCheckedChange={(on) => toggle(item.id, on)}
                      />
                      <div className="atlas-review__body">
                        <div className="atlas-review__head">
                          <EntryDisc entry={item.entry} size="sm" />
                          <Link href={href(entryPath(item.entry.id))}>{item.entry.name}</Link>
                          <Tag variant="neutral" size="xs" appearance="outline">
                            {words.one}
                          </Tag>
                          <Text variant="caption" tone="tertiary">
                            Waiting since {relTime(item.since, now)}
                          </Text>
                        </div>
                        {item.fact || item.relation ? (
                          <>
                            <Text variant="label">{subjectOf(item)}</Text>
                            <ClaimList
                              shown={item.fact ?? item.relation!}
                              others={(item.fact ?? item.relation!).others ?? []}
                              seatName={seatName}
                              now={now}
                            />
                          </>
                        ) : (
                          <>
                            <Text variant="body" tone="secondary">
                              {KINDS[item.entry.kind].word}{" "}
                              <span className="mono">{item.entry.key}</span>. {words.why}
                            </Text>
                            <Reports reports={item.reports ?? []} seatName={seatName} now={now} />
                          </>
                        )}
                        {undecided(item) && (
                          <Text variant="body" tone="secondary">
                            {undecided(item)}
                          </Text>
                        )}
                        <DecisionButtons decisions={reviewDecisions(item)} />
                      </div>
                    </div>
                  </Card>
                );
              })}
              {paged.more && (
                <div>
                  <Button variant="ghost" size="small" loading={paged.paging} onClick={paged.next}>
                    Show more
                  </Button>
                </div>
              )}
            </div>
          )}
        </QueryState>
      )}
      {bulk && (
        <BulkDialog
          how={bulk}
          items={chosen}
          onClose={() => setBulk(null)}
          onDone={(left) => {
            setPicked(new Set(left));
            setBulk(null);
          }}
        />
      )}
    </div>
  );
}

/**
 * What each agent said behind an entry that waits, newest first: who reported
 * it and in which turn, as the by-line every claim carries, then the call it
 * cited. The turn's link opens the run, where the call's own answer is.
 */
function Reports({
  reports,
  seatName,
  now,
}: {
  reports: readonly AtlasReport[];
  seatName: (handle: string) => string;
  now: number;
}) {
  if (reports.length === 0) return null;
  return (
    <ul className="atlas-claims" aria-label="What was reported, and the call behind it">
      {reports.map((report) => (
        <li key={`${report.source.name ?? ""}:${report.source.turn ?? ""}`}>
          <SetByLine setBy={sourceSetBy(report, seatName, now)} className="t-caption muted" />
          <Cited source={report.source} />
        </li>
      ))}
    </ul>
  );
}

/** Who reported an entry that waits, for the line a bulk decision lists it on. */
function reportedBy(item: AtlasReviewItem, seatName: (handle: string) => string): string {
  const names = [...new Set((item.reports ?? []).map((one) => seatName(one.source.name ?? "")))];
  return names.length > 0 ? `, reported by ${andList(names)}` : "";
}

/** The decision a bulk press takes on one item. */
function bulkDecision(item: AtlasReviewItem, how: "accept" | "retire"): Decision | undefined {
  return reviewDecisions(item).find((one) =>
    how === "accept" ? one.accepts : one.key === "retire",
  );
}

/**
 * The bulk decision, listed before it is sent: every item and what is decided
 * on it. It sends on this dialog's own press, one write an item in order, and
 * reports what landed and what the engine refused, which stays picked.
 */
function BulkDialog({
  how,
  items,
  onClose,
  onDone,
}: {
  how: "accept" | "retire";
  items: readonly AtlasReviewItem[];
  onClose: () => void;
  /** Called with the ids still to decide: the ones refused or not confirmed. */
  onDone: (left: string[]) => void;
}) {
  const writes = useDecisionWrites();
  const toast = useToast();
  const seatBadge = useSeatBadgeOf();
  const seatName = (handle: string) => seatBadge(handle).name;
  const [busy, setBusy] = useState(false);
  const planned = items.flatMap((item) => {
    const decision = bulkDecision(item, how);
    return decision ? [{ item, decision }] : [];
  });
  const gate = how === "accept" ? writes.atlas_confirm : writes.atlas_retire;
  const blocked = planned.length === 0 ? "Nothing picked can be decided this way" : undefined;
  const submit = async () => {
    if (!pressable(gate, blocked) || busy) return;
    setBusy(true);
    let landed = 0;
    const refused: { id: string; why: string }[] = [];
    for (const { item, decision } of planned) {
      const result = await decide(writes, decision, true);
      if (result?.kind === "applied" || result?.kind === "pending") landed += 1;
      else if (result?.kind === "refused") {
        refused.push({ id: item.id, why: `${item.entry.name}: ${result.sentence}` });
      } else if (result?.kind === "unknown") {
        refused.push({ id: item.id, why: `${item.entry.name}: not confirmed. ${result.reason}` });
      } else refused.push({ id: item.id, why: `${item.entry.name}: not sent.` });
    }
    setBusy(false);
    for (const tool of new Set(planned.map((one) => one.decision.tool))) writes[tool].dismiss();
    toast.show({
      variant: refused.length === 0 ? "success" : "warning",
      title:
        how === "accept"
          ? `Accepted ${plural(landed, "item")} as shown`
          : `Retired ${plural(landed, "entry", "entries")}`,
      message:
        refused.length === 0
          ? undefined
          : `${plural(refused.length, "item")} not decided. ${refused.map((one) => one.why).join(" ")}`,
    });
    onDone(refused.map((one) => one.id));
  };
  return (
    <Modal
      open
      size="md"
      phoneSheet
      stackBody
      title={
        how === "accept"
          ? `Accept ${plural(planned.length, "item")} as shown?`
          : `Retire ${plural(planned.length, "entry", "entries")}?`
      }
      subtitle={
        how === "accept"
          ? "Each value or entry the atlas shows becomes confirmed by you. Another source’s differing value stays in the history."
          : "Each leaves the map at once and stays in its history. Restore brings one back."
      }
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason={busy ? WAITING : undefined}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <WriteButton
            write={gate}
            variant="primary"
            showRefusal={false}
            blocked={blocked}
            pressing={busy}
            onPress={() => void submit()}
          >
            {how === "accept" ? "Accept as shown" : "Retire them"}
          </WriteButton>
        </>
      }
    >
      <ul className="atlas-list" aria-label="What this decides">
        {planned.map(({ item, decision }) => (
          <li key={item.id}>
            <strong>{item.entry.name}</strong>: {decision.label.toLowerCase()}
            {item.fact || item.relation ? ` (${subjectOf(item)})` : reportedBy(item, seatName)}
          </li>
        ))}
      </ul>
      {!gate.access.can && <p className="muted">{gate.access.reason}</p>}
    </Modal>
  );
}
