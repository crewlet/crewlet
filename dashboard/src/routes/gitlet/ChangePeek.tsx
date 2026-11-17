/**
 * One change in the frame's peek, by its reference (`peek=change:web!42`):
 * what it is, where it stands, and what its merge waits on.
 *
 * A PEEK ASKS FOR ITS OWN OBJECT (`app/frame/peeks.tsx`): the change's detail
 * from `code_change` — its row, its authors, its conversation's counts and its
 * readiness, which the engine judges again on this read, against its own
 * chart — never the listing row that opened it, which was judged on the
 * listing's read.
 */

import { EmptyState, Skeleton, Stack, VisuallyHidden } from "@crewlethq/ui";
import { CircleAlertGlyph, CircleCheckGlyph, CircleDashedGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { QueryState } from "~/components/common.tsx";
import { changeLive, parseChangeRef, preconditionWord, refName, repoPath } from "~/lib/code.ts";
import { plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeChangeDetail, CodePrecondition, CodeReadiness } from "~/protocol/index.ts";
import { ChangeStateTag } from "./ChangeRow.tsx";
import { preconditionSentence, readinessCount, readinessNext } from "./readinessWords.ts";
import { BranchChip, Face } from "./ui.tsx";
import "~/styles/gitlet.css";

/**
 * How often an open peek reads its change again: a MINUTE, the listing's own
 * cadence, so the peek and the row beside it move together.
 */
const PEEK_POLL_MS = 60_000;

export function ChangePeek({ reference }: { reference: string }) {
  const named = parseChangeRef(reference);
  const read = useCodeQuery(
    "code_change",
    named ? { name: named.repo, change: named.number } : {},
    { enabled: named !== null, pollMs: PEEK_POLL_MS },
  );
  if (!named) {
    return (
      <EmptyState
        size="compact"
        title="Not a change"
        description={`“${reference}” names no change: a change is its repository's name and its number, as web!42.`}
      />
    );
  }
  const change = read.data?.change;
  if (read.loading && !change) return <Skeleton variant="text" rows={6} label="Loading" />;
  if (!change) {
    return (
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return <ChangeReading change={change} />;
}

/** The change's header, its readiness, and who wrote it. */
function ChangeReading({ change }: { change: CodeChangeDetail }) {
  const seatBadge = useSeatBadgeOf();
  const owner = seatBadge(change.owner);
  const work = change.work_item;
  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Change"
        icon="diff"
        identifier={change.ref}
        title={change.title}
        status={<ChangeStateTag state={change.state} />}
        facts={[
          {
            label: "Repository",
            value: change.repo,
            path: repoPath(change.repo),
          },
          {
            label: "Branches",
            value: (
              <>
                <BranchChip name={refName(change.source_ref).name} />
                {" → "}
                <BranchChip name={refName(change.target_ref).name} />
              </>
            ),
            set: true,
          },
          {
            label: "Owner",
            value: (
              <>
                <Face who={change.owner} size="text" /> {owner.name}
              </>
            ),
            // THE OWNER'S OTHER OPEN CHANGES: an owner's changes are kept by
            // state, so the listing names one.
            path: ["gitlet"],
            query: { owner: change.owner },
          },
          { label: "Revisions", value: String(change.revision) },
          ...(work
            ? [
                {
                  label: "Work item",
                  value: work.key || work.id,
                  // EVERY CHANGE OF THE ITEM, whatever its state: a work
                  // item's changes are their own index.
                  path: ["gitlet"],
                  query: { work: `${work.backend}:${work.id}`, state: "all" },
                },
              ]
            : []),
        ]}
      />
      <Stack gap={3}>
        {changeLive(change.state) && change.readiness ? (
          <Readiness change={change} readiness={change.readiness} />
        ) : (
          <p className="muted">{endedSentence(change)}</p>
        )}
        {change.conversation && (
          <p className="muted">
            {plural(change.conversation.reviews, "review")},{" "}
            {plural(change.conversation.comments, "comment")},{" "}
            {plural(change.conversation.open_findings, "open finding")}
          </p>
        )}
        <div>
          <strong>Authors</strong>
          <ul className="code-change-authors">
            {change.authors.map((a) => (
              <li key={a.author}>
                <Face who={a.author} size="text" /> {seatBadge(a.author).name}{" "}
                <span className="muted">from revision {a.first_revision}</span>
              </li>
            ))}
          </ul>
        </div>
      </Stack>
    </>
  );
}

/** What a change that is no longer live came to. */
function endedSentence(change: CodeChangeDetail): string {
  if (change.state === "merged") {
    return change.merged_via === "push"
      ? "Merged: its target came to hold its head by a push."
      : "Merged through its gate.";
  }
  return change.close_reason ? `Closed: ${change.close_reason}` : "Closed without being merged.";
}

/**
 * A live change's readiness, as a code host's merge box lists its checks: how
 * many conditions to merge are met and what the reader acts on next, then
 * each condition — a glyph for where it stands, its word, and the sentence of
 * what it waits on — worded from what the engine judged
 * (`readinessWords.ts`). The engine's own sentence, a model's words, is each
 * line's hover text for the facts it carries.
 */
export function Readiness({
  change,
  readiness,
  lead = true,
}: {
  change: CodeChangeDetail;
  readiness: CodeReadiness;
  /** Whether the count and the next step lead the list: off where a box's head says them. */
  lead?: boolean;
}) {
  const seatBadge = useSeatBadgeOf();
  const subject = { change, nameOf: (h: string) => seatBadge(h).name };
  return (
    <section aria-label="Readiness" className="code-checks-list">
      {lead && (
        <p className="code-checks-list__lead">
          <strong>{readinessCount(readiness)}</strong>. {readinessNext(readiness, subject)}
        </p>
      )}
      <ul className="code-checklist">
        {readiness.preconditions.map((p) => {
          const said = preconditionSentence(p, subject);
          return (
            <li key={p.kind} title={p.detail} className="code-checklist__item">
              <PreconditionGlyph precondition={p} />
              <span className="code-checklist__what">
                <span className="code-checklist__word">{preconditionWord(p.kind)}</span>
                {said && <span className="code-checklist__said">{said}</span>}
              </span>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

/**
 * Where one precondition stands: a glyph and, to a screen reader, its word.
 * COLOUR IS STATE: met is done, and unmet waits on somebody; one the engine is
 * still computing waits on nobody, and takes no hue. The SHAPE says it too —
 * a check, an alert, a dashed ring — for a reader who cannot tell the inks
 * apart.
 */
export function PreconditionGlyph({ precondition }: { precondition: CodePrecondition }) {
  switch (precondition.state) {
    case "met":
      return (
        <span className="code-checklist__glyph code-checklist__glyph--met">
          <CircleCheckGlyph size="sm" />
          <VisuallyHidden>Met:</VisuallyHidden>
        </span>
      );
    case "unmet":
      return (
        <span className="code-checklist__glyph code-checklist__glyph--unmet">
          <CircleAlertGlyph size="sm" />
          <VisuallyHidden>Waiting:</VisuallyHidden>
        </span>
      );
    default:
      return (
        <span className="code-checklist__glyph">
          <CircleDashedGlyph size="sm" />
          <VisuallyHidden>
            {precondition.state === "computing" ? "Computing:" : `${precondition.state}:`}
          </VisuallyHidden>
        </span>
      );
  }
}
