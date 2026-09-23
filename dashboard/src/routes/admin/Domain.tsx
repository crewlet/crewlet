/**
 * One state-log domain, as a page.
 *
 * # Why a domain has an address at all
 *
 * A domain is the unit the trim actually operates on. The fleet card draws
 * all of them stacked, which is the right shape for "is anything stuck" and
 * the wrong one for the question that follows it — *why is THIS one stuck,
 * and which node is pinning it*. That second question is answered by facts
 * scattered across two cards and one table: the domain's own six terms live
 * in the retention card, and every node's position for it lives folded into a
 * per-node column that shows every domain at once. A reader chasing one
 * domain had to read the whole fleet to find its rows.
 *
 * `domains` was reserved as a path segment before anything routed it. This is
 * the route it was reserved for.
 *
 * # It asks nothing new
 *
 * The whole page is the `retention` document the panels beside it already
 * read, picked apart by domain — the same move `NodeScreen` makes over
 * `fleet`. A second query for a subset of one answer would be a second thing
 * to keep in step, and the two would disagree the first time a trim tick
 * landed between them.
 *
 * # What it can and cannot say
 *
 * A domain absent from the document is NOT a domain at position zero: a node
 * that keeps no state log serves a report with no domains at all, and drawing
 * zeros over that would be a confident claim about a subsystem this node does
 * not run. It says which, and the two read differently.
 */

import { useMemo } from "react";
import { Card, EmptyValue, InlineCode, Skeleton, Tag } from "@crewlethq/ui";
import { DnsGlyph, LayersGlyph } from "@crewlethq/icons/glyphs";

import { href } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { TextCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtBytes } from "~/lib/format.ts";
import { DomainBlock, Terms } from "./Retention.tsx";
import type { RetentionNode } from "~/protocol/index.ts";

/** How often the retention document is re-read, matching the fleet's own. */
const POLL_MS = 30_000;

/** One node's position in THIS domain, flattened out of the per-node map. */
interface Position {
  node_id: string;
  counted: boolean;
  live: boolean;
  seq: number;
  applied_through: number;
  lag?: number;
  generation: number;
}

