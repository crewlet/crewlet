/**
 * A change's rail: its reviewers, readiness, work item, owner, branches, head, checks,
 * participants, reference and whom it notified. It reads the target branch and the routing itself.
 */

import { useState, type ReactNode } from "react";
import { Button, Count, EmptyValue, SegmentedMeter, Tag, VisuallyHidden } from "@crewlethq/ui";
import {
  ArrowDownGlyph,
  ArrowUpRightGlyph,
  BellGlyph,
  CheckGlyph,
  FileDiffGlyph,
  MessageSquareGlyph,
  ShieldCheckGlyph,
  ShieldUserGlyph,
  SquareKanbanGlyph,
  UserPlusGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { PageFailed } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { ago } from "~/lib/ago.ts";
import { checkStateWord } from "~/lib/checks.ts";
import { changeLive, codeReasonPhrase, refName } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { Paged } from "~/lib/usePaged.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import { itemPath } from "~/lib/work.ts";
import type {
  CodeChangeAnswer,
  CodeChangeDetail,
  CodeReviewerRow,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { preconditionTone } from "../ChangeRow.tsx";
import { protectionTitle, useBranchDetail } from "../branchDetail.ts";
import { ownedPaths } from "../owners.ts";
import { changeHref, changesHref, commitHref } from "../paths.ts";
import { checksWords, readinessCount, readinessWaits, verdictPhrase } from "../readinessWords.ts";
import { BranchChip, Face, ShaChip, useWhoName } from "../ui.tsx";
import { AskDialog } from "./AskDialog.tsx";
import { verdictTone } from "./Conversation.tsx";
import { CopyButton } from "./CopyButton.tsx";
import { CheckMark } from "../checks/marks.tsx";
import { ReRequest } from "./Merge.tsx";
import { checksKept, type Standing } from "./mergeSections.ts";
import { partMovedBy, refusedWords, useAskAgainWhenMoved, type LatestChecks } from "./reads.ts";
import type { DiscTone } from "./Timeline.tsx";
import "~/styles/gitlet.css";

/** A section of the rail: its heading and what sits at its end, then its rows. */
function RailSection({
  title,
  end,
  children,
}: {
  title: string;
  end?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="code-rail__section" aria-label={title}>
      <div className="code-rail__head">
        <h2 className="code-rail__title">{title}</h2>
        {end}
      </div>
      {children}
    </section>
  );
}

/** The engine's sentence on a refused read, "" where it wrote none. */
function sentenceOf(read: { refusal: unknown; detail?: string | undefined }): string {
  return (read.refusal as { detail?: string | null } | null)?.detail ?? read.detail ?? "";
}

/** A verdict's mark at a reviewer's row end, in its verdict's tone, the verdict said beside it. */
function VerdictMark({ verdict }: { verdict: string }) {
  const glyph =
    verdict === "request_changes" ? (
      <FileDiffGlyph size="sm" />
    ) : verdict === "approve" ? (
      <CheckGlyph size="sm" />
    ) : (
      <MessageSquareGlyph size="sm" />
    );
  return (
    <span className="code-rail__mark" data-tone={verdictTone(verdict)} aria-hidden="true">
      {glyph}
    </span>
  );
}

/** An unanswered ask's dot: a seat's wait in info, a person's in warning, as the meter tones them. */
function waitingTone(kind: string | undefined): DiscTone {
  return kind === "agent" ? "info" : "warning";
}

/** Whom the change asked for a review, each with where they stand and a re-request. */
function Reviewers({
  change,
  standings,
  again,
  asked,
}: {
  change: CodeChangeDetail;
  standings: readonly Standing[];
  again: readonly string[];
  asked: Paged<CodeReviewerRow, CodeChangeAnswer>;
}) {
  const whoName = useWhoName();
  const viewer = useViewer();
  const standingOf = new Map(standings.map((s) => [s.handle, s]));
  // A REVIEWER NOBODY ASKED is listed too: their verdict is a fact about the change.
  const unasked = standings
    .map((s) => s.handle)
    .filter((who) => !asked.items.some((a) => a.reviewer === who));
  if (asked.loading && asked.items.length === 0 && unasked.length === 0) {
    return <p className="code-rail__note">Reading who is asked…</p>;
  }
  if (asked.items.length === 0 && unasked.length === 0) {
    return (
      <p className="code-rail__note">
        {asked.error ? <PageFailed error={asked.error} /> : "Nobody is asked to review it yet."}
      </p>
    );
  }
  const row = (who: string, ask: CodeReviewerRow | undefined) => {
    const s = standingOf.get(who);
    const waiting = !s && ask?.state === "requested";
    const words = s
      ? `${verdictPhrase(s.verdict)} revision ${s.revision}`
      : ask?.state === "withdrawn"
        ? "no longer asked"
        : who === viewer.handle
          ? "waiting on you"
          : "asked, not answered yet";
    const owns = ask?.reason === "path_owner" ? ownedPaths(ask) : "";
    return (
      <li key={`${ask?.kind ?? "unasked"}:${who}`}>
        <Face who={who} ring={who === viewer.handle ? "brand" : undefined} />
        <span className="code-rail__person">
          <span className="code-rail__line">
            <a className="code-rail__name" href={changesHref({ reviewer: who })}>
              {whoName(who)}
            </a>
            {ask?.reason === "path_owner" && (
              <Tag
                size="xs"
                appearance="outline"
                leadingIcon={<ShieldUserGlyph size="xs" />}
                title={
                  owns
                    ? `Owns ${owns}, which this change touches`
                    : "Owns paths this change touches"
                }
              >
                path owner
              </Tag>
            )}
          </span>
          <span className="code-rail__note">{words}</span>
        </span>
        <span className="code-rail__end">
          {s && <VerdictMark verdict={s.verdict} />}
          {waiting && (
            <span
              className="code-rail__dot"
              data-tone={waitingTone(ask?.kind)}
              title={`Waiting on ${who === viewer.handle ? "you" : whoName(who)}`}
            >
              <VisuallyHidden>waiting</VisuallyHidden>
            </span>
          )}
          {again.includes(who) && <ReRequest change={change} handles={[who]} icon />}
        </span>
      </li>
    );
  };
  return (
    <>
      <ul className="code-rail__people">
        {asked.items.map((a) => row(a.reviewer, a))}
        {unasked.map((who) => row(who, undefined))}
      </ul>
      {asked.more && (
        <Button
          size="small"
          variant="ghost"
          onClick={asked.next}
          disabled={asked.paging}
          loading={asked.paging}
        >
          Load more reviewers
        </Button>
      )}
      {asked.pageError && <PageFailed error={asked.pageError} />}
    </>
  );
}

/** Request at the Reviewers head: the ask dialog, focus back on the press as it closes. */
function RequestReview({ change }: { change: CodeChangeDetail }) {
  const ask = useAct("code_request_review");
  const [asking, setAsking] = useState(false);
  if (!changeLive(change.state)) return null;
  return (
    <>
      <WriteButton
        write={ask}
        size="small"
        variant="ghost"
        leadingIcon={<UserPlusGlyph size="sm" />}
        showRefusal={false}
        onPress={() => setAsking(true)}
      >
        Request
      </WriteButton>
      {asking && <AskDialog change={change} onClose={() => setAsking(false)} />}
    </>
  );
}

/** The readiness meter and what the change still waits on. */
function Readiness({ change, checks }: { change: CodeChangeDetail; checks: LatestChecks }) {
  const seatBadge = useSeatBadgeOf();
  const viewer = useViewer();
  const readiness = change.readiness!;
  const subject = { change, nameOf: (h: string) => seatBadge(h).name, me: viewer.handle };
  const waits = readinessWaits(readiness, subject, checks.rows, "Waits on");
  return (
    <>
      <SegmentedMeter
        label={`${readinessCount(readiness)}. ${waits}.`}
        total={readiness.total}
        segments={readiness.preconditions.map((p, i) => ({
          id: `${i}-${p.kind}`,
          value: 1,
          tone: preconditionTone(p),
          label: p.state,
        }))}
      />
      <p className="code-rail__note">{waits}</p>
    </>
  );
}

/** The source branch into the target, the target's protected tag from its own rules. */
function Branches({ change }: { change: CodeChangeDetail }) {
  const target = refName(change.target_ref).name;
  const found = useBranchDetail(change.repo, target);
  const protection = found.row?.protection;
  return (
    <div className="code-rail__branches">
      <p className="code-rail__row">
        <BranchChip name={refName(change.source_ref).name} />
      </p>
      <p className="code-rail__row">
        <ArrowDownGlyph size="xs" aria-hidden="true" />
        <span className="code-rail__note">into</span>
        <BranchChip name={target} />
        {protection && (
          <Tag
            size="xs"
            appearance="outline"
            leadingIcon={<ShieldCheckGlyph size="xs" />}
            title={protectionTitle(protection)}
          >
            protected
          </Tag>
        )}
      </p>
      {found.error && !found.row && !found.loading && (
        <p className="code-rail__note">
          {refusedWords(`Whether ${target} is protected is unavailable`, sentenceOf(found))}
        </p>
      )}
    </div>
  );
}

/** Each latest check's mark, name and state. */
function RailChecks({
  change,
  checks,
  kept,
}: {
  change: CodeChangeDetail;
  checks: LatestChecks;
  /** Whether its head's checks are still kept (`checksKept`). */
  kept: boolean;
}) {
  if (checks.refused !== null) {
    return (
      <p className="code-rail__note">
        {refusedWords("Its checks are unavailable", checks.refused)}
      </p>
    );
  }
  if (checks.rows === null) return <p className="code-rail__note">Reading its checks…</p>;
  if (checks.rows.length === 0) {
    const target = refName(change.target_ref).name;
    return <p className="code-rail__note">{checksWords([], target, kept).title}.</p>;
  }
  return (
    <ul className="code-rail__checks" aria-label={`Checks of ${change.ref}`}>
      {checks.rows.map((r) => (
        <li key={`${r.target}/${r.name}`}>
          <CheckMark state={r.state} />
          <strong>{r.name}</strong>
          <span className="code-rail__note">{checkStateWord(r.state)}</span>
        </li>
      ))}
    </ul>
  );
}

/** Everyone taking part, their faces in a wrapping row, each titled with where they stand. */
function Participants({ change }: { change: CodeChangeDetail }) {
  const whoName = useWhoName();
  const viewer = useViewer();
  const list = change.participants;
  if (!list) {
    return <p className="code-rail__note">Who takes part is unavailable on this fleet yet.</p>;
  }
  return (
    <p className="code-rail__faces">
      {list.rows.map((p) => {
        const name = whoName(p.handle);
        const said = p.verdict
          ? `${name}, ${verdictPhrase(p.verdict)} revision ${p.verdict_revision}`
          : name;
        return (
          <span key={p.handle} title={said}>
            <Face
              who={p.handle}
              size="sm"
              ring={p.handle === viewer.handle ? "brand" : undefined}
            />
            <VisuallyHidden>{said}</VisuallyHidden>
          </span>
        );
      })}
      {(list.omitted ?? 0) > 0 && <span className="code-rail__note">+{list.omitted}</span>}
    </p>
  );
}

/** Whether the change's newest records notified the reader, and why; the rest in More. */
function Notified({ change }: { change: CodeChangeDetail }) {
  const now = useNow();
  const viewer = useViewer();
  const read = useCodeQuery("code_change_routing", { name: change.repo, change: change.number });
  useAskAgainWhenMoved(partMovedBy(change, "reviewers"), true, read.refetch);
  const more = (said: string) => (
    <a className="t-link" href={changeHref(change.repo, change.number, { tab: "more" })}>
      {said}
    </a>
  );
  if (read.error && !read.data) {
    return (
      <p className="code-rail__note">
        {refusedWords("Whom it notified is unavailable", sentenceOf(read))}
      </p>
    );
  }
  const routed = read.data?.routed;
  if (!routed) return <p className="code-rail__note">Reading whom it notified…</p>;
  if (!viewer.handle) return more("See who was notified");
  const records = routed.records ?? [];
  if (records.length === 0 && routed.delivery === "swept") {
    return <p className="code-rail__note">Its notices were swept.</p>;
  }
  const mine = records
    .map((r) => ({ at: r.at, me: r.recipients.find((w) => w.handle === viewer.handle) }))
    .find((r) => r.me);
  return (
    <>
      <p className="code-rail__row code-rail__row--lead">
        <BellGlyph size="sm" aria-hidden="true" />
        <span className="code-rail__note">
          {mine?.me ? (
            <>
              You were notified{" "}
              <time dateTime={mine.at} title={fmtDateTime(mine.at)}>
                {ago(mine.at, now, { clock: true })}
              </time>
              : {codeReasonPhrase(mine.me.reason, "short")}
            </>
          ) : (
            "None of its newest records notified you."
          )}
        </span>
      </p>
      {more(mine?.me ? "See who else was notified" : "See who was notified")}
    </>
  );
}

export function ChangeRail({
  change,
  revisions,
  standings,
  again,
  asked,
  checks,
}: {
  change: CodeChangeDetail;
  revisions: readonly CodeRevisionRow[];
  /** Each reviewer's standing verdict (`mergeSections.ts`). */
  standings: readonly Standing[];
  /** Whom a re-request asks. */
  again: readonly string[];
  /** Whom it asked, read once by the conversation for its timeline and this rail. */
  asked: Paged<CodeReviewerRow, CodeChangeAnswer>;
  checks: LatestChecks;
}) {
  const whoName = useWhoName();
  const now = useNow();
  const work = change.work_item;
  const head = revisions.find((r) => r.n === change.revision)?.head ?? change.head;
  const people = (change.participants?.rows.length ?? 0) + (change.participants?.omitted ?? 0);
  return (
    <div className="code-rail">
      <RailSection title="Reviewers" end={<RequestReview change={change} />}>
        <Reviewers change={change} standings={standings} again={again} asked={asked} />
      </RailSection>
      {changeLive(change.state) && change.readiness && (
        <RailSection
          title="Readiness"
          end={
            <span className="code-rail__note">
              {change.readiness.met} of {change.readiness.total}
            </span>
          }
        >
          <Readiness change={change} checks={checks} />
        </RailSection>
      )}
      <RailSection
        title="Work item"
        end={
          work?.backend === "worklet" ? (
            <a className="t-link code-rail__note" href={href(itemPath(work))}>
              Worklet <ArrowUpRightGlyph size="xs" aria-hidden="true" />
            </a>
          ) : undefined
        }
      >
        {work ? (
          <a
            className="code-rail__row code-rail__name"
            href={changesHref({ work: `${work.backend}:${work.id}`, state: "all" })}
            title="Every change of the work item"
          >
            <SquareKanbanGlyph size="sm" aria-hidden="true" />
            {work.key || work.id}
          </a>
        ) : (
          <EmptyValue label="No work item" />
        )}
      </RailSection>
      <RailSection title="Owner">
        <p className="code-rail__row">
          <Face who={change.owner} />
          <a className="code-rail__name" href={changesHref({ owner: change.owner })}>
            {whoName(change.owner)}
          </a>
        </p>
      </RailSection>
      <RailSection title="Branches">
        <Branches change={change} />
      </RailSection>
      <RailSection title="Head">
        <p className="code-rail__row">
          <ShaChip oid={head} />
          <a className="t-link" href={commitHref(change.repo, head)}>
            revision {change.revision}
          </a>
        </p>
      </RailSection>
      <RailSection
        title="Checks"
        end={
          <a
            className="t-link code-rail__note"
            href={changeHref(change.repo, change.number, { tab: "checks" })}
          >
            All checks
          </a>
        }
      >
        <RailChecks change={change} checks={checks} kept={checksKept(change, revisions, now)} />
      </RailSection>
      <RailSection
        title="Participants"
        end={change.participants ? <Count value={people} label="participants" /> : undefined}
      >
        <Participants change={change} />
      </RailSection>
      <RailSection title="Reference">
        <p className="code-rail__row">
          <span className="mono">{change.ref}</span>
          <CopyButton text={change.ref} label={`Copy the reference ${change.ref}`} />
        </p>
      </RailSection>
      <RailSection title="Notified">
        <Notified change={change} />
      </RailSection>
    </div>
  );
}
