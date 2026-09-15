/**
 * Configuration: what the fleet is running, how it got here, and what changed.
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
import { QueryState } from "~/components/common.tsx";
import {
  Badge,
  Button,
  Code,
  CopyButton,
  Empty,
  Panel,
  Segmented,
  Skeleton,
} from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { Dash, DateCell, KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { FleetNode, RevisionMeta } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useTab } from "~/app/frame/tabs.ts";

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
 * The addressable collections of the active revision, as `configapi` names
 * them.
 *
 * FOUR NAMES THE ENGINE OWNS, held against `configapi.EntityKinds()` by
 * `internal/api/configapi/entities_client_test.go` — a kind this list spells
 * differently asks for a collection that does not exist, and the answer is a
 * bad-params refusal rather than anything a reader could act on.
 */
const ENTITY_KINDS = [
  { kind: "roles", label: "Seats" },
  { kind: "units", label: "Units" },
  { kind: "llm-providers", label: "LLM providers" },
  { kind: "mcp-servers", label: "MCP servers" },
] as const;

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
  if (revision.is_active) return <Badge tone="positive">active</Badge>;
  if (revision.activated_at) return <Badge outline>superseded</Badge>;
  return <Badge outline>never activated</Badge>;
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
      <Skeleton rows={2} />
    ) : !answered ? (
      <p className="t-body faint">
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
    <Panel title="Across the fleet" icon="server">
      {body}
    </Panel>
  );
}

