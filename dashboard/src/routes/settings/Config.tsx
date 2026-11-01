/**
 * Configuration: what the fleet is running, how it got here, and what changed.
 *
 * THE DIFF LENS COMPARES AGAINST THE ACTIVE REVISION UNLESS A LINK NAMES
 * ANOTHER. What one save changed is its revision against its PARENT: once the
 * save is active, its diff against the active revision is empty, so a "View
 * changes" link that named only the revision opened "No differences" with a
 * note about credential rotation. `against=` is the side to compare with, and
 * picking a row from the history drops it, because a row picked here is a
 * question about the active document again.
 *
 * Read-only, deliberately. The previous editor was a bare JSON textarea with
 * no schema hints, no validation until Save, no diff before saving, and a
 * dirty flag that was set and never read — so navigating away lost the edit
 * silently. A config editor worth having is a real project; a config VIEWER
 * that shows the active revision, its history and its diffs is genuinely
 * useful today and cannot lose anybody's work.
 */

import { useCallback, useMemo } from "react";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { href, useParam } from "~/app/router.tsx";
import {
  ButtonLink,
  Card,
  CodeBlock,
  CopyButton,
  EmptyState,
  EmptyValue,
  InlineCode,
  SegmentedControl,
  Skeleton,
  TableDateCell,
  TableKeyCell,
  TableTextCell,
  Tag,
  PageNote,
  type DataGridActivationEvent,
  peekClickHandler,
} from "@crewlethq/ui";
import {
  FileTextGlyph,
  ServerGlyph,
  SplitGlyph,
  LayersGlyph,
  ClockGlyph,
  SlidersVerticalGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";

import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, usePeek, usePeekControls } from "~/app/frame/peekAddress.ts";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { authorLabel, throughOf } from "~/lib/attribution.ts";
import type { ConfigChange, FleetNode, RevisionMeta } from "~/protocol/index.ts";
import { reveal } from "~/lib/scroller.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { ENTITY_KINDS } from "~/contract/config.ts";

const LENSES = ["active", "entities", "audit", "diff"] as const;
type Lens = (typeof LENSES)[number];

/**
 * How far back the history is read.
 *
 * The same 100 for the grid and for the rail, so a revision the list can show
 * is a revision the rail can resolve — two limits would make a peek opened
 * from the bottom of the table answer "no such revision" about a row that is
 * on screen.
 */
const HISTORY_LIMIT = 100;

/**
 * How often the fleet's apply status is re-read.
 *
 * `admin/Fleet.tsx`'s interval, because it is the same lease table and the
 * same question — which node is on which revision — and two screens polling
 * one answer at different cadences is how two readings of one fleet come to
 * disagree while sitting side by side.
 */
const FLEET_POLL_MS = 15_000;

/**
 * What "no active revision" means, written ONCE.
 *
 * Three places on this screen answer that one engine state — the whole
 * document, a collection, and one entity out of it — and the engine says it
 * the same way in all three: `queries/config.go` answers nil for
 * ErrNoActiveRevision and the envelope omits an absent `data`, so it arrives
 * as `undefined` rather than as an error. Spelled separately, the Entities
 * lens told an operator on a fresh install that "the active revision declares
 * none of these" — a claim about a revision that does not exist, one lens away
 * from the truth.
 */
const NO_REVISION = {
  title: "No company configuration is active",
  // A COMMAND IS CODE, in a sentence as anywhere: the info card on this
  // screen sets `crewlet config import` as code, and the same command in
  // prose here would read as words rather than as something to type.
  hint: (
    <>
      Import one with <InlineCode>crewlet config import</InlineCode>.
    </>
  ),
};

/**
 * Who wrote a revision: the label it was recorded under, and WHAT that label
 * names.
 *
 * THE KIND IS THE REVISION'S OWN WORD, never inferred from the label: the name
 * spaces overlap (a seat's handle and a person's login can read alike), and a
 * label alone drew a node's boot seed and the reconcile loop's reloads as a
 * person's writes. The kinds are the engine's author kinds — `agent`, `human`
 * (a person writing as the seat the directory binds them to), `operator`
 * (anybody bound to none, under their whole login) and `system` (the engine
 * itself). Every revision carries its kind; the label may be empty, which is
 * said as such.
 *
 * AND THE CREDENTIAL BESIDE THE NAME ([authorLabel]), where it is not the name
 * itself: a machine token acting as its owner writes under the owner's login,
 * and which of their credentials wrote a revision is what an incident asks.
 */
export function RevisionAuthor({ revision }: { revision: RevisionMeta }) {
  const name = revision.created_by;
  const kind = revision.created_by_kind;
  return (
    // THE NAME OUTRANKS ITS KIND. In a narrow By column the name was cut to
    // "n…" beside a whole `operator` chip, which is the half of the fact a
    // reader can least do without. In a revisions table (`.crewlet-data-grid__cell >
    // .revision-author`) the chip wraps onto a line the cell does not draw
    // before the name gives up a character; the title keeps both.
    <span
      className="row gap-1 revision-author"
      title={name ? `${authorLabel(name, revision.operator_id)} · ${kind}` : undefined}
    >
      {name ? (
        <TableTextCell>{authorLabel(name, revision.operator_id)}</TableTextCell>
      ) : (
        <EmptyValue label="No name recorded" />
      )}
      <Tag appearance="outline">{kind}</Tag>
    </span>
  );
}

