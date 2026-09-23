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
import { useParam } from "~/app/router.tsx";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  CopyButton,
  EmptyState,
  EmptyValue,
  InlineCode,
  Skeleton,
  Tag,
} from "@crewlethq/ui";
import {
  DescriptionGlyph,
  DnsGlyph,
  ForkRightGlyph,
  LayersGlyph,
  ScheduleGlyph,
  TuneGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState, RECORD_MAX_HEIGHT } from "~/components/common.tsx";
// OURS, DELIBERATELY. uilet's `SegmentedControl` has no manual-activation
// mode: with `semantics="radio"` its arrow keys COMMIT the option they land
// on, and every group on this screen drives a `useParam` — the lens pushes a
// history entry and re-runs the screen's query, and the collection picker
// re-runs `config_entities`. Arrowing across four lenses under that control
// is four queries nobody asked for and four Back presses to undo. See the
// note on `useRovingGroup`; this is the trade the pattern itself names.
import { Segmented } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { FleetNode, RevisionMeta } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
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
  hint: "The engine is running with nothing to run: no seats are spawned, and every inbound webhook is refused with a 503 its sender will retry. Import one with crewlet config import, or PUT /config.",
} as const;

/**
 * The facts a revision is recognised by, in the history table's own order.
 *
 * ONE FUNCTION for the page and the rail, so a reader who peeks a revision and
 * then opens it reads the same things in the same places. Whether it is ACTIVE
 * is not among them — that is the header's own pill, and a state spelled in
 * colour and again in a list reads as two facts about one revision.
 */
function revisionFacts(revision: RevisionMeta, now: number): Fact[] {
  return [
    { label: "Created", value: <DateCell at={revision.created_at} now={now} /> },
    { label: "By", value: revision.created_by || "nobody recorded" },
    { label: "Source", value: revision.source },
    {
      label: "Activated",
      // ABSENT IS NOT A DATE. A revision that was stored and never activated
      // has no activation, and the fact line drops a fact with no value
      // rather than claiming one.
      value: revision.activated_at ? <DateCell at={revision.activated_at} now={now} /> : "",
    },
  ];
}

/**
 * What this revision is to the fleet, in three words rather than two.
 *
 * "NOT ACTIVE" IS TWO DIFFERENT FACTS on this record: a revision that was
 * active and has since been replaced carries an `activated_at`, and one that
 * was stored and never applied does not. Collapsing them into "superseded"
 * tells a reader a document ran when nothing ever ran it — and the history is
 * exactly where somebody goes looking for the revision that never landed.
 */
function RevisionState({ revision }: { revision: RevisionMeta }) {
  if (revision.is_active) return <Tag variant="success">active</Tag>;
  if (revision.activated_at) return <Tag appearance="outline">superseded</Tag>;
  return <Tag appearance="outline">never activated</Tag>;
}

