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
import { ScreenHead } from "~/app/Shell.tsx";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Button, Code, Empty, Panel, Segmented, Skeleton } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { RevisionMeta } from "~/protocol/index.ts";

type Lens = "active" | "audit" | "diff";

export function ConfigScreen() {
  const now = useNow();
  const [lens, setLens] = useParam("lens", "active", "section");
  const [revision, setRevision] = useParam("revision", "");

  const active = useQuery("config", undefined, { enabled: lens === "active" });
  const audit = useQuery("config_audit", { limit: 100 }, { enabled: lens !== "active" });
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
      <ScreenHead
        title="Configuration"
        sub="The founder-owned company document, versioned in the store and applied live. Secrets are redacted by the engine before it leaves the process."
        actions={
          <Segmented<Lens>
            ariaLabel="Configuration view"
            value={lens as Lens}
            onChange={setLens}
            options={[
              { value: "active", label: "Active", icon: "file" },
              { value: "audit", label: "History", icon: "clock" },
              { value: "diff", label: "Diff", icon: "gitBranch" },
            ]}
          />
        }
      />

      {lens === "active" && (
        <>
          {active.loading && <Skeleton rows={6} />}
          <QueryState error={active.error} loading={active.loading}>
            {active.data ? (
              <Panel
                title="Active revision"
                icon="file"
                subtitle="as the engine resolved it"
                actions={
                  <Button
                    size="sm"
                    icon="copy"
                    onClick={() => void navigator.clipboard?.writeText(pretty)}
                  >
                    Copy
                  </Button>
                }
              >
                <Code plain>{pretty}</Code>
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

      {lens !== "active" && (
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
              <DataTable<RevisionMeta>
                rows={audit.data ?? []}
                rowKey={(r) => r.id}
                defaultSort={{ key: "at", dir: "desc" }}
                onRowClick={(r) => {
                  setRevision(r.id);
                  setLens("diff");
                }}
                isSelected={(r) => r.id === revision}
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
                        <code className="inline">{r.id.slice(0, 10)}</code>
                        {r.active && <Badge tone="positive">active</Badge>}
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
                    sortValue: (r) => r.author,
                    cell: (r) => r.author || <span className="faint">—</span>,
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
                      <div key={i} className="diff-line" data-op={c.op}>
                        <span>{c.op === "add" ? "+" : c.op === "remove" ? "−" : "~"}</span>
                        <span className="truncate">{c.path}</span>
                        <span className="truncate">
                          {c.op === "add"
                            ? JSON.stringify(c.to)
                            : c.op === "remove"
                              ? JSON.stringify(c.from)
                              : `${JSON.stringify(c.from)} → ${JSON.stringify(c.to)}`}
                        </span>
                      </div>
                    ))}
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