/**
 * What this revision is to the fleet, in three words rather than two.
 *
 * "NOT ACTIVE" IS TWO DIFFERENT FACTS on this record: a revision the fleet
 * activated and has since replaced carries an `activated_at`, and one that was
 * stored and never activated — a write that lost its race, an offline change
 * the fleet had moved on from — does not. Collapsing them into "superseded"
 * tells a reader the fleet took a document it never took — and the history is
 * exactly where somebody goes looking for the revision that never landed.
 *
 * ACTIVE IS THE FLEET'S, not a node's: `is_active` names the revision the
 * fleet's activation pointer names. Whether each node runs it is the Across
 * the fleet list beside it.
 */
function RevisionState({ revision }: { revision: RevisionMeta }) {
  if (revision.is_active) return <Tag variant="success">active</Tag>;
  if (revision.activated_at) return <Tag appearance="outline">superseded</Tag>;
  return <Tag appearance="outline">never activated</Tag>;
}

/**
 * A value as a diff draws it: JSON, so `"true"` and `true` stay two different
 * settings (the harness's `renderValue` rule), and an object or a list in the
 * document's own indented shape rather than as one run-on line.
 *
 * ABSENT IS `null`. `configapi.Change` omits an empty side, and Go's
 * `omitempty` on an `any` drops a JSON null with it — so a setting changed
 * from null arrives with no `from` at all, and `JSON.stringify` of that is
 * `undefined`, which the line would print as the word.
 */
function shownValue(value: unknown): string {
  if (value !== null && typeof value === "object") return JSON.stringify(value, null, 2);
  return JSON.stringify(value) ?? "null";
}

/**
 * What one change says about its value: the side that exists, or both.
 *
 * A change whose values are objects puts the arrow at the head of its own line
 * — after the old block, before the new one — because trailing a closing
 * brace it reads as part of it.
 */
export function changeValue(change: ConfigChange): string {
  if (change.kind === "added") return shownValue(change.to);
  if (change.kind === "removed") return shownValue(change.from);
  const from = shownValue(change.from);
  const to = shownValue(change.to);
  return from.includes("\n") || to.includes("\n") ? `${from}\n→ ${to}` : `${from} → ${to}`;
}

/** The mark each kind of change is drawn with, beside its tint. */
const CHANGE_MARK: Record<ConfigChange["kind"], string> = {
  added: "+",
  removed: "−",
  changed: "~",
};

/**
 * A comparison, one change a row: its mark, the path and the value, WHOLE.
 *
 * The tracks are `.config-diff`'s (see there): the path wraps rather than
 * cutting, and so does the value. The mark is a glyph as well as a tint,
 * because colour alone is not a signal, and the kind is said in words to a
 * reader who hears the list rather than seeing it.
 */
function ConfigChanges({ changes }: { changes: ConfigChange[] }) {
  return (
    <ul className="config-diff">
      {changes.map((change, i) => (
        <li key={i} className="config-diff-line" data-kind={change.kind}>
          <span className="config-diff-mark">
            <span aria-hidden="true">{CHANGE_MARK[change.kind]}</span>
            <span className="sr-only">{change.kind}</span>
          </span>
          <span className="config-diff-path">{change.path}</span>
          <span className="config-diff-value">{changeValue(change)}</span>
        </li>
      ))}
    </ul>
  );
}

/** The Diff lens's head, which a row pressed below it brings back on screen. */
const CHANGES_HEAD_ID = "config-changes";

/**
 * Which nodes run this revision, which refused it, which run another, and
 * which have not said.
 *
 * FOUR ANSWERS, NOT TWO. The third is the one this panel exists for: a node
 * that published no apply record is not a node that failed to apply, and
 * rendering it among the stragglers would send an operator after a machine
 * that is fine. It is the same three-valued rule `coord` is built on — held,
 * definitively not, unknown — read at the one place an operator acts on it.
 *
 * And a node is counted by the revision it RUNS (`config_serving_revision_id`),
 * never by the one it last attempted: a node that refused this revision
 * reports it beside `error` and goes on serving the one before, and counting
 * it here called a node converged that runs another company. It is listed as
 * refused, naming what it runs — and so is a node whose last attempt of this
 * revision failed although it runs it ([nodeOnRevision]): the refused
 * re-activation of a credential rotation, which goes on running the revision
 * with the credentials it had. The count is of the nodes that run it and
 * refused nothing about it.
 *
 * `flush` is the peek: the rail is the panel already.
 */