export function DomainScreen({ name }: { name: string }) {
  const { data, loading, error } = useQuery("retention", undefined, {
    enabled: name !== "",
    pollMs: POLL_MS,
  });

  const domain = data?.domains?.find((d) => d.domain === name);

  // EVERY NODE'S POSITION IN THIS ONE DOMAIN. The fleet's own table has this
  // folded into a cell that lists every domain a node reports, so reading one
  // domain's rows meant reading every node's cell and filtering by eye.
  const positions = useMemo<Position[]>(() => {
    const nodes: RetentionNode[] = data?.nodes ?? [];
    const out: Position[] = [];
    for (const node of nodes) {
      const at = node.domains?.[name];
      // A NODE THAT HAS PUBLISHED NOTHING FOR THIS DOMAIN IS NOT AT ZERO, and
      // it is the one the trim is most likely waiting on — the minimum is
      // taken across these rows, so a node it cannot see is a node it cannot
      // trim past. It is kept as a row with no position rather than dropped.
      if (!at) {
        out.push({
          node_id: node.node_id,
          counted: node.counted,
          live: node.live,
          seq: -1,
          applied_through: -1,
          generation: 0,
        });
        continue;
      }
      out.push({
        node_id: node.node_id,
        counted: node.counted,
        live: node.live,
        seq: at.seq,
        applied_through: at.applied_through,
        lag: at.lag,
        generation: at.generation,
      });
    }
    return out;
  }, [data, name]);

  const facts: Fact[] = domain
    ? [
        { label: "Replay", value: domain.replay },
        { label: "Generation", value: String(domain.generation) },
        {
          label: "Sequence",
          value: (
            <span className="t-num">
              {domain.first_seq}…{domain.last_seq}
            </span>
          ),
        },
        {
          label: "Floor",
          // THE FLOOR AND WHAT THIS TICK CONCLUDED, which are two facts: the
          // floor is what the six terms permit and the conclusion is where the
          // trim actually got to. They are equal on a healthy domain and the
          // gap between them is the whole story on a stuck one.
          value: (
            <span className="t-num">
              {domain.trim_floor}
              {domain.trim_to !== domain.trim_floor && (
                <span className="muted"> · concluded {domain.trim_to}</span>
              )}
            </span>
          ),
        },
        {
          label: "Size",
          value: (
            <span className="t-num">
              {fmtBytes(domain.bytes)}
              {domain.headroom_fraction != null && (
                <span className="muted"> · {Math.round(domain.headroom_fraction * 100)}% free</span>
              )}
              {/* Absent is unmeasured, never an idle log — see RetentionDomain. */}
              {domain.bytes_per_day != null && (
                <span className="muted"> · {fmtBytes(domain.bytes_per_day)} a day</span>
              )}
            </span>
          ),
        },
      ]
    : [];

  return (
    <>
      <PageActions>
        <a className="t-link" href={href(["admin", "fleet"])}>
          Infrastructure →
        </a>
      </PageActions>

      {loading && !data && <Skeleton variant="text" rows={6} label="Loading the domain" />}
      <QueryState error={error} loading={loading}>
        {data &&
          (domain ? (
            <>
              <ObjectHeader
                kind="Domain"
                icon="layers"
                title={domain.domain}
                status={
                  domain.blocked_by ? (
                    <Tag variant="warning">{domain.blocked_by}</Tag>
                  ) : (
                    <Tag variant="success">advancing</Tag>
                  )
                }
                facts={facts}
              />

              <Card>
                <Card.Header icon={<LayersGlyph size="sm" />}>
                  <Card.Title>What the trim may remove</Card.Title>
                </Card.Header>
                {/* THE SIX TERMS, each with its own sentence and its own
                    remedy — the same block the fleet card draws, so the card
                    and the page can never disagree about whether this log is
                    advancing. */}
                <Terms terms={domain.terms} snapshotBlocked={domain.snapshot_blocked_by} />
              </Card>

              <Card padding="none">
                <Card.Header
                  icon={<DnsGlyph size="sm" />}
                  count={positions.length}
                  subtitle="The trim takes a minimum across these, so the lowest row is what the floor is waiting on."
                >
                  <Card.Title>Where each node is</Card.Title>
                </Card.Header>
                <DataGrid
                  rows={positions}
                  rowKey={(p) => p.node_id}
                  rowHref={(p) => href(["admin", "fleet", p.node_id])}
                  defaultSort="position"
                  columns={[
                    {
                      key: "node",
                      header: "Node",
                      cell: (p) => <TextCell icon="dns">{p.node_id}</TextCell>,
                      sortValue: (p) => p.node_id,
                    },
                    {
                      key: "position",
                      header: "Position",
                      shrink: true,
                      sortValue: (p) => p.seq,
                      cell: (p) =>
                        p.seq < 0 ? (
                          // COUNTED AND SILENT, which is the state that pins a
                          // log: the trim waits for a node it cannot see.
                          <EmptyValue label="This node has published no position for this domain" />
                        ) : (
                          <span className="t-num">
                            {p.seq}
                            {p.applied_through !== p.seq && (
                              <span className="muted"> · applied {p.applied_through}</span>
                            )}
                          </span>
                        ),
                    },
                    {
                      key: "behind",
                      header: "Behind",
                      shrink: true,
                      sortValue: (p) => p.lag ?? 0,
                      cell: (p) =>
                        p.lag == null ? (
                          <EmptyValue label="Not reported" />
                        ) : (
                          <span className="t-num">{p.lag}</span>
                        ),
                    },
                    {
                      key: "state",
                      header: "",
                      label: "State",
                      shrink: true,
                      cell: (p) => (
                        <span className="row gap-1">
                          {/* COUNTED AND LIVE ARE INDEPENDENT, which the
                              fleet's own rows say too: counted-and-not-live is
                              the node pinning the log, and live-and-not-counted
                              is one inside its eviction fence window. */}
                          {p.counted && <Tag appearance="outline">counted</Tag>}
                          {p.live && <Tag variant="success">live</Tag>}
                        </span>
                      ),
                    },
                  ]}
                />
              </Card>
            </>
          ) : (
            // ABSENT IS NOT EMPTY. A node that keeps no state log serves a
            // report with no domains at all, and a page of zeros over that
            // would be a confident claim about a subsystem this node does not
            // run.
            <Card>
              <Card.Header icon={<LayersGlyph size="sm" />}>
                <Card.Title>No such domain</Card.Title>
              </Card.Header>
              <p className="t-body">
                This node&apos;s retention report names no domain <InlineCode>{name}</InlineCode>.
                The domains it does report are{" "}
                {(data.domains ?? []).length === 0 ? (
                  <>none at all, which is what a node running no state log looks like.</>
                ) : (
                  <>
                    {(data.domains ?? []).map((d, i) => (
                      <span key={d.domain}>
                        {i > 0 && ", "}
                        <a
                          className="t-link prose-link"
                          href={href(["admin", "fleet", "domains", d.domain])}
                        >
                          <InlineCode>{d.domain}</InlineCode>
                        </a>
                      </span>
                    ))}
                    .
                  </>
                )}
              </p>
            </Card>
          ))}
      </QueryState>
    </>
  );
}

/** Re-exported so the fleet card and this page are one import away. */
export { DomainBlock };
