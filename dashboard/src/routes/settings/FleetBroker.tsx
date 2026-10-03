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
 * # Read through a member, matched by peer id
 *
 * Only a member holds the group, so the node answering may have asked another
 * member for it; `group_from` names whose view this is. A group no member could
 * report is said to be unread, never drawn as an empty one — an empty table of
 * voters would read as a broker with no quorum at all.
 *
 * The group counts each voter by its raft PEER ID, and a voter's name is only
 * what the answering member has heard — a member that restarted after another
 * died never hears its name. So a voter is matched to its node by the peer id
 * each node row carries, and one nobody can name is a row of its own, by that
 * id.
 *
 * # Removing is typed out, and forcing is a second decision
 *
 * A removed member no longer counts in any election, which changes the quorum
 * every node runs on, so the node id — or the peer id, for a voter nobody can
 * name — is typed. A voter whose node still holds a live presence lease AS A
 * MEMBER (or does not say) is refused unless forced — a running member removed
 * from the group rejoins it as a voter at its next restart — so for one the
 * dialog asks for that separately, and says why. A node alive as a leaf or a
 * client under a voter's name needs no force: its broker never rejoins.
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
import { ServerGlyph, ClockGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, TagsCell } from "~/app/frame/cells.tsx";
import { BROKER_REMOVE_TIMEOUT_MS, rest, RestError } from "~/protocol/index.ts";
import type { BrokerRemoved, FleetBrokerAnswer, MetaGroup, MetaPeer } from "~/protocol/index.ts";
import { fmtDuration } from "~/lib/format.ts";

/** One row of the panel: a live node, a voter no live node is, or both. */
export interface BrokerRow {
  /**
   * The node id — or empty for a voter no live node is and the answering
   * member cannot name.
   */
  node: string;
  /** The peer id the group counts it by (were it a voter). The row's key. */
  peer: string;
  /** What the node advertises, or undefined for a voter no live node is. */
  kind?: string;
  roles?: string[] | null;
  /** How the group counts it, or undefined for a node it does not. */
  voter?: MetaPeer;
}

/**
 * Every live node and every voter, once each, matched BY PEER ID and in id
 * order — so a voter no live node is still has a row (it is the member an
 * operator is looking for), and one whose name the answering member never
 * heard is still the live node it is where one is.
 */
export function brokerRows(answer: FleetBrokerAnswer): BrokerRow[] {
  const rows = new Map<string, BrokerRow>();
  for (const n of answer.nodes ?? []) {
    rows.set(n.peer, { node: n.node, peer: n.peer, kind: n.kind, roles: n.roles });
  }
  for (const p of answer.group?.peers ?? []) {
    const row = rows.get(p.peer) ?? { node: p.name, peer: p.peer };
    row.voter = p;
    rows.set(p.peer, row);
  }
  return [...rows.values()].sort(
    (a, b) => a.node.localeCompare(b.node) || a.peer.localeCompare(b.peer),
  );
}

/** A voter's node id where one is known, and its peer id where only that is. */
function voterLabel(node: string, peer?: string): string {
  return node || `peer ${peer ?? "?"}`;
}

/**
 * A node's broker kind as a tag — `unknown` marked, because it is the one
 * reading that is a guess, and a capacity seal counts it as a member. The fleet
 * screen's Broker column draws the same tag.
 */
export function BrokerKindTag({ kind }: { kind: string }) {
  return <Tag variant={kind === "unknown" ? "warning" : "neutral"}>{kind}</Tag>;
}