/** One node's answer about one revision. */
function NodeLine({ node, state }: { node: FleetNode; state: "here" | "elsewhere" | "silent" }) {
  const on = node.config_revision_id ?? "";
  return (
    <div className="row gap-2">
      <code className="inline">{node.id}</code>
      {state === "here" ? (
        <Badge tone="positive">applied</Badge>
      ) : state === "elsewhere" ? (
        <Badge tone="caution" title={on}>
          on {on.slice(0, 10)}
        </Badge>
      ) : (
        <Badge outline>not saying</Badge>
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
        <Panel title="Where it came from" icon="file">
          {provenance}
        </Panel>
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
      {audit.loading && !audit.data && <Skeleton rows={6} />}
      <QueryState error={audit.error} loading={audit.loading}>
        {/* NOT AN EMPTY RAIL. A `peek=revision:` arrives from a pasted URL as
            often as from a row, and naming the id that resolved to nothing —
            and the window it was looked for in — is more use than a header
            over no revision. */}
        {audit.data && !revision && (
          <Empty
            inline
            icon="clock"
            title="No such revision in the recent history"
            hint={`The last ${HISTORY_LIMIT} revisions were read and none of them is this one. It may be older than that window, or the id may be wrong.`}
          />
        )}
        {revision && (
          <>
            <ObjectHeader
              size="peek"
              kind="Revision"
              icon="file"
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
    { revision_id: revision, against: "active" },
    { enabled: lens === "diff" && !!revision },
  );
  const fleet = useQuery("fleet", undefined, {
    enabled: revisionPath !== undefined,
    pollMs: FLEET_POLL_MS,
  });

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
        openPeek({ kind: "revision", id: row.revision_id });
      };
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
    },
    [openPeek, setRevision],
  );

  const pretty = useMemo(
    () => (active.data ? JSON.stringify(active.data, null, 2) : ""),
    [active.data],
  );

  const addressed = revisionPath
    ? (rows.find((r) => r.revision_id === revisionPath) ?? null)
    : null;

  return (
    <>
      <PageActions>
        {
          <Segmented<Lens>
            ariaLabel="Configuration view"
            value={lens}
            onChange={setLens}
            options={[
              { value: "active", label: "Active", icon: "file" },
              { value: "entities", label: "Entities", icon: "layers" },
              { value: "audit", label: "History", icon: "clock" },
              { value: "diff", label: "Diff", icon: "gitBranch" },
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
          {audit.loading && !audit.data && <Skeleton rows={4} />}
          <QueryState error={audit.error} loading={audit.loading}>
            {audit.data && !addressed && (
              <Empty
                icon="clock"
                title="No such revision in the recent history"
                hint={`The last ${HISTORY_LIMIT} revisions were read and none of them is this one. It may be older than that window, or the id may be wrong.`}
              />
            )}
            {addressed && (
              <>
                <ObjectHeader
                  kind="Revision"
                  icon="file"
                  identifier={addressed.revision_id.slice(0, 10)}
                  title={addressed.summary || "No summary was written"}
                  status={<RevisionState revision={addressed} />}
                  facts={revisionFacts(addressed, now)}
                  actions={
                    <Button
                      size="sm"
                      icon="gitBranch"
                      onClick={() => {
                        setRevision(addressed.revision_id);
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
          {active.loading && <Skeleton rows={6} />}
          <QueryState error={active.error} loading={active.loading}>
            {active.data ? (
              <Panel
                title="Active revision"
                icon="file"
                subtitle="as the engine resolved it"
                actions={<CopyButton text={pretty} title="the active revision, as JSON" />}
              >
                <div className="col gap-1">
                  <Code plain selectable label="The active company configuration, as JSON">
                    {pretty}
                  </Code>
                  <span className="t-caption">
                    Click into the revision, and ⌘A / Ctrl+A selects it alone rather than the page.
                  </span>
                </div>
              </Panel>
            ) : (
              <Empty
                icon="sliders"
                title="No company configuration is active"
                hint="The engine is running with nothing to run: no seats are spawned and every inbound webhook is dropped. Import one with crewlet config import, or PUT /config."
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
          {ids.loading && !ids.data && <Skeleton rows={4} />}
          <QueryState
            error={ids.error}
            loading={ids.loading}
            empty={
              (ids.data?.ids ?? []).length
                ? undefined
                : {
                    title: "Nothing in this collection",
                    hint: "The active revision declares none of these.",
                  }
            }
          >
            <div className="row gap-3 wrap" style={{ alignItems: "flex-start" }}>
              <Panel
                title={ENTITY_KINDS.find((k) => k.kind === kind)?.label ?? kind}
                icon="layers"
                count={(ids.data?.ids ?? []).length}
                padding="none"
              >
                <div className="col">
                  {(ids.data?.ids ?? []).map((id) => (
                    <Button
                      key={id}
                      variant={id === entity ? "primary" : "ghost"}
                      size="sm"
                      onClick={() => setEntity(id === entity ? "" : id)}
                    >
                      <code className="inline">{id}</code>
                    </Button>
                  ))}
                </div>
              </Panel>
              <Panel
                title={entity || "Pick one"}
                icon="file"
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
                {!entity ? (
                  <Empty
                    inline
                    icon="layers"
                    title="Pick one from the list"
                    hint="Its own slice of the active document is shown here, rather than the whole thing."
                  />
                ) : one.loading && !one.data ? (
                  <Skeleton rows={6} />
                ) : (
                  <QueryState error={one.error} loading={one.loading}>
                    <Code plain selectable label={`${entity}, as JSON`}>
                      {JSON.stringify(one.data?.entity ?? null, null, 2)}
                    </Code>
                  </QueryState>
                )}
              </Panel>
            </div>
          </QueryState>
        </>
      )}

      {(lens === "audit" || lens === "diff") && (
        <>
          {audit.loading && <Skeleton rows={5} />}
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
            <Panel title="Revisions" icon="clock" count={rows.length} padding="none">
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
                        {r.is_active && <Badge tone="positive">active</Badge>}
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
                        <Dash title="no summary was written" />
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
                        <Dash title="nobody recorded" />
                      ),
                  },
                ]}
              />
            </Panel>
          </QueryState>

          {lens === "diff" && (
            <Panel
              title={revision ? `Changes in ${revision.slice(0, 10)}` : "Diff"}
              icon="gitBranch"
              subtitle="against the active revision"
            >
              {!revision ? (
                <Empty
                  inline
                  icon="gitBranch"
                  title="Pick a revision above"
                  hint="Its differences against the currently active document are shown here."
                />
              ) : diff.loading ? (
                <Skeleton rows={4} />
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
                      <div className="t-caption faint" style={{ paddingTop: 6 }}>
                        {diff.data?.changes.length} of {diff.data?.changes_total} shown —{" "}
                        <code className="inline">crewlet config diff</code> prints them all
                      </div>
                    )}
                  </div>
                </QueryState>
              )}
            </Panel>
          )}
        </>
      )}

      <div className="banner neutral">
        <Icon name="info" size="sm" />
        <span className="col" style={{ gap: 4 }}>
          <span>
            This screen reads. Writing a revision is <code className="inline">PUT /config</code> or{" "}
            <code className="inline">crewlet config import</code>, which validate against the
            generated schema before anything is stored.
          </span>
          <span className="t-caption">
            An activation is a compare-and-set on a shared pointer, so two operators cannot
            overwrite each other; each node then reconciles onto the new epoch on its own tick.
          </span>
        </span>
      </div>
    </>
  );
}
