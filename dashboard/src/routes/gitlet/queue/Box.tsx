/** One merge queue as a box: its head, its required checks, its train base and its entries' parts. */

import type { ReactNode } from "react";
import {
  EmptyState,
  Meter,
  SegmentedMeter,
  Skeleton,
  Spinner,
  Tag,
  VisuallyHidden,
} from "@crewlethq/ui";
import {
  ArrowRightGlyph,
  ClockGlyph,
  GitCommitVerticalGlyph,
  InfoGlyph,
  ListChecksGlyph,
  ListOrderedGlyph,
  SquareCodeGlyph,
} from "@crewlethq/icons/glyphs";
import { AgoTime } from "~/components/AgoTime.tsx";
import { useNow } from "~/lib/clock.ts";
import { changeRef, refName, shortOID } from "~/lib/code.ts";
import { queueStateWord, type QueueBand, type QueuePlace } from "~/lib/checks.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeCheckRow } from "~/protocol/index.ts";
import { changeHref, changesHref, commitHref, repoHref } from "../paths.ts";
import { BranchChip, Face, useWhoName } from "../ui.tsx";
import { checksWords, elapsedWords, trainEnd, trainMeter, type Elapsed } from "./words.ts";

/** A gate's required checks as read: undefined while asked, null where a read was refused. */
export interface GateRead {
  checks: readonly string[] | null | undefined;
  /** The refusal's sentence, where a read was refused. */
  refusal?: string;
  /** Whether a matched gate asks an approval. */
  approval: boolean;
}

/** A candidate tip's checks as read. */
export interface TipRead {
  rows: readonly CodeCheckRow[] | undefined;
  error: string | null;
  detail: string | undefined;
}

const branchOf = (band: QueueBand) => refName(band.target).name;

/** The box's heading: the repository, its branch and how many it holds. */
export function BoxHead({ band, atLeast }: { band: QueueBand; atLeast: boolean }) {
  const count = band.places.length;
  // THE SPACES ARE TEXT, so the list it names reads "web into main, 2 in the queue".
  return (
    <>
      <SquareCodeGlyph size="sm" aria-hidden />
      <a className="code-queue__repo" href={repoHref(band.repo)}>
        {band.repo}
      </a>{" "}
      <ArrowRightGlyph size="sm" aria-hidden />
      <VisuallyHidden>into</VisuallyHidden> <BranchChip name={branchOf(band)} />
      <VisuallyHidden>,</VisuallyHidden>{" "}
      <Tag size="sm" appearance="outline">
        {count === 0 ? "Empty" : `${atLeast ? "at least " : ""}${count} in the queue`}
      </Tag>
    </>
  );
}

/** "Required checks: e2e, unit", from the gate of the rules the engine matched to the branch. */
export function RequiredChecks({ gate }: { gate: GateRead }) {
  const { checks } = gate;
  return (
    <span className="code-queue__required" title={checks === null ? gate.refusal : undefined}>
      <ListChecksGlyph size="sm" aria-hidden />
      {checks === undefined ? (
        <span className="muted">Reading the required checks</span>
      ) : checks === null ? (
        "Required checks are unavailable"
      ) : checks.length === 0 ? (
        "No required checks"
      ) : (
        <span>
          Required checks: <strong>{checks.join(", ")}</strong>
        </span>
      )}
    </span>
  );
}

/** The train base on the spine: the commit the box's first train is merged onto, and its subject. */
export function BaseRow({ repo, branch, base }: { repo: string; branch: string; base: string }) {
  const read = useCodeQuery("code_log", { name: repo, ref: base, limit: 1 });
  const subject = read.data?.log?.commits?.[0]?.subject;
  return (
    <>
      <span className="code-queue__node" aria-hidden="true">
        <GitCommitVerticalGlyph size="sm" />
      </span>
      <p className="code-queue__base">
        <strong>Train base</strong>
        <BranchChip name={branch} />
        <span>at</span>
        <a className="code-queue__oid mono" href={commitHref(repo, base)} title={base}>
          {shortOID(base)}
        </a>
        {subject && (
          <span className="code-queue__subject clamp" title={subject}>
            {subject}
          </span>
        )}
      </p>
    </>
  );
}

/** The place badge on the spine. */
export function PlaceBadge({ place }: { place: number }) {
  return (
    <span className="code-queue__place">
      <span aria-hidden="true">#{place}</span>
      <VisuallyHidden>Place {place}</VisuallyHidden>
    </span>
  );
}

/** The entry at `place - 1` in its box, where this page holds it. */
export function ahead(band: QueueBand, p: QueuePlace): QueuePlace | undefined {
  return band.places.find((q) => q.row.place === p.row.place - 1);
}

/** Who enqueued an entry, when, and where it stands. */
export function EntryMeta({ band, place }: { band: QueueBand; place: QueuePlace }) {
  const whoName = useWhoName();
  const by = place.row.enqueued_by.handle;
  const before = ahead(band, place);
  return (
    <>
      <span className="code-queue__by">
        <Face who={by} />
        <span>{whoName(by)}</span>
        <span>
          enqueued it <AgoTime value={place.row.enqueued_at} />
        </span>
      </span>
      {place.row.place === 1 ? (
        <span>· next to land</span>
      ) : (
        before && (
          <span>
            · waits behind{" "}
            <a className="prose-link mono" href={changeHref(before.row.repo, before.row.number)}>
              {changeRef(before.row.repo, before.row.number)}
            </a>
          </span>
        )
      )}
    </>
  );
}