function AppliedAcross({
  revisionId,
  nodes,
  answered,
  loading,
  flush,
}: {
  revisionId: string;
  nodes: FleetNode[];
  /** Whether the fleet read answered at all. */
  answered: boolean;
  /** Still in flight, which is a third thing again — see below. */
  loading?: boolean;
  flush?: boolean;
}) {
  const lines = nodes.map((node) => ({ node, state: nodeOnRevision(node, revisionId) }));
  const here = lines.filter((l) => l.state === "here");
  // THE ORDER A READER ACTS IN: what runs it, what refused it, what runs
  // another, and what has said nothing.
  const ordered = (["here", "refused", "elsewhere", "silent"] as const).flatMap((state) =>
    lines.filter((l) => l.state === state),
  );

  // A READ IN FLIGHT IS NOT A READ THAT FAILED, and for the second or two
  // before the lease table lands they render identically: without this the
  // panel opens by announcing that the fleet did not answer, and then quietly
  // contradicts itself.
  const body =
    loading && !answered ? (
      <Skeleton variant="text" rows={2} label="Loading" />
    ) : !answered ? (
      <p className="t-body muted">Couldn’t tell which nodes run this revision.</p>
    ) : nodes.length === 0 ? (
      <p className="t-body">No node is running right now.</p>
    ) : (
      <div className="col gap-2">
        <p className="t-body">
          {here.length} of {plural(nodes.length, "node")} run this revision.
        </p>
        <div className="col" style={{ gap: 2 }}>
          {ordered.map(({ node, state }) => (
            <NodeLine key={node.id} node={node} state={state} />
          ))}
        </div>
      </div>
    );

  if (flush) {
    return (
      <section className="col gap-2">
        <div className="t-label">Across the fleet</div>
        {body}
      </section>
    );
  }
  return (
    <Card>
      <Card.Header icon={<ServerGlyph size="sm" />}>
        <Card.Title>Across the fleet</Card.Title>
      </Card.Header>
      {body}
    </Card>
  );
}

/** Where one node stands on one revision. */
type NodeOnRevision = "here" | "refused" | "elsewhere" | "silent";

/**
 * Where one node stands on one revision: running it, having refused it (its
 * last attempt was this revision and failed), running another, or silent.
 *
 * THE REFUSAL IS READ BEFORE THE REVISION IT RUNS. Re-activating the revision
 * a node already runs is how a credential is rotated, and a node that refuses
 * that activation names the one revision in both fields: attempted and still
 * served, beside `error` (`FleetNode.config_serving_revision_id`). Read by
 * what it runs first, it was tagged "runs it" in the success tone and its
 * `config_error` went unsaid, on the panel an operator opens to see whether
 * the rotation took — while the Nodes screen (`configStanding`) called the
 * same node refused.
 *
 * Exported for the test, which holds the four apart without a screen.
 */
export function nodeOnRevision(node: FleetNode, revisionId: string): NodeOnRevision {
  const failed = node.config_status === "error" || node.config_status === "degraded";
  if (failed && (node.config_revision_id ?? "") === revisionId) return "refused";
  const runs = node.config_serving_revision_id ?? "";
  if (runs === revisionId) return "here";
  if (runs !== "" || (node.config_revision_id ?? "") !== "") return "elsewhere";
  return "silent";
}

/** One node's answer about one revision. */
function NodeLine({ node, state }: { node: FleetNode; state: NodeOnRevision }) {
  const runs = node.config_serving_revision_id ?? "";
  return (
    <div className="row gap-2">
      <InlineCode>{node.id}</InlineCode>
      {state === "here" ? (
        <Tag variant="success">runs it</Tag>
      ) : state === "refused" ? (
        <Tag variant="danger" title={runs || undefined}>
          {node.config_status === "degraded" ? "applied in part" : "refused"}
          {runs ? `, runs ${runs.slice(0, 10)}` : ", runs none"}
        </Tag>
      ) : state === "elsewhere" ? (
        <Tag variant="warning" title={runs || undefined}>
          {runs ? `on ${runs.slice(0, 10)}` : "runs none"}
        </Tag>
      ) : (
        <Tag appearance="outline">not saying</Tag>
      )}
      {/* WHY IT REFUSED, which the tag alone cannot say. Only for this
          revision: an error about another one is that revision's story. */}
      {state === "refused" && (
        <span className="t-caption" title={node.config_error}>
          {node.config_error || "it could not apply that revision"}
        </span>
      )}
    </div>
  );
}

