/**
 * The fleet broker's members: what every node advertises about its broker
 * beside what the broker's metadata group counts, and the removal of a member
 * that is gone for good.
 *
 * # Two records, and the disagreement that matters
 *
 * Every node advertises its broker kind on its presence lease — derived from
 * its own `stream` block, never from its roles — and the metadata group, the
 * raft group that places every stream and consumer, counts its voters for
 * itself. A member gone for good is where they part: its presence lapses with
 * its process, and the group goes on counting it in every election and every
 * create until it is removed. The engine names each disagreement
 * (`engine.BrokerFindingKinds`), and this panel says each in words — a dead
 * member with the gesture that removes it.
 *
 * # Read through a member
 *
 * Only a member holds the group, so the node answering may have asked another
 * member for it; `group_from` names whose view this is. A group no member could
 * report is said to be unread, never drawn as an empty one — an empty table of
 * voters would read as a broker with no quorum at all.
 *
 * # Removing is typed out, and forcing is a second decision
 *
 * A removed member no longer counts in any election, which changes the quorum
 * every node runs on, so the node id is typed. A node that still holds a live
 * presence lease is refused unless forced — a running member removed from the
 * group rejoins it as a voter at its next restart — so for one this panel sees
 * live the dialog asks for that separately, and says why.
 */

import { useState } from "react";

import {
  Button,
  Callout,
  Card,
  Checkbox,
  EmptyState,
  InlineCode,
  Input,
  Modal,
  Tag,
} from "@crewlethq/ui";
import { DnsGlyph, ScheduleGlyph, WarningGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, TagsCell } from "~/app/frame/cells.tsx";
import { BROKER_REMOVE_TIMEOUT_MS, rest, RestError } from "~/protocol/index.ts";
import type { BrokerRemoved, FleetBrokerAnswer, MetaGroup, MetaPeer } from "~/protocol/index.ts";
import { fmtDuration } from "~/lib/format.ts";

/** One row of the panel: a live node, a voter no live node is, or both. */
export interface BrokerRow {
  node: string;
  /** What the node advertises, or undefined for a voter no live node is. */
  kind?: string;
  roles?: string[] | null;
  /** How the group counts it, or undefined for a node it does not. */
  voter?: MetaPeer;
}

/**
 * Every live node and every voter, once each, in id order — so a voter no
 * live node is still has a row: it is the member an operator is looking for.
 */
export function brokerRows(answer: FleetBrokerAnswer): BrokerRow[] {
  const rows = new Map<string, BrokerRow>();
  for (const n of answer.nodes ?? []) {
    rows.set(n.node, { node: n.node, kind: n.kind, roles: n.roles });
  }
  for (const p of answer.group?.peers ?? []) {
    const row = rows.get(p.name) ?? { node: p.name };
    row.voter = p;
    rows.set(p.name, row);
  }
  return [...rows.values()].sort((a, b) => a.node.localeCompare(b.node));
}

/** How the group counts a node, in the words the command line uses. */
export function voterState(voter?: MetaPeer): string {
  if (!voter) return "not a voter";
  if (voter.leader) return "leader";
  if (voter.self || voter.current) return "current";
  if (voter.offline) return `offline, last heard ${fmtDuration(voter.active / 1e6)} ago`;
  return "behind";
}

/** The group line: whose view, how many voters, and who leads — or that nobody does. */
export function groupSummary(group: MetaGroup, from?: string): string {
  const view = from ? `as ${from} reports it` : "as a member reports it";
  if (!group.leader) {
    return `Metadata group of ${group.cluster}, ${view}: no leader — it is electing one, or it has lost its quorum and can change nothing about itself`;
  }
  return `Metadata group of ${group.cluster}, ${view}: ${group.peers.length} voters, led by ${group.leader}`;
}

/** What each finding kind is called on this screen. */
const FINDING_TITLE: Record<string, string> = {
  dead_member: "Dead member",
  not_in_group: "Not counted",
  unknown_kind: "Unknown broker",
};

