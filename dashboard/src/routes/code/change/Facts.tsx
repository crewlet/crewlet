/**
 * What a change is made of and where it came from: its revisions, its
 * provenance, and whom its records woke.
 *
 * WHOM IT WOKE is the engine's own account (`code_change_routing`): each
 * record about the change that reached anybody, newest first, with every
 * recipient it reached and the ONE reason that recipient heard it under, said
 * in the voice of somebody else's notice (`CODE_PHRASES`' `about`) — and, where
 * no record is listed, whether that is because nobody was told or because the
 * notices were swept with the turn detail they named.
 */

import type { ReactNode } from "react";
import { EmptyValue, Stack, Tag } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { codeReasonPhrase, refName, shortOID } from "~/lib/code.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeChangeDetail, CodeRevisionRow } from "~/protocol/index.ts";
import { CHANGE_POLL_MS } from "./reads.ts";

/** The change's revisions, newest first, each with the facts its home recorded. */
export function Commits({
  change,
  revisions,
}: {
  change: CodeChangeDetail;
  revisions: readonly CodeRevisionRow[];
}) {
  const seatBadge = useSeatBadgeOf();
  if (revisions.length === 0) return <p className="muted">No revision is recorded yet.</p>;
  return (
    <ol className="code-timeline" aria-label="Revisions">
      {revisions.map((r) => (
        <li key={r.n} className="code-entry">
          <p>
            <strong>Revision {r.n}</strong> <span className="mono">{shortOID(r.head)}</span>{" "}
            {r.n === change.revision && <Tag appearance="outline">head</Tag>} pushed by{" "}
            {seatBadge(r.pusher).name} <span className="muted">{fmtDateTime(r.at)}</span>
          </p>
          {r.facts ? (
            <p className="muted">
              Against {refName(r.facts.target_ref).name} at{" "}
              <span className="mono">{shortOID(r.facts.target_oid)}</span>
              {r.facts.base_oid && (
                <>
                  , from <span className="mono">{shortOID(r.facts.base_oid)}</span>
                </>
              )}
              :{" "}
              {r.facts.touches_all
                ? "touches more files than its facts list"
                : `${r.facts.touched.length} file${r.facts.touched.length === 1 ? "" : "s"} touched`}
              {r.facts.patch_id_omitted && ", too large to identify its diff"}
            </p>
          ) : (
            <p className="muted">Its home has not measured it yet.</p>
          )}
        </li>
      ))}
    </ol>
  );
}

/** Where the change came from: who opened it, from which turn and run, for which work, and how it ended. */
export function Provenance({ change }: { change: CodeChangeDetail }) {
  const seatBadge = useSeatBadgeOf();
  const work = change.work_item;
  const rows: { label: string; value: ReactNode }[] = [
    {
      label: "Opened by",
      value: (
        <>
          {seatBadge(change.owner).name}
          {change.owner_operator_id && (
            <span className="muted mono"> via {change.owner_operator_id}</span>
          )}
        </>
      ),
    },
    { label: "Opened", value: fmtDateTime(change.opened_at) },
    {
      label: "Turn",
      value: change.created_turn ? (
        <a className="mono" href={href(["live", "turns", change.created_turn])}>
          {change.created_turn}
        </a>
      ) : (
        <EmptyValue label="Opened by a person" />
      ),
    },
    {
      label: "Coding run",
      value: change.created_run ? (
        <a className="mono" href={href(["live", "runs", change.created_run])}>
          {change.created_run}
        </a>
      ) : (
        <EmptyValue label="None" />
      ),
    },
    {
      label: "Work item",
      value: work ? (
        <span className="mono">
          {work.key || work.id} <span className="muted">({work.backend})</span>
        </span>
      ) : (
        <EmptyValue label="None" />
      ),
    },
    {
      label: "Authors",
      value: change.authors.map((a) => seatBadge(a.author).name).join(", "),
    },
    ...(change.state === "merged"
      ? [
          {
            label: "Merged",
            value: (
              <>
                {change.merged_at && fmtDateTime(change.merged_at)}{" "}
                {change.merged_via === "push" ? "by a push" : "through its gate"}
                {change.merged_by && ` by ${seatBadge(change.merged_by).name}`}
                {change.merge_oid && (
                  <>
                    {" "}
                    at <span className="mono">{shortOID(change.merge_oid)}</span>
                  </>
                )}
                {change.override_reason && ` — overriding its gate: ${change.override_reason}`}
              </>
            ),
          },
        ]
      : []),
    ...(change.state === "closed"
      ? [
          {
            label: "Closed",
            value: `${change.closed_at ? fmtDateTime(change.closed_at) : ""}${
              change.close_reason ? ` — ${change.close_reason}` : ""
            }`,
          },
        ]
      : []),
  ];
  return (
    <dl className="code-facts">
      {rows.map((row) => (
        <div key={row.label}>
          <dt>{row.label}</dt>
          <dd>{row.value}</dd>
        </div>
      ))}
    </dl>
  );
}

/** Whom the change's records woke, newest first, each recipient with why. */
export function Woke({ change }: { change: CodeChangeDetail }) {
  const seatBadge = useSeatBadgeOf();
  const read = useCodeQuery(
    "code_change_routing",
    { name: change.repo, change: change.number },
    { pollMs: CHANGE_POLL_MS },
  );
  const routed = read.data?.routed;
  const records = routed?.records ?? [];
  return (
    <QueryState
      error={read.error}
      refusal={read.refusal}
      detail={read.detail}
      loading={read.loading && !read.data}
      empty={
        routed && records.length === 0
          ? {
              title: routed.delivery === "swept" ? "Its notices were swept" : "Nobody was told",
              hint:
                routed.delivery === "swept"
                  ? "A seat's notice is kept as long as the turn detail it names; this change's are older."
                  : "No record about this change reached anybody.",
            }
          : undefined
      }
    >
      <Stack gap={3}>
        <ol className="code-timeline" aria-label="Whom it woke">
          {records.map((r) => (
            <li key={r.record_id} className="code-entry">
              <p className="muted">{fmtDateTime(r.at)}</p>
              {r.excerpt && <p>{r.excerpt}</p>}
              <ul className="code-person-band__list">
                {r.recipients.map((who) => (
                  <li key={who.handle}>
                    {seatBadge(who.handle).name}{" "}
                    <span className="muted">— {codeReasonPhrase(who.reason, "about")}</span>
                    {who.addressed && (
                      <Tag size="sm" appearance="outline">
                        asked
                      </Tag>
                    )}
                    {who.fallback && (
                      <Tag size="sm" appearance="outline">
                        nobody nearer
                      </Tag>
                    )}
                  </li>
                ))}
              </ul>
            </li>
          ))}
        </ol>
        {read.data?.next && <p className="muted">Older records are past this page.</p>}
      </Stack>
    </QueryState>
  );
}