/** Who a removal names: a node id where one is known, else the peer id. */
interface RemovalTarget {
  node: string;
  peer: string;
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
  const [removing, setRemoving] = useState<RemovalTarget | null>(null);
  if (!answer) {
    return error ? (
      <Card>
        <Card.Header icon={<ServerGlyph size="sm" />}>Broker members</Card.Header>
        <Callout variant="warning" role="alert">
          <span>The broker&apos;s membership could not be read ({error}).</span>
        </Callout>
      </Card>
    ) : null;
  }
  if (answer.external) {
    return (
      <Card>
        <Card.Header icon={<ServerGlyph size="sm" />}>Broker members</Card.Header>
        <EmptyState
          size="compact"
          icon={<ServerGlyph size="xl" />}
          title="An external NATS cluster"
          description="This fleet's broker is somebody else's cluster (stream.type: nats). Its membership belongs to whoever runs it, and nothing here lists or changes it."
        />
      </Card>
    );
  }
  const rows = brokerRows(answer);
  const live = new Set((answer.nodes ?? []).map((n) => n.node));
  // WHETHER A REMOVAL NEEDS FORCE is the engine's rule, read off the same
  // rows: the voter's node is live AS A MEMBER, or does not say what its
  // broker is. A leaf or a client under its name never rejoins the group.
  const needsForce = (t: RemovalTarget) =>
    (answer.nodes ?? []).some(
      (n) =>
        (n.peer === t.peer || (t.node !== "" && n.node === t.node)) &&
        (n.kind === "member" || n.kind === "unknown"),
    );
  return (
    <Card padding="none">
      <Card.Header
        icon={<ServerGlyph size="sm" />}
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
                key={`${f.kind}:${f.node}:${f.peer ?? ""}`}
                variant={f.kind === "dead_member" ? "warning" : "info"}
                role={f.kind === "dead_member" ? "alert" : "status"}
                icon={<TriangleAlertGlyph size="md" />}
              >
                <span className="row wrap gap-2 baseline">
                  <span>
                    <strong>{FINDING_TITLE[f.kind] ?? f.kind}</strong>{" "}
                    <InlineCode>{voterLabel(f.node, f.peer)}</InlineCode> — {f.detail}.
                  </span>
                  {f.kind === "dead_member" && (
                    <Button
                      variant="ghost"
                      size="small"
                      onClick={() => setRemoving({ node: f.node, peer: f.peer ?? "" })}
                    >
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
        rowKey={(r) => r.peer}
        defaultSort="node"
        empty={{ title: "No node advertises a broker, and no voter was read" }}
        columns={[
          {
            key: "node",
            header: "Node",
            sortValue: (r) => r.node,
            cell: (r) =>
              !r.node ? (
                <span className="t-caption">name unknown</span>
              ) : live.has(r.node) ? (
                <KeyCell value={r.node} path={["settings", "nodes", r.node]} />
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
                <BrokerKindTag kind={r.kind} />
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
          {
            key: "peer",
            header: "Peer",
            shrink: true,
            sortValue: (r) => (r.voter ? r.peer : ""),
            // THE ID A REMOVAL OF A VOTER NOBODY CAN NAME GOES BY, and the
            // one the group counts every voter by.
            cell: (r) => (r.voter ? <InlineCode>{r.peer}</InlineCode> : null),
          },
        ]}
      />
      {removing && (
        <BrokerRemoveDialog
          node={removing.node}
          peer={removing.peer}
          live={needsForce(removing)}
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

/**
 * Sends one removal — by node id where one is known, else by peer id through
 * the route for a voter nobody can name — and reads back what it did.
 */
async function remove(node: string, peer: string, force: boolean): Promise<Heard> {
  const id = node || peer;
  const query: Record<string, string> = { confirm: id };
  if (force) query.force = "true";
  const options = { body: {}, query, timeoutMs: BROKER_REMOVE_TIMEOUT_MS };
  try {
    // EACH ROUTE WRITTEN OUT, so the dev proxy's gate reads its path.
    const answer = (
      await (node
        ? rest.request("POST", `/fleet/broker/remove/${encodeURIComponent(node)}`, options)
        : rest.request("POST", `/fleet/broker/remove-peer/${encodeURIComponent(peer)}`, options))
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
 * Removing a voter from the metadata group, named by its node id or — for one
 * nobody can name — by its peer id, which is then what is typed.
 *
 * `live` is whether this screen saw the voter's node holding a presence lease
 * as a member (or not saying): the engine refuses such a node unless forced,
 * so the dialog asks for that too rather than sending a request it knows will
 * be refused.
 */
export function BrokerRemoveDialog({
  node,
  peer,
  live,
  onDone,
  onClose,
}: {
  node: string;
  peer: string;
  live: boolean;
  onDone: () => void;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [forcing, setForcing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [heard, setHeard] = useState<Heard | null>(null);
  const id = node || peer;
  const confirmed = typed.trim() === id && (!live || forcing);
  const done = heard?.kind === "removed";

  async function send() {
    if (busy || !confirmed) return;
    setBusy(true);
    const next = await remove(node, peer, live && forcing);
    setHeard(next);
    setBusy(false);
    onDone();
  }

  return (
    <Modal
      open
      title={`Remove ${voterLabel(node, peer)} from the broker`}
      icon={<ServerGlyph size="md" />}
      onClose={onClose}
      // A REQUEST IN FLIGHT HAS AN OUTCOME NOBODY HAS HEARD YET.
      dismissable={!busy}
      closeDisabledReason="Waiting for the metadata group to commit the change."
      size="md"
      stackBody
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
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
            The metadata group stops counting <InlineCode>{voterLabel(node, peer)}</InlineCode> in
            its elections and its creates — the gesture for a member that is gone for good. A member
            that restarts under that name joins the group again as a new voter.
          </p>
          <label className="col" style={{ gap: 6 }}>
            <span className="t-caption">
              Type <InlineCode>{id}</InlineCode> to confirm
            </span>
            <Input
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoFocus
              spellCheck={false}
              disabled={busy}
              aria-label={`Type ${id} to confirm`}
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
                  <InlineCode>{voterLabel(node, peer)}</InlineCode> still holds a live presence
                  lease as a member, so it is running, and a running member removed from the group
                  rejoins it at its next restart. Stop it first — force this only for a member
                  wedged in a way that still renews its lease.
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
              <InlineCode>{voterLabel(heard.answer.node, heard.answer.peer)}</InlineCode> is no
              longer a voter, removed through <InlineCode>{heard.answer.by}</InlineCode>&apos;s
              system account.
            </span>
            {heard.answer.group && (
              <span className="t-caption">
                The group counts{" "}
                {heard.answer.group.peers.map((p) => voterLabel(p.name, p.peer)).join(", ")}.
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
        <Callout variant="warning" role="alert" icon={<ClockGlyph size="md" />}>
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