export function BrokerMembership({
  answer,
  error,
  onChanged,
}: {
  answer?: FleetBrokerAnswer;
  /** The last poll's failure, when there was one. */
  error?: string | null;
  /** Called after a removal, so the screen re-reads the group. */
  onChanged?: () => void;
}) {
  const [removing, setRemoving] = useState<string | null>(null);
  if (!answer) {
    return error ? (
      <Card>
        <Card.Header icon={<DnsGlyph size="sm" />}>Broker members</Card.Header>
        <Callout variant="warning" role="alert">
          <span>The broker&apos;s membership could not be read ({error}).</span>
        </Callout>
      </Card>
    ) : null;
  }
  if (answer.external) {
    return (
      <Card>
        <Card.Header icon={<DnsGlyph size="sm" />}>Broker members</Card.Header>
        <EmptyState
          size="compact"
          icon={<DnsGlyph size="xl" />}
          title="An external NATS cluster"
          description="This fleet's broker is somebody else's cluster (stream.type: nats). Its membership belongs to whoever runs it, and nothing here lists or changes it."
        />
      </Card>
    );
  }
  const rows = brokerRows(answer);
  const live = new Set((answer.nodes ?? []).map((n) => n.node));
  return (
    <Card padding="none">
      <Card.Header
        icon={<DnsGlyph size="sm" />}
        count={answer.group?.peers.length ?? 0}
        subtitle={answer.group ? groupSummary(answer.group, answer.group_from) : undefined}
      >
        Broker members
      </Card.Header>
      {(!answer.group || answer.findings.length > 0 || error) && (
        <Card.Body padding="md">
          <div className="col gap-2">
            {error && (
              <Callout variant="danger" role="alert">
                <span>
                  This poll failed ({error}). What is below is the last reading that succeeded.
                </span>
              </Callout>
            )}
            {!answer.group && (
              <Callout variant="warning" role="alert">
                <span>
                  The metadata group could not be read: {answer.group_error}. The rows below are
                  only what the nodes advertise.
                </span>
              </Callout>
            )}
            {answer.findings.map((f) => (
              <Callout
                key={`${f.kind}:${f.node}`}
                variant={f.kind === "dead_member" ? "warning" : "info"}
                role={f.kind === "dead_member" ? "alert" : "status"}
                icon={<WarningGlyph size="md" />}
              >
                <span className="row wrap gap-2 baseline">
                  <span>
                    <strong>{FINDING_TITLE[f.kind] ?? f.kind}</strong>{" "}
                    <InlineCode>{f.node}</InlineCode> — {f.detail}.
                  </span>
                  {f.kind === "dead_member" && (
                    <Button variant="tertiary" size="small" onClick={() => setRemoving(f.node)}>
                      Remove from the group
                    </Button>
                  )}
                </span>
              </Callout>
            ))}
          </div>
        </Card.Body>
      )}
      <DataGrid<BrokerRow>
        name="broker-members"
        rows={rows}
        rowKey={(r) => r.node}
        defaultSort="node"
        empty={{ title: "No node advertises a broker, and no voter was read" }}
        columns={[
          {
            key: "node",
            header: "Node",
            sortValue: (r) => r.node,
            cell: (r) =>
              live.has(r.node) ? (
                <KeyCell value={r.node} path={["admin", "fleet", r.node]} />
              ) : (
                <KeyCell value={r.node} />
              ),
          },
          {
            key: "kind",
            header: "Advertises",
            shrink: true,
            sortValue: (r) => r.kind ?? "",
            cell: (r) =>
              r.kind ? (
                <Tag variant={r.kind === "unknown" ? "warning" : "neutral"}>{r.kind}</Tag>
              ) : (
                <span className="t-caption">no live node</span>
              ),
          },
          {
            key: "roles",
            header: "Roles",
            sortValue: (r) => (r.roles ?? []).join(","),
            cell: (r) => <TagsCell tags={r.roles} />,
          },
          {
            key: "voter",
            header: "Voter",
            sortValue: (r) => voterState(r.voter),
            cell: (r) => (
              <Tag
                variant={
                  !r.voter
                    ? "neutral"
                    : r.voter.leader || r.voter.current || r.voter.self
                      ? "success"
                      : "warning"
                }
              >
                {voterState(r.voter)}
              </Tag>
            ),
          },
        ]}
      />
      {removing && (
        <BrokerRemoveDialog
          node={removing}
          live={live.has(removing)}
          onDone={() => onChanged?.()}
          onClose={() => setRemoving(null)}
        />
      )}
    </Card>
  );
}

/** What the last request came back with. */
type Heard =
  | { kind: "removed"; answer: BrokerRemoved }
  | { kind: "refused"; detail: string; hint: string }
  | { kind: "unanswered"; why: string };