/** A revision's provenance — the fields the history table has no room for. */
function RevisionBody({
  revision,
  nodes,
  answered,
  loading,
  flush,
}: {
  revision: RevisionMeta;
  nodes: FleetNode[];
  answered: boolean;
  loading?: boolean;
  flush?: boolean;
}) {
  // EVERY PROPERTY ONCE, here and nowhere else. The header above carries the
  // revision's identity and its state and passes no facts: stacked in one
  // column — the peek's, and the page's, whose cards run under the header
  // rather than beside it — a fact line and this group said Created, By,
  // Source and Activated twice within a hundred pixels. The ids are VALUES
  // in the mono face; the labels are words.
  const provenance = (
    <PropertiesRail
      groups={[
        {
          properties: [
            { label: "Revision", value: <span className="mono">{revision.revision_id}</span> },
            {
              label: "Parent",
              value: revision.parent_revision_id ? (
                <span className="mono">{revision.parent_revision_id}</span>
              ) : undefined,
              title: "the revision this one was written against",
            },
            { label: "Source", value: revision.source },
            { label: "Created by", value: <RevisionAuthor revision={revision} /> },
            {
              label: "Through",
              value: throughOf(revision.created_by, revision.operator_id) ? (
                <span className="mono">{throughOf(revision.created_by, revision.operator_id)}</span>
              ) : undefined,
              title: "the credential the revision was written through",
            },
            { label: "Created", value: fmtDateTime(revision.created_at) },
            {
              label: "Activated",
              value: revision.activated_at ? fmtDateTime(revision.activated_at) : undefined,
            },
          ],
        },
      ]}
    />
  );

  return (
    <>
      {flush ? (
        <section className="col gap-2">
          <div className="t-label">Where it came from</div>
          {provenance}
        </section>
      ) : (
        <Card>
          <Card.Header icon={<FileTextGlyph size="sm" />}>
            <Card.Title>Where it came from</Card.Title>
          </Card.Header>
          {provenance}
        </Card>
      )}
      <AppliedAcross
        revisionId={revision.revision_id}
        nodes={nodes}
        answered={answered}
        loading={loading}
        flush={flush}
      />
    </>
  );
}

/**
 * One configuration revision, beside the history it was found in.
 *
 * # It asks for the history, not for the revision
 *
 * There is no question that answers one revision: `config_audit` answers the
 * recent ones and the rail picks its own out. That is also why a revision
 * older than the window says so rather than rendering an empty header — the
 * answer is bounded, and a bounded answer that did not contain something is
 * not evidence the thing is gone.
 *
 * # And it asks the fleet
 *
 * Because the question a reader has about a revision they did not write is
 * whether the company is actually RUNNING it, and that is a property of the
 * nodes rather than of the document: the pointer says which revision is meant
 * to be active and each node writes its own apply status beside it.
 */