/**
 * Which nodes are on this revision, which are on another, and which have not
 * said.
 *
 * THREE ANSWERS, NOT TWO, and the third is the one this panel exists for: a
 * node that published no apply record is not a node that failed to apply, and
 * rendering it among the stragglers would send an operator after a machine
 * that is fine. It is the same three-valued rule `coord` is built on — held,
 * definitively not, unknown — read at the one place an operator acts on it.
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
  const here = nodes.filter((n) => (n.config_revision_id ?? "") === revisionId);
  const elsewhere = nodes.filter((n) => {
    const on = n.config_revision_id ?? "";
    return on !== "" && on !== revisionId;
  });
  const silent = nodes.filter((n) => (n.config_revision_id ?? "") === "");

  // A READ IN FLIGHT IS NOT A READ THAT FAILED, and for the second or two
  // before the lease table lands they render identically: without this the
  // panel opens by announcing that the fleet did not answer, and then quietly
  // contradicts itself.
  const body =
    loading && !answered ? (
      <Skeleton variant="text" rows={2} label="Loading" />
    ) : !answered ? (
      <p className="t-body muted">
        The lease table did not answer, so which nodes are on this revision is not known.
      </p>
    ) : nodes.length === 0 ? (
      <p className="t-body">
        No node holds a live lease. Nothing is running this configuration, or any other.
      </p>
    ) : (
      <div className="col gap-2">
        <p className="t-body">
          {here.length} of {plural(nodes.length, "node")} report this revision.
        </p>
        <div className="col" style={{ gap: 2 }}>
          {here.map((node) => (
            <NodeLine key={node.id} node={node} state="here" />
          ))}
          {elsewhere.map((node) => (
            <NodeLine key={node.id} node={node} state="elsewhere" />
          ))}
          {silent.map((node) => (
            <NodeLine key={node.id} node={node} state="silent" />
          ))}
        </div>
        {silent.length > 0 && (
          <p className="t-caption">
            A node that published no apply record has said nothing about which revision it holds —
            which is not the same as not holding this one.
          </p>
        )}
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
      <Card.Header icon={<DnsGlyph size="sm" />}>Across the fleet</Card.Header>
      {body}
    </Card>
  );
}

/** One node's answer about one revision. */
function NodeLine({ node, state }: { node: FleetNode; state: "here" | "elsewhere" | "silent" }) {
  const on = node.config_revision_id ?? "";
  return (
    <div className="row gap-2">
      <InlineCode>{node.id}</InlineCode>
      {state === "here" ? (
        <Tag variant="success">applied</Tag>
      ) : state === "elsewhere" ? (
        <Tag variant="warning" title={on}>
          on {on.slice(0, 10)}
        </Tag>
      ) : (
        <Tag appearance="outline">not saying</Tag>
      )}
      {/* THE APPLY THAT FAILED. A node can report this revision and have
          refused it — `config_status` is the other half of the record, and a
          list that showed only the id would call that node converged. */}
      {node.config_status === "error" && (
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
  const provenance = (
    <PropertiesRail
      groups={[
        {
          properties: [
            { label: "Revision", value: revision.revision_id, code: true },
            {
              label: "Parent",
              value: revision.parent_revision_id,
              code: true,
              title: "the revision this one was written against",
            },
            { label: "Source", value: revision.source },
            { label: "Created by", value: revision.created_by || undefined },
            { label: "Created", value: fmtDateTime(revision.created_at) },
            {
              label: "Activated",
              value: revision.activated_at ? fmtDateTime(revision.activated_at) : undefined,
              title: "a revision can be stored and never activated; activation is its own gesture",
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
          <Card.Header icon={<DescriptionGlyph size="sm" />}>Where it came from</Card.Header>
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
  const now = useNow();
  const audit = useQuery("config_audit", { limit: HISTORY_LIMIT }, { enabled: id !== "" });
  const fleet = useQuery("fleet", undefined, { enabled: id !== "", pollMs: FLEET_POLL_MS });

  const revision = useMemo(
    () => (audit.data ?? []).find((r) => r.revision_id === id) ?? null,
    [audit.data, id],
  );

  return (
    <>
      {audit.loading && !audit.data && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState error={audit.error} loading={audit.loading}>
        {/* NOT AN EMPTY RAIL. A `peek=revision:` arrives from a pasted URL as
            often as from a row, and naming the id that resolved to nothing —
            and the window it was looked for in — is more use than a header
            over no revision. */}
        {audit.data && !revision && (
          <EmptyState
            size="compact"
            icon={<ScheduleGlyph size="xl" />}
            title="No such revision in the recent history"
            description={`The last ${HISTORY_LIMIT} revisions were read and none of them is this one. It may be older than that window, or the id may be wrong.`}
          />
        )}
        {revision && (
          <>
            <ObjectHeader
              size="peek"
              kind="Revision"
              icon="description"
              identifier={revision.revision_id.slice(0, 10)}
              title={revision.summary || "No summary was written"}
              status={<RevisionState revision={revision} />}
              facts={revisionFacts(revision, now)}
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

export function ConfigScreen({ revision: revisionPath }: { revision?: string }) {
  const now = useNow();
  const [lens, setLens] = useTab("lens", LENSES);
  const [chosen, setRevision] = useParam("revision", "");
  // THE SIDE A DIFF IS READ AGAINST. Empty is the active revision, which is
  // what the engine's own `against: "active"` means, so the parameter is
  // absent from every link that wants the ordinary comparison. A FILTER rather
  // than a section: it narrows what is already on screen, so it replaces the
  // history entry instead of adding one to press Back through.
  const [against, setAgainst] = useParam("against", "", "filter");

  const [kind, setKind] = useParam("kind", ENTITY_KINDS[0].kind);
  const [entity, setEntity] = useParam("entity", "");

  // `#/admin/config/revisions/{id}` ADDRESSES ONE REVISION, and this screen
  // accepted the segment and dropped it: a reader who followed a link to one
  // revision got the Active lens with nothing selected and no sign of what
  // they had asked for — and it is where `Open ↗` from the rail lands. The
  // query key still wins where a reader picked a row, so the path is the
  // landing rather than a lock.
  const revision = chosen || revisionPath || "";

  const active = useQuery("config", undefined, { enabled: lens === "active" });
  // ONE COLLECTION AT A TIME, which is what makes this different from the
  // Active lens rather than a second copy of it: the whole document is one
  // unreadable block of JSON, and the question a reader actually has is
  // "what does THIS seat's configuration say".
  const ids = useQuery("config_entities", { kind }, { enabled: lens === "entities" });
  const one = useQuery(
    "config_entities",
    { kind, id: entity },
    { enabled: lens === "entities" && entity !== "" },
  );
  const audit = useQuery(
    "config_audit",
    { limit: HISTORY_LIMIT },
    {
      // THE HISTORY IS ALSO WHAT THE ADDRESSED REVISION IS READ FROM, so the
      // block below has something to render on the lens a link lands on
      // rather than only on the two that show the table.
      enabled: lens === "audit" || lens === "diff" || revisionPath !== undefined,
    },
  );
  const diff = useQuery(
    "config_diff",
    { revision_id: revision, against: against || "active" },
    { enabled: lens === "diff" && !!revision },
  );
  const fleet = useQuery("fleet", undefined, {
    enabled: revisionPath !== undefined,
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

  /**
   * A row's click.
   *
   * IT ALSO POINTS THE DIFF. Picking a revision in this table has always meant
   * "compare this one", and a peek that left the Diff lens on whatever was
   * selected last would make the two halves of the screen describe different
   * revisions. The lens itself is not changed: a reader who is reading the
   * history asked for the history.
   *
   * THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's one copy
   * of which clicks mean elsewhere and reads a mouse event; the `enter` chord
   * carries no button at all and is never "open elsewhere".
   */
  const openRevision = useCallback(
    (row: RevisionMeta, e: React.MouseEvent | React.KeyboardEvent) => {
      const go = () => {
        setRevision(row.revision_id);
        // AND DROPS THE SIDE A LINK NAMED. A row picked here is "compare this
        // one with what we are running"; carrying somebody else's `against=`
        // into it would answer a question this reader did not ask.
        setAgainst("");
        openPeek({ kind: "revision", id: row.revision_id });
      };
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
    },
    [openPeek, setRevision, setAgainst],
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
      <PageActions>
        {
          <Segmented<Lens>
            ariaLabel="Configuration view"
            value={lens}
            onChange={setLens}
            options={[
              { value: "active", label: "Active", icon: "description" },
              { value: "entities", label: "Entities", icon: "layers" },
              { value: "audit", label: "History", icon: "schedule" },
              { value: "diff", label: "Diff", icon: "fork_right" },
            ]}
          />
        }
      </PageActions>
      <PageNote>
        The founder-owned company document, versioned in the store and applied live. Secrets are
        redacted by the engine before it leaves the process.
      </PageNote>

      {/* THE REVISION THIS PATH NAMES, above the lens rather than below it:
          the reader followed a link to one revision, so what it is has to be
          the first thing on the screen and the lens they land on is a second
          question. */}
      {revisionPath !== undefined && (
        <>
          {audit.loading && !audit.data && <Skeleton variant="text" rows={4} label="Loading" />}
          <QueryState error={audit.error} loading={audit.loading}>
            {audit.data && !addressed && (
              <EmptyState
                icon={<ScheduleGlyph size="xl" />}
                title="No such revision in the recent history"
                description={`The last ${HISTORY_LIMIT} revisions were read and none of them is this one. It may be older than that window, or the id may be wrong.`}
              />
            )}
            {addressed && (
              <>
                <ObjectHeader
                  kind="Revision"
                  icon="description"
                  identifier={addressed.revision_id.slice(0, 10)}
                  title={addressed.summary || "No summary was written"}
                  status={<RevisionState revision={addressed} />}
                  facts={revisionFacts(addressed, now)}
                  actions={
                    <Button
                      size="small"
                      leadingIcon={<ForkRightGlyph size="sm" />}
                      onClick={() => {
                        setRevision(addressed.revision_id);
                        setAgainst("");
                        setLens("diff");
                      }}
                    >
                      See its changes
                    </Button>
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

      {lens === "active" && (
        <>
          {active.loading && <Skeleton variant="text" rows={6} label="Loading" />}
          <QueryState error={active.error} loading={active.loading}>
            {active.data ? (
              <Card>
                <Card.Header
                  icon={<DescriptionGlyph size="sm" />}
                  subtitle="as the engine resolved it"
                  actions={<CopyButton text={pretty} title="the active revision, as JSON" />}
                >
                  Active revision
                </Card.Header>
                <div className="col gap-1">
                  {/* BOUNDED, like every other record block. A whole company
                      configuration runs to hundreds of lines, and it is one of
                      the two records `RECORD_MAX_HEIGHT` is written down for
                      by name — unbounded, it pushes the caption under it and
                      the entity lens below that off the screen. */}
                  <CodeBlock
                    plain
                    maxHeight={RECORD_MAX_HEIGHT}
                    selectable
                    label="The active company configuration, as JSON"
                    code={pretty}
                  />
                  <span className="t-caption">
                    Click into the revision, and ⌘A / Ctrl+A selects it alone rather than the page.
                  </span>
                </div>
              </Card>
            ) : (
              <EmptyState
                icon={<TuneGlyph size="xl" />}
                title={NO_REVISION.title}
                description={NO_REVISION.hint}
              />
            )}
          </QueryState>
        </>
      )}

      {lens === "entities" && (
        <>
          <Segmented<string>
            ariaLabel="Which collection"
            value={kind}
            onChange={(next) => {
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
            <div className="row gap-3 wrap" style={{ alignItems: "flex-start" }}>
              <Card padding="none">
                <Card.Header icon={<LayersGlyph size="sm" />} count={(ids.data?.ids ?? []).length}>
                  {ENTITY_KINDS.find((k) => k.kind === kind)?.label ?? kind}
                </Card.Header>
                <div className="col">
                  {(ids.data?.ids ?? []).map((id) => (
                    <Button
                      key={id}
                      variant={id === entity ? "primary" : "tertiary"}
                      size="small"
                      onClick={() => setEntity(id === entity ? "" : id)}
                    >
                      <InlineCode>{id}</InlineCode>
                    </Button>
                  ))}
                </div>
              </Card>
              <Card>
                <Card.Header
                  icon={<DescriptionGlyph size="sm" />}
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
                    description="Its own slice of the active document is shown here, rather than the whole thing."
                  />
                ) : one.loading && !one.data ? (
                  <Skeleton variant="text" rows={6} label="Loading" />
                ) : (
                  <QueryState error={one.error} loading={one.loading}>
                    {one.data ? (
                      <CodeBlock
                        plain
                        maxHeight={RECORD_MAX_HEIGHT}
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
                        icon={<TuneGlyph size="xl" />}
                        title={NO_REVISION.title}
                        description="There is no active revision for this entity to be declared in."
                      />
                    )}
                  </QueryState>
                )}
              </Card>
            </div>
          </QueryState>
        </>
      )}

      {(lens === "audit" || lens === "diff") && (
        <>
          {audit.loading && <Skeleton variant="text" rows={5} label="Loading" />}
          <QueryState
            error={audit.error}
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
              <Card.Header icon={<ScheduleGlyph size="sm" />} count={rows.length}>
                Revisions
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
                isSelected={(r) => r.revision_id === revision}
                columns={[
                  {
                    key: "at",
                    header: "When",
                    shrink: true,
                    sortValue: (r) => tsKey(r.created_at),
                    cell: (r) => <DateCell at={r.created_at} now={now} />,
                  },
                  {
                    key: "id",
                    header: "Revision",
                    cell: (r) => (
                      <span className="row gap-1">
                        <KeyCell value={r.revision_id.slice(0, 10)} />
                        {r.is_active && <Tag variant="success">active</Tag>}
                      </span>
                    ),
                  },
                  {
                    key: "summary",
                    header: "Summary",
                    sortValue: (r) => r.summary,
                    // NOT `value || "—"`. A revision imported without a
                    // message has no summary, and the dash says so rather
                    // than standing in for an empty string.
                    cell: (r) =>
                      r.summary ? (
                        <TextCell>{r.summary}</TextCell>
                      ) : (
                        <EmptyValue label="No summary was written" />
                      ),
                  },
                  {
                    key: "author",
                    header: "By",
                    shrink: true,
                    sortValue: (r) => r.created_by,
                    cell: (r) =>
                      r.created_by ? (
                        <TextCell>{r.created_by}</TextCell>
                      ) : (
                        <EmptyValue label="Nobody recorded" />
                      ),
                  },
                ]}
              />
            </Card>
          </QueryState>

          {lens === "diff" && (
            <Card>
              <Card.Header
                icon={<ForkRightGlyph size="sm" />}
                subtitle={
                  against
                    ? `against revision ${against.slice(0, 10)}`
                    : "against the active revision"
                }
              >
                {revision ? `Changes in ${revision.slice(0, 10)}` : "Diff"}
              </Card.Header>
              {!revision ? (
                <EmptyState
                  size="compact"
                  icon={<ForkRightGlyph size="xl" />}
                  title="Pick a revision above"
                  description="Its differences against the currently active document are shown here."
                />
              ) : diff.loading ? (
                <Skeleton variant="text" rows={4} label="Loading" />
              ) : (
                <QueryState
                  error={diff.error}
                  loading={diff.loading}
                  empty={
                    diff.data?.changes?.length
                      ? undefined
                      : {
                          title: "No differences",
                          hint: "This revision is byte-identical to the active one. Re-activating an unchanged revision is the credential-rotation gesture.",
                        }
                  }
                >
                  <div className="col" style={{ gap: 2 }}>
                    {(diff.data?.changes ?? []).map((c, i) => (
                      <div key={i} className="diff-line" data-kind={c.kind}>
                        <span>{c.kind === "added" ? "+" : c.kind === "removed" ? "−" : "~"}</span>
                        <span className="truncate">{c.path}</span>
                        <span className="truncate">
                          {c.kind === "added"
                            ? JSON.stringify(c.to)
                            : c.kind === "removed"
                              ? JSON.stringify(c.from)
                              : `${JSON.stringify(c.from)} → ${JSON.stringify(c.to)}`}
                        </span>
                      </div>
                    ))}
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
                      <div className="t-caption" style={{ paddingTop: 6 }}>
                        {diff.data?.changes.length} of {diff.data?.changes_total} shown —{" "}
                        <InlineCode>crewlet config diff</InlineCode> prints them all
                      </div>
                    )}
                  </div>
                </QueryState>
              )}
            </Card>
          )}
        </>
      )}

      <Callout variant="neutral">
        <span className="col" style={{ gap: 4 }}>
          <span>
            This screen reads. Writing a revision is <InlineCode>PUT /config</InlineCode> or{" "}
            <InlineCode>crewlet config import</InlineCode>, which validate against the generated
            schema before anything is stored.
          </span>
          <span className="t-caption">
            An activation is a compare-and-set on a shared pointer, so two operators cannot
            overwrite each other; each node then reconciles onto the new epoch on its own tick.
          </span>
        </span>
      </Callout>
    </>
  );
}
