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

import { EmptyState, Skeleton, Stack, Tag } from "@crewlethq/ui";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { QueryState } from "~/components/common.tsx";
import { changeLive, parseChangeRef, preconditionWord, refName } from "~/lib/code.ts";
import { plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeChangeDetail, CodePrecondition, CodeReadiness } from "~/protocol/index.ts";
import { ChangeStateTag } from "./Changes.tsx";
import { REPO_PARAM } from "./paths.ts";

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
            path: ["code", "pushes"],
            query: { [REPO_PARAM]: change.repo },
          },
          {
            label: "Branches",
            value: (
              <span className="mono">
                {refName(change.source_ref).name} → {refName(change.target_ref).name}
              </span>
            ),
            token: true,
          },
          {
            label: "Owner",
            value: owner.name,
            // THE OWNER'S OTHER OPEN CHANGES: an owner's changes are kept by
            // state, so the listing names one.
            path: ["code"],
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
                  path: ["code"],
                  query: { work: `${work.backend}:${work.id}`, state: "all" },
                },
              ]
            : []),
        ]}
      />
      <Stack gap={3}>
        {changeLive(change.state) && change.readiness ? (
          <Readiness readiness={change.readiness} />
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
          <ul className="code-person-band__list">
            {change.authors.map((a) => (
              <li key={a.author}>
                {seatBadge(a.author).name}{" "}
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
 * A live change's readiness, as the engine judged it: how many preconditions
 * are met of how many, its first sentence, and each precondition in the
 * engine's order with where it stands and the engine's own sentence about it.
 */
export function Readiness({ readiness }: { readiness: CodeReadiness }) {
  return (
    <section aria-label="Readiness">
      <p>
        <strong>
          {readiness.met} of {plural(readiness.total, "precondition")} met
        </strong>{" "}
        — {readiness.first}
      </p>
      <ul className="code-person-band__list">
        {readiness.preconditions.map((p) => (
          <li key={p.kind}>
            <PreconditionTag precondition={p} /> {preconditionWord(p.kind)}
            <span className="muted"> — {p.detail}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * Where one precondition stands, in a word. COLOUR IS STATE: met is done and
 * unmet waits on somebody; one the engine is still computing waits on nobody,
 * and is outlined.
 */
export function PreconditionTag({ precondition }: { precondition: CodePrecondition }) {
  switch (precondition.state) {
    case "met":
      return (
        <Tag size="sm" variant="success">
          Met
        </Tag>
      );
    case "unmet":
      return (
        <Tag size="sm" variant="warning">
          Waiting
        </Tag>
      );
    case "computing":
      return (
        <Tag size="sm" appearance="outline">
          Computing
        </Tag>
      );
    default:
      return (
        <Tag size="sm" appearance="outline">
          {precondition.state}
        </Tag>
      );
  }
}