/** Sends one removal and reads back what it did. */
async function remove(node: string, force: boolean): Promise<Heard> {
  const query: Record<string, string> = { confirm: node };
  if (force) query.force = "true";
  try {
    const answer = (
      await rest.request("POST", `/fleet/broker/remove/${encodeURIComponent(node)}`, {
        body: {},
        query,
        timeoutMs: BROKER_REMOVE_TIMEOUT_MS,
      })
    ).body as BrokerRemoved | null;
    if (!answer) return { kind: "unanswered", why: "the node answered with no body" };
    return { kind: "removed", answer };
  } catch (err) {
    if (err instanceof RestError && err.unanswered) {
      return {
        kind: "unanswered",
        why:
          err.status === 0 || err.code === "unreadable_body"
            ? err.message
            : `a ${err.status} came back with no engine error code in it — something in front of the node answered`,
      };
    }
    if (err instanceof RestError) return { kind: "refused", detail: err.message, hint: err.hint };
    return { kind: "refused", detail: String(err), hint: "" };
  }
}

/**
 * Removing a member from the metadata group.
 *
 * `live` is whether this screen saw the node holding a presence lease: the
 * engine refuses such a node unless forced, so the dialog asks for that too
 * rather than sending a request it knows will be refused.
 */
export function BrokerRemoveDialog({
  node,
  live,
  onDone,
  onClose,
}: {
  node: string;
  live: boolean;
  onDone: () => void;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [forcing, setForcing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [heard, setHeard] = useState<Heard | null>(null);
  const confirmed = typed.trim() === node && (!live || forcing);
  const done = heard?.kind === "removed";

  async function send() {
    if (busy || !confirmed) return;
    setBusy(true);
    const next = await remove(node, live && forcing);
    setHeard(next);
    setBusy(false);
    onDone();
  }

  return (
    <Modal
      open
      title={`Remove ${node} from the broker`}
      icon={<DnsGlyph size="md" />}
      onClose={onClose}
      // A REQUEST IN FLIGHT HAS AN OUTCOME NOBODY HAS HEARD YET.
      dismissable={!busy}
      closeDisabledReason="Waiting for the metadata group to commit the change."
      size="md"
      stackBody
      footer={
        <>
          <Button variant="tertiary" onClick={onClose} disabled={busy}>
            {done ? "Close" : "Cancel"}
          </Button>
          {!done && (
            <Button variant="danger" onClick={() => void send()} disabled={busy || !confirmed}>
              {busy ? "Waiting for the commit" : heard ? "Send again" : "Remove"}
            </Button>
          )}
        </>
      }
    >
      {!done && (
        <>
          <p className="t-body secondary" style={{ margin: 0 }}>
            The metadata group stops counting <InlineCode>{node}</InlineCode> in its elections and
            its creates — the gesture for a member that is gone for good. A process that restarts
            under that name joins the group again as a new voter.
          </p>
          <label className="col" style={{ gap: 6 }}>
            <span className="t-caption">
              Type <InlineCode>{node}</InlineCode> to confirm
            </span>
            <Input
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoFocus
              spellCheck={false}
              disabled={busy}
              aria-label={`Type ${node} to confirm`}
            />
          </label>
          {live && (
            <Checkbox
              framed
              tone="danger"
              checked={forcing}
              disabled={busy}
              onCheckedChange={setForcing}
              label="Remove it although it is running"
              description={
                <>
                  <InlineCode>{node}</InlineCode> still holds a live presence lease, so it is
                  running, and a running member removed from the group rejoins it at its next
                  restart. Stop it first — force this only for a member wedged in a way that still
                  renews its lease.
                </>
              }
            />
          )}
        </>
      )}
      {heard?.kind === "removed" && (
        <Callout variant="success" role="status">
          <span className="col" style={{ gap: 6 }}>
            <span>
              <InlineCode>{heard.answer.node}</InlineCode> is no longer a voter, removed through{" "}
              <InlineCode>{heard.answer.by}</InlineCode>&apos;s system account.
            </span>
            {heard.answer.group && (
              <span className="t-caption">
                The group counts {heard.answer.group.peers.map((p) => p.name).join(", ")}.
              </span>
            )}
          </span>
        </Callout>
      )}
      {heard?.kind === "refused" && (
        <Callout variant="danger" role="alert">
          <span className="col" style={{ gap: 6 }}>
            <span>{heard.detail}</span>
            {heard.hint && <span className="t-caption">{heard.hint}</span>}
          </span>
        </Callout>
      )}
      {heard?.kind === "unanswered" && (
        <Callout variant="warning" role="alert" icon={<ScheduleGlyph size="md" />}>
          <span>
            <strong>No answer.</strong> {heard.why}, so whether the removal was committed is
            unknown. The panel behind this re-reads the group; sending it again for a member already
            removed is refused as not a member.
          </span>
        </Callout>
      )}
    </Modal>
  );
}
