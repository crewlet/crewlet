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

import { useMemo } from "react";
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
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { RevisionMeta } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useTab } from "~/app/frame/tabs.ts";

const LENSES = ["active", "entities", "audit", "diff"] as const;
type Lens = (typeof LENSES)[number];

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

export function ConfigScreen({ revision: revisionPath }: { revision?: string }) {
  const now = useNow();
  const [lens, setLens] = useTab("lens", LENSES);
  const [revision, setRevision] = useParam("revision", "");

  const [kind, setKind] = useParam("kind", ENTITY_KINDS[0].kind);
  const [entity, setEntity] = useParam("entity", "");

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
    { limit: 100 },
    {
      enabled: lens === "audit" || lens === "diff",
    },
  );
  const diff = useQuery(
    "config_diff",
    { revision_id: revision, against: "active" },
    { enabled: lens === "diff" && !!revision },
  );

  const pretty = useMemo(
    () => (active.data ? JSON.stringify(active.data, null, 2) : ""),
    [active.data],
  );

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
              (audit.data ?? []).length
                ? undefined
                : {
                    title: "No revisions recorded",
                    hint: "The history begins with the first import.",
                  }
            }
          >
            <Panel title="Revisions" icon="clock" count={(audit.data ?? []).length} padding="none">
              <DataGrid<RevisionMeta>
                rows={audit.data ?? []}
                rowKey={(r) => r.revision_id}
                defaultSort="-at"
                onRowActivate={(r) => {
                  setRevision(r.revision_id);
                  setLens("diff");
                }}
                isSelected={(r) => r.revision_id === revision}
                columns={[
                  {
                    key: "at",
                    header: "When",
                    shrink: true,
                    sortValue: (r) => tsKey(r.created_at),
                    cell: (r) => (
                      <span className="t-caption" title={fmtDateTime(r.created_at)}>
                        {relTime(r.created_at, now)}
                      </span>
                    ),
                  },
                  {
                    key: "id",
                    header: "Revision",
                    cell: (r) => (
                      <span className="row gap-1">
                        <code className="inline">{r.revision_id.slice(0, 10)}</code>
                        {r.is_active && <Badge tone="positive">active</Badge>}
                      </span>
                    ),
                  },
                  {
                    key: "summary",
                    header: "Summary",
                    sortValue: (r) => r.summary,
                    cell: (r) => (
                      <span className="truncate">
                        {r.summary || <span className="faint">—</span>}
                      </span>
                    ),
                  },
                  {
                    key: "author",
                    header: "By",
                    shrink: true,
                    sortValue: (r) => r.created_by,
                    cell: (r) => r.created_by || <span className="faint">—</span>,
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
                        <span>
                          {c.kind === "added" ? "+" : c.kind === "removed" ? "\u2212" : "~"}
                        </span>
                        <span className="truncate">{c.path}</span>
                        <span className="truncate">
                          {c.kind === "added"
                            ? JSON.stringify(c.to)
                            : c.kind === "removed"
                              ? JSON.stringify(c.from)
                              : `${JSON.stringify(c.from)} \u2192 ${JSON.stringify(c.to)}`}
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
