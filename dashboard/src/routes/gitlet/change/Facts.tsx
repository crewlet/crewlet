/**
 * What a change is made of and where it came from: its revisions, its
 * provenance, and whom its records notified.
 *
 * WHOM IT WOKE is the engine's own account (`code_change_routing`): each
 * record about the change that reached anybody, newest first, with every
 * recipient it reached and the ONE reason that recipient heard it under, said
 * in the voice of somebody else's notice (`CODE_PHRASES`' `about`) — and, where
 * no record is listed, whether that is because nobody was told or because the
 * notices were swept with the turn detail they named.
 */

import type { ReactNode } from "react";
import { Card, EmptyValue, Stack, Tag } from "@crewlethq/ui";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { codeReasonPhrase, commitPath, refName, shortOID } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeChangeDetail, CodeRevisionRow } from "~/protocol/index.ts";
import { changeHref, type ChangeAt } from "../paths.ts";
import { Byline, Face, ShaChip } from "../ui.tsx";
import { TimelineCard, TimelineList } from "./Timeline.tsx";
import { CHANGE_POLL_MS } from "./reads.ts";
import "~/styles/gitlet.css";

/**
 * The change's revisions, newest first, each a card on the rail: who pushed it
 * and when, its head with its copy, the facts its home recorded, and the way
 * to its files and to what it changed since the revision before it.
 */
export function Revisions({
  change,
  revisions,
}: {
  change: CodeChangeDetail;
  revisions: readonly CodeRevisionRow[];
}) {
  const now = useNow();
  if (revisions.length === 0) return <p className="muted">No revision is recorded yet.</p>;
  const files = (at: Omit<ChangeAt, "tab">) =>
    changeHref(change.repo, change.number, { tab: "files", ...at });
  return (
    <TimelineList label="Revisions">
      {revisions.map((r) => (
        <TimelineCard
          key={r.n}
          label={`Revision ${r.n}`}
          head={<Byline who={r.pusher} verb={`pushed revision ${r.n}`} at={r.at} now={now} />}
          marks={
            <>
              {r.n === change.revision && <Tag appearance="outline">head</Tag>}
              <ShaChip oid={r.head} />
            </>
          }
          foot={
            <span className="row gap-3 wrap">
              <a className="t-link" href={href(commitPath(change.repo, r.head))}>
                Its head commit →
              </a>
              <a className="t-link" href={files(r.n === change.revision ? {} : { rev: r.n })}>
                Its files →
              </a>
              {r.n > 1 && (
                <a className="t-link" href={files({ rev: r.n, since: r.n - 1 })}>
                  What it changed since revision {r.n - 1} →
                </a>
              )}
            </span>
          }
        >
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
        </TimelineCard>
      ))}
    </TimelineList>
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
      // THE EXECUTORS BOARD IS KEYED BY THE TURN that launched the executor,
      // so the launch's own job id is shown and the turn is what it links.
      label: "Executor",
      value:
        change.created_run && change.created_turn ? (
          <a className="mono" href={href(["live", "executors", change.created_turn])}>
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
                {change.override_reason && `, overriding its gate: ${change.override_reason}`}
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
              change.close_reason ? ` · ${change.close_reason}` : ""
            }`,
          },
        ]
      : []),
  ];
  return (
    <Card className="code-provenance">
      <PropertiesRail
        headings="sentence"
        groups={[{ properties: rows.map((row) => ({ label: row.label, value: row.value })) }]}
      />
    </Card>
  );
}

/** Whom the change's records notified, newest first, each recipient with why. */
export function Notified({ change }: { change: CodeChangeDetail }) {
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
        <TimelineList label="Whom it notified">
          {records.map((r) => (
            <TimelineCard
              key={r.record_id}
              label={`Notified ${fmtDateTime(r.at)}`}
              head={
                <time className="code-byline__when" dateTime={r.at} title={fmtDateTime(r.at)}>
                  {fmtDateTime(r.at)}
                </time>
              }
            >
              {r.excerpt && <p>{r.excerpt}</p>}
              <ul className="code-rail__people" aria-label="Who heard">
                {r.recipients.map((who) => (
                  <li key={who.handle}>
                    <Face who={who.handle} />
                    <span className="code-rail__person">
                      <span className="code-rail__name">{seatBadge(who.handle).name}</span>
                      <span className="code-rail__note">
                        {codeReasonPhrase(who.reason, "about")}
                      </span>
                    </span>
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
            </TimelineCard>
          ))}
        </TimelineList>
        {read.data?.next && <p className="muted">Older records are past this page.</p>}
      </Stack>
    </QueryState>
  );
}