export function RevisionPeek({ id }: { id: string }) {
  const audit = useQuery("config_audit", { limit: HISTORY_LIMIT }, { enabled: id !== "" });
  const fleet = useQuery("fleet", undefined, { enabled: id !== "", pollMs: FLEET_POLL_MS });

  const revision = useMemo(
    () => (audit.data ?? []).find((r) => r.revision_id === id) ?? null,
    [audit.data, id],
  );

  return (
    <>
      {audit.loading && !audit.data && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState error={audit.error} refusal={audit.refusal} loading={audit.loading}>
        {/* NOT AN EMPTY RAIL. A `peek=revision:` arrives from a pasted URL as
            often as from a row, and naming the id that resolved to nothing —
            and the window it was looked for in — is more use than a header
            over no revision. */}
        {audit.data && !revision && (
          <EmptyState
            size="compact"
            icon={<ClockGlyph size="xl" />}
            title="No such revision in the recent history"
            description={`It may be older than the last ${HISTORY_LIMIT}, or the id is wrong.`}
          />
        )}
        {revision && (
          <>
            <ObjectHeader
              size="peek"
              kind="Revision"
              icon="file-text"
              identifier={revision.revision_id.slice(0, 10)}
              title={revision.summary || "No summary was written"}
              status={<RevisionState revision={revision} />}
            />
            <div className="col gap-3">
              <RevisionBody
                revision={revision}
                nodes={fleet.data?.nodes ?? []}
                answered={Boolean(fleet.data)}
                loading={fleet.loading}
                flush
              />
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

export function ConfigScreen({
  revision: revisionPath,
  revisions = false,
}: {
  revision?: string;
  /** The address is `#/settings/config/revisions[/{id}]`. */
  revisions?: boolean;
}) {
  const now = useNow();
  // ONE REVISION'S PAGE IS AN OBJECT, NOT A LENS. It drew the lens bar with
  // Active pressed and the whole active document under the revision's own
  // header, so a reader looking at last week's save was told, by the one
  // pressed control on the screen, that they were looking at what is running
  // now. On that page the lenses are not drawn and not asked for, and the
  // digits are left alone ("filter"), because there is no strip for them to
  // move.
  const onRevision = revisionPath !== undefined;
  // `#/settings/config/revisions` IS THE HISTORY. The trail names that address
  // "Revisions" and a revision's page links back to it, so it lands on the
  // History lens — as the Active lens it showed the running document under a
  // crumb that said something else.
  const [lens, setLens] = useTab(
    "lens",
    LENSES,
    onRevision ? "filter" : "section",
    revisions && !onRevision ? "audit" : "active",
  );
  const [chosen, setRevision] = useParam("revision", "");
  // THE SIDE A DIFF IS READ AGAINST. Empty is the active revision, which is
  // what the engine's own `against: "active"` means, so the parameter is
  // absent from every link that wants the ordinary comparison. A FILTER rather
  // than a section: it narrows what is already on screen, so it replaces the
  // history entry instead of adding one to press Back through.
  const [against, setAgainst] = useParam("against", "", "filter");

  const [kind, setKind] = useParam("kind", ENTITY_KINDS[0].kind);
  const [entity, setEntity] = useParam("entity", "");

  // `#/settings/config/revisions/{id}` ADDRESSES ONE REVISION, and this screen
  // accepted the segment and dropped it: a reader who followed a link to one
  // revision got the Active lens with nothing selected and no sign of what
  // they had asked for — and it is where `Open ↗` from the rail lands. The
  // query key still wins where a reader picked a row, so the path is the
  // landing rather than a lock.
  const revision = chosen || revisionPath || "";

  const active = useQuery("config", undefined, { enabled: !onRevision && lens === "active" });
  // ONE COLLECTION AT A TIME, which is what makes this different from the
  // Active lens rather than a second copy of it: the whole document is one
  // unreadable block of JSON, and the question a reader actually has is
  // "what does THIS seat's configuration say".
  const ids = useQuery(
    "config_entities",
    { kind },
    { enabled: !onRevision && lens === "entities" },
  );
  const one = useQuery(
    "config_entities",
    { kind, id: entity },
    { enabled: !onRevision && lens === "entities" && entity !== "" },
  );
  const audit = useQuery(
    "config_audit",
    { limit: HISTORY_LIMIT },
    {
      // THE HISTORY IS ALSO WHAT THE ADDRESSED REVISION IS READ FROM, so the
      // block below has something to render on the lens a link lands on
      // rather than only on the two that show the table.
      enabled: onRevision || lens === "audit" || lens === "diff",
    },
  );
  const diff = useQuery(
    "config_diff",
    { revision_id: revision, against: against || "active" },
    { enabled: !onRevision && lens === "diff" && !!revision },
  );
  const fleet = useQuery("fleet", undefined, {
    enabled: onRevision,
    pollMs: FLEET_POLL_MS,
  });

  // A NIL ANSWER IS "NO ACTIVE REVISION", NOT AN EMPTY COLLECTION, and the two
  // are one lens apart on this screen: the Active lens already says so, and
  // the Entities lens was reading the same silence as "the active revision
  // declares none of these". It arrives as `undefined` rather than `null` —
  // the envelope omits an absent `data` — so this tests falsiness, and it
  // tests `loading` and `error` first because neither of those is an answer.
  const noRevision = !ids.loading && !ids.error && !ids.data;

  const rows = useMemo(() => audit.data ?? [], [audit.data]);

  // THE ORDER `[` AND `]` WALK — the history as the engine answered it, which
  // is the order the table lands in.
  usePeekNeighbours(
    useMemo(() => rows.map((r) => ({ kind: "revision" as const, id: r.revision_id })), [rows]),
  );

  const { open: openPeek } = usePeekControls();
  // THE ROW THE RAIL IS OPEN ON IS MARKED, as the work list marks its peeked
  // item. A row's click sets `revision=` and opens the peek together, but `[`
  // and `]` move the peek alone, so a mark keyed on `revision=` stayed on the
  // row the reader had stepped away from; with no revision peeked, the picked
  // revision (the Diff lens's subject, or the addressed one) is the mark.
  const peek = usePeek();
  const marked = peek?.kind === "revision" ? peek.id : revision;

  /**
   * A row's click.
   *
   * IT ALSO POINTS THE DIFF. Picking a revision in this table has always meant
   * "compare this one", and a peek that left the Diff lens on whatever was
   * selected last would make the two halves of the screen describe different
   * revisions. The lens itself is not changed: a reader who is reading the
   * history asked for the history.
   *
   * THE GRID HANDS THIS BOTH EVENTS. `peekClickHandler` is the frame's one copy
   * of which clicks mean elsewhere and reads a mouse event; the `enter` chord
   * carries no button at all and is never "open elsewhere".
   */
  const openRevision = useCallback(
    (row: RevisionMeta, e: DataGridActivationEvent) => {
      const go = () => {
        setRevision(row.revision_id);
        // AND DROPS THE SIDE A LINK NAMED. A row picked here is "compare this
        // one with what we are running"; carrying somebody else's `against=`
        // into it would answer a question this reader did not ask.
        setAgainst("");
        openPeek({ kind: "revision", id: row.revision_id });
        // ON THE DIFF LENS THE PRESS IS A QUESTION ABOUT THE CHANGES, which
        // sit above the table: a row pressed forty revisions down changed a
        // card the reader could not see. Brought on screen only when it is
        // off it, and focus stays on the row, where `[` and `]` and the
        // arrows keep stepping through the history.
        if (lens === "diff") reveal(document.getElementById(CHANGES_HEAD_ID));
      };
      if (!("button" in e)) {
        go();
        return;
      }
      peekClickHandler(go)?.(e);
    },
    [openPeek, setRevision, setAgainst, lens],
  );

  const pretty = useMemo(
    () => (active.data ? JSON.stringify(active.data, null, 2) : ""),
    [active.data],
  );

  const addressed = revisionPath
    ? (rows.find((r) => r.revision_id === revisionPath) ?? null)
    : null;
  // WHAT THE REVISION IS CALLED, for the breadcrumb, the browser tab and the
  // palette's recents — all of which read the one label a screen publishes and
  // otherwise show the raw path segment, which here is a revision id. THE
  // SUMMARY ONLY, never the header's "No summary was written": a header needs
  // a word in the slot and a recents row needs to name one revision, and two
  // rows reading "No summary was written" name none.
  usePageLabels(revisionPath && addressed?.summary ? { [revisionPath]: addressed.summary } : {});

  return (
    <>
      {!onRevision && (
        <PageActions>
          <SegmentedControl<Lens>
            semantics="radio"
            activate="manual"
            label="Configuration view"
            value={lens}
            onValueChange={setLens}
            options={[
              { value: "active", label: "Active", icon: <NamedGlyph name="file-text" size="xs" /> },
              {
                value: "entities",
                label: "Entities",
                icon: <NamedGlyph name="layers" size="xs" />,
              },
              { value: "audit", label: "History", icon: <NamedGlyph name="clock" size="xs" /> },
              { value: "diff", label: "Diff", icon: <NamedGlyph name="split" size="xs" /> },
            ]}
          />
        </PageActions>
      )}

      {/* WHAT THE SCREEN IS, said first and once, as every Settings page
          that needs it says it: a callout under the last card was a footnote
          a reader met after everything it qualifies. */}
      <PageNote>
        Read only here. Change the company document with{" "}
        <InlineCode>crewlet config import</InlineCode>.
      </PageNote>

      {/* THE REVISION THIS PATH NAMES, above the lens rather than below it:
          the reader followed a link to one revision, so what it is has to be
          the first thing on the screen and the lens they land on is a second
          question. */}
      {revisionPath !== undefined && (
        <>
          {audit.loading && !audit.data && <Skeleton variant="text" rows={4} label="Loading" />}
          <QueryState error={audit.error} refusal={audit.refusal} loading={audit.loading}>
            {audit.data && !addressed && (
              <EmptyState
                icon={<ClockGlyph size="xl" />}
                title="No such revision in the recent history"
                description={`It may be older than the last ${HISTORY_LIMIT}, or the id is wrong.`}
              />
            )}
            {addressed && (
              <>
                <ObjectHeader
                  kind="Revision"
                  icon="file-text"
                  identifier={addressed.revision_id.slice(0, 10)}
                  title={addressed.summary || "No summary was written"}
                  status={<RevisionState revision={addressed} />}
                  actions={
                    // WHAT THIS SAVE CHANGED is the revision against its
                    // PARENT (see the file's head): against the active one,
                    // the active revision's own "See its changes" opened
                    // "No differences". The company's first revision has no
                    // parent, and all of it is what that save wrote, which the
                    // provenance below already says.
                    addressed.parent_revision_id ? (
                      <ButtonLink
                        size="small"
                        leadingIcon={<SplitGlyph size="sm" />}
                        href={href(["settings", "config"], {
                          lens: "diff",
                          revision: addressed.revision_id,
                          against: addressed.parent_revision_id,
                        })}
                      >
                        See its changes
                      </ButtonLink>
                    ) : undefined
                  }
                />
                <RevisionBody
                  revision={addressed}
                  nodes={fleet.data?.nodes ?? []}
                  answered={Boolean(fleet.data)}
                  loading={fleet.loading}
                />
              </>
            )}
          </QueryState>
        </>
      )}

      {!onRevision && lens === "active" && (
        <>
          {active.loading && <Skeleton variant="text" rows={6} label="Loading" />}
          <QueryState error={active.error} refusal={active.refusal} loading={active.loading}>
            {active.data ? (
              // FLUSH: the block is the card's content, so it takes the
              // card's surface rather than drawing a second frame inside it.
              <Card padding="none">
                <Card.Header
                  icon={<FileTextGlyph size="sm" />}
                  subtitle="as the engine resolved it"
                  actions={<CopyButton text={pretty} title="the active revision, as JSON" />}
                >
                  <Card.Title>Active revision</Card.Title>
                </Card.Header>
                {/* BOUNDED, like every other record block, by the ceiling the
                    shell declares once (`--record-ceiling`, frame.css). A
                    whole company configuration runs to hundreds of lines, and
                    it is one of the records that ceiling is written down for
                    by name. Unbounded, it pushes the entity lens below it off
                    the screen. */}
                <CodeBlock
                  plain
                  selectable
                  label="The active company configuration, as JSON"
                  code={pretty}
                />
              </Card>
            ) : (
              <EmptyState
                icon={<SlidersVerticalGlyph size="xl" />}
                title={NO_REVISION.title}
                description={NO_REVISION.hint}
              />
            )}
          </QueryState>
        </>
      )}

      {!onRevision && lens === "entities" && (
        <>
          <SegmentedControl<string>
            semantics="radio"
            activate="manual"
            label="Which collection"
            value={kind}
            onValueChange={(next) => {
              setKind(next);
              // AN ID BELONGS TO ONE COLLECTION. Carrying it across would ask
              // for a seat's handle out of the MCP servers and render the
              // refusal that comes back.
              setEntity("");
            }}
            options={ENTITY_KINDS.map((k) => ({ value: k.kind, label: k.label }))}
          />
          {ids.loading && !ids.data && <Skeleton variant="text" rows={4} label="Loading" />}
          <QueryState
            error={ids.error}
            refusal={ids.refusal}
            loading={ids.loading}
            empty={
              noRevision
                ? NO_REVISION
                : (ids.data?.ids ?? []).length
                  ? undefined
                  : {
                      title: "Nothing in this collection",
                      hint: "The active revision declares none of these.",
                    }
            }
          >
            {/* A LIST AND ITS DETAIL, as one grid: a fixed list track and a
                `1fr` detail, so the detail card fills the row like every other
                full-width card on this screen. As a wrapping row both were
                sized to their content, and the detail ended wherever its empty
                state's sentence did. */}
            <div className="entity-pick">
              <Card padding="none">
                <Card.Header icon={<LayersGlyph size="sm" />} count={(ids.data?.ids ?? []).length}>
                  {ENTITY_KINDS.find((k) => k.kind === kind)?.label ?? kind}
                </Card.Header>
                <div className="col entity-pick-list">
                  {(ids.data?.ids ?? []).map((id) => (
                    // A LIST SELECTION, NOT AN ACTION, so it is drawn as the
                    // Settings column draws its current row — the neutral
                    // raised surface and its hairline, the handle in the
                    // primary ink — and never as the kit's primary button.
                    // As one it filled solid violet under the pointer the
                    // moment it was picked, over a code chip that kept its
                    // own grey ink: 1.5:1 in the light theme. The accent is
                    // for the one primary action, which a picked row is not.
                    <button
                      key={id}
                      type="button"
                      className="entity-pick-row"
                      aria-pressed={id === entity}
                      // The list track is fixed, so a handle longer than it is
                      // cut; the whole handle is still one hover away.
                      title={id}
                      onClick={() => setEntity(id === entity ? "" : id)}
                    >
                      {id}
                    </button>
                  ))}
                </div>
              </Card>
              <Card>
                <Card.Header
                  icon={<FileTextGlyph size="sm" />}
                  subtitle="as the active revision declares it"
                  actions={
                    one.data?.entity ? (
                      <CopyButton
                        text={JSON.stringify(one.data.entity, null, 2)}
                        title={`${entity}, as JSON`}
                      />
                    ) : undefined
                  }
                >
                  {entity || "Pick one"}
                </Card.Header>
                {!entity ? (
                  <EmptyState
                    size="compact"
                    icon={<LayersGlyph size="xl" />}
                    title="Pick one from the list"
                    description="Its part of the configuration appears here."
                  />
                ) : one.loading && !one.data ? (
                  <Skeleton variant="text" rows={6} label="Loading" />
                ) : (
                  <QueryState error={one.error} refusal={one.refusal} loading={one.loading}>
                    {one.data ? (
                      <CodeBlock
                        plain
                        selectable
                        label={`${entity}, as JSON`}
                        code={JSON.stringify(one.data.entity, null, 2)}
                      />
                    ) : (
                      // THE SAME SILENCE, IN THE PANEL. `entity` is a URL key,
                      // so a shared `?lens=entities&entity=…` lands here on a
                      // deployment with nothing active — and this rendered the
                      // literal word `null` as if it were the seat's own slice
                      // of the document.
                      <EmptyState
                        size="compact"
                        icon={<SlidersVerticalGlyph size="xl" />}
                        title={NO_REVISION.title}
                        description="Import a company to see it."
                      />
                    )}
                  </QueryState>
                )}
              </Card>
            </div>
          </QueryState>
        </>
      )}

      {/* THE LENS'S SUBJECT FIRST, and the revisions it is picked from under
          it. Drawn under the table, the changes began below fifteen rows of
          history — off the screen at 1440 × 900 — so "See its changes" on a
          revision's page landed on a list and showed no change at all. */}
      {!onRevision && lens === "diff" && (
        <Card>
          <Card.Header
            id={CHANGES_HEAD_ID}
            icon={<SplitGlyph size="sm" />}
            subtitle={
              against ? `against revision ${against.slice(0, 10)}` : "against the active revision"
            }
          >
            {revision ? `Changes in ${revision.slice(0, 10)}` : "Diff"}
          </Card.Header>
          {!revision ? (
            <EmptyState
              size="compact"
              icon={<SplitGlyph size="xl" />}
              title="Pick a revision below"
              description="Its changes against the active one appear here."
            />
          ) : diff.loading ? (
            <Skeleton variant="text" rows={4} label="Loading" />
          ) : (
            <QueryState
              error={diff.error}
              refusal={diff.refusal}
              loading={diff.loading}
              empty={
                diff.data?.changes?.length
                  ? undefined
                  : {
                      title: "No differences",
                      hint: "Identical to the active revision.",
                    }
              }
            >
              <div className="col" style={{ gap: 6 }}>
                <ConfigChanges changes={diff.data?.changes ?? []} />
                {/* Both `?? 0` guard the ANSWER, not the fields: `changes`
                    and `changes_total` are always sent together, but
                    `diff.data` is null until the query lands, and an
                    unanswered comparison must render nothing rather than
                    compare two undefineds. */}
                {(diff.data?.changes_total ?? 0) > (diff.data?.changes.length ?? 0) && (
                  // THE CUT, SAID. The answer is bounded by its response
                  // budget, so this list is a page of the comparison —
                  // without a line here a short diff reads as "that is
                  // all that changed". The server used to report it as a
                  // pathless CHANGE, which this screen drew as a blank
                  // path turning undefined into a sentence.
                  <div className="t-caption">
                    {diff.data?.changes.length} of {diff.data?.changes_total} shown.{" "}
                    <InlineCode>crewlet config diff</InlineCode> shows all.
                  </div>
                )}
              </div>
            </QueryState>
          )}
        </Card>
      )}

      {!onRevision && (lens === "audit" || lens === "diff") && (
        <>
          {audit.loading && <Skeleton variant="text" rows={5} label="Loading" />}
          <QueryState
            error={audit.error}
            refusal={audit.refusal}
            loading={audit.loading}
            empty={
              rows.length
                ? undefined
                : {
                    title: "No revisions recorded",
                    hint: "The history begins with the first import.",
                  }
            }
          >
            <Card padding="none">
              <Card.Header icon={<ClockGlyph size="sm" />} count={rows.length}>
                <Card.Title>Revisions</Card.Title>
              </Card.Header>
              <DataGrid<RevisionMeta>
                rows={rows}
                rowKey={(r) => r.revision_id}
                defaultSort="-at"
                // A ROW IS A REAL LINK AND A PLAIN CLICK PEEKS. The href is
                // the frame's own answer to where a revision lives, so a
                // row's target and the rail's `Open ↗` can never name
                // different pages.
                rowHref={(r) => peekHref({ kind: "revision", id: r.revision_id })}
                onRowActivate={openRevision}
                isSelected={(r) => r.revision_id === marked}
                columns={[
                  {
                    key: "at",
                    align: "right",
                    header: "When",
                    shrink: true,
                    // THE ONE COLUMN THAT GIVES WAY, and it is the revision
                    // peek's own "Created". Beside a peek at 1440 the table is
                    // ~486px, and with When drawn the summary kept ~128px —
                    // "Add the MCP server…" — while When, the id and its pill
                    // held the rest.
                    drop: 1,
                    sortValue: (r) => tsKey(r.created_at),
                    cell: (r) => <TableDateCell value={r.created_at} now={now} />,
                  },
                  {
                    key: "id",
                    header: "Revision",
                    // ITS OWN WIDTH, EXACTLY. Ten characters of id and an
                    // `active` pill are bounded, and as a second `1fr` this
                    // column took ~340px at 1440 while Summary was cut — and
                    // under a peek it cut the active id itself ("cebf4afb…").
                    // Summary is the one flexible track.
                    width: "max-content",
                    cell: (r) => (
                      <span className="row gap-1">
                        <TableKeyCell value={r.revision_id.slice(0, 10)} />
                        {r.is_active && <Tag variant="success">active</Tag>}
                      </span>
                    ),
                  },
                  {
                    key: "summary",
                    header: "Summary",
                    // THE ONE FLEXIBLE TRACK, floored so it cannot be the
                    // column that gives way: at a floor of zero every
                    // content-sized column beside it was drawn whole and the
                    // summary took what was left, so the grid never had a
                    // reason to hide When. At 12rem a row that cannot hold
                    // When, the id and the author beside it drops When
                    // instead ("Hidden to fit: When").
                    floor: "12rem",
                    sortValue: (r) => r.summary,
                    // NOT `value || "—"`. A revision imported without a
                    // message has no summary, and the dash says so rather
                    // than standing in for an empty string.
                    cell: (r) =>
                      r.summary ? (
                        <TableTextCell>{r.summary}</TableTextCell>
                      ) : (
                        <EmptyValue label="No summary was written" />
                      ),
                  },
                  {
                    key: "author",
                    header: "By",
                    shrink: true,
                    sortValue: (r) => r.created_by,
                    cell: (r) => <RevisionAuthor revision={r} />,
                  },
                ]}
              />
            </Card>
          </QueryState>
        </>
      )}
    </>
  );
}