/** Where an entry stands: Testing with its spinner, Waiting, or the engine's word. */
export function EntryState({ state }: { state: string }) {
  if (state === "testing") {
    return (
      <Tag size="sm" variant="info" leadingIcon={<Spinner size="xs" />}>
        Testing
      </Tag>
    );
  }
  if (state === "waiting") {
    return (
      <Tag size="sm" appearance="outline" leadingIcon={<ClockGlyph size="xs" />}>
        Waiting
      </Tag>
    );
  }
  return (
    <Tag size="sm" appearance="outline">
      {queueStateWord(state)}
    </Tag>
  );
}

/** One line of the checks cell: a meter or a bar, then its words. */
function Line({ mark, children }: { mark: ReactNode; children: ReactNode }) {
  return (
    <span className="code-queue__line">
      <span className="code-queue__mark">{mark}</span>
      <span className="code-queue__words">{children}</span>
    </span>
  );
}

/** An entry's train: its required checks as a meter, and how long they have run. */
export function EntryChecks({
  band,
  place,
  gate,
  tip,
}: {
  band: QueueBand;
  place: QueuePlace;
  gate: GateRead;
  tip: TipRead | undefined;
}) {
  const names = gate.checks ?? null;
  if (!place.candidate) {
    const meter = trainMeter(names, []);
    const before = ahead(band, place);
    return (
      <div className="code-queue__checks">
        <Line
          mark={
            <SegmentedMeter size="compact" decorative segments={meter.parts} total={meter.total} />
          }
        >
          {checksWords(names, [])}
        </Line>
        <Line mark={null}>
          {place.row.place === 1 ? (
            "starts next"
          ) : before ? (
            <>
              starts after <strong>{changeRef(before.row.repo, before.row.number)}</strong>
            </>
          ) : (
            "waits its turn"
          )}
        </Line>
      </div>
    );
  }
  if (tip?.error) {
    return (
      <p className="code-queue__checks muted">
        Its checks are unavailable{tip.detail ? `: ${tip.detail}` : ""}.
      </p>
    );
  }
  if (!tip?.rows) {
    return <Skeleton variant="text" rows={1} label="Reading its checks" />;
  }
  const meter = trainMeter(names, tip.rows);
  return (
    <div className="code-queue__checks">
      <Line
        mark={
          <SegmentedMeter size="compact" decorative segments={meter.parts} total={meter.total} />
        }
      >
        {checksWords(names, tip.rows)}
      </Line>
      <TrainTime gate={names} rows={tip.rows} />
    </div>
  );
}

/** The elapsed bar: still once the checks ended, else against the one clock. */
function TrainTime({
  gate,
  rows,
}: {
  gate: readonly string[] | null;
  rows: readonly CodeCheckRow[];
}) {
  const end = trainEnd(gate, rows);
  return end === null ? (
    <Ticking gate={gate} rows={rows} />
  ) : (
    <TimeLine elapsed={elapsedWords(gate, rows, end)} />
  );
}

/** A tick re-renders this line alone. */
function Ticking({
  gate,
  rows,
}: {
  gate: readonly string[] | null;
  rows: readonly CodeCheckRow[];
}) {
  const now = useNow();
  return <TimeLine elapsed={elapsedWords(gate, rows, now)} />;
}

function TimeLine({ elapsed }: { elapsed: Elapsed }) {
  return (
    <Line
      mark={
        elapsed.bar && (
          <Meter
            size="compact"
            tone="quantity"
            hideLabel
            label="Time its checks have run"
            value={elapsed.bar.value}
            max={elapsed.bar.max}
            valueText={elapsed.words}
          />
        )
      }
    >
      <span aria-hidden={elapsed.bar ? true : undefined}>{elapsed.words}</span>
    </Line>
  );
}

/** Under a box's entries, how the queue lands them; under an empty box, how a change joins it. */
export function BoxNote({ band, approval }: { band: QueueBand; approval: boolean }) {
  const branch = branchOf(band);
  if (band.places.length > 0) {
    return (
      <p className="code-queue__note">
        <InfoGlyph size="sm" aria-hidden />
        <span>
          A change is tested merged onto {branch} and every change ahead of it, and lands when its
          required checks pass. One that fails is taken out, and the changes behind it move up.
        </span>
      </p>
    );
  }
  return (
    <EmptyState
      size="compact"
      headingLevel={4}
      icon={<ListOrderedGlyph size={24} />}
      title={`Nothing is queued for ${branch}`}
      description={`A change joins this queue from its merge box once ${approval ? "it is approved" : "its gate is met"}, and lands when its required checks pass on the merge.`}
      action={
        <a className="t-link code-queue__open" href={changesHref({ repo: band.repo })}>
          Open changes
          <ArrowRightGlyph size="sm" aria-hidden />
        </a>
      }
    />
  );
}
