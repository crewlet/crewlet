/**
 * Configuration: what the fleet is running, how it got here, and what changed.
 *
 * THIS SCREEN READS. It shows the active revision, the history of revisions
 * and the difference between any one of them and the active document, and it
 * writes nothing. The diff lens compares against the active revision unless
 * the link names another (`against=`): what one save changed is its revision
 * against its parent, because once that save is active its diff against the
 * active revision is empty. The previous editor on this screen was a bare JSON textarea
 * with no schema hints, no validation until Save, no diff before saving and a
 * dirty flag that was set and never read, so navigating away lost the edit
 * silently.
 *
 * WRITING HAPPENS WHERE THE THING BEING WRITTEN IS. The organization (its
 * units, seats, leads, reporting lines and charter) is created and edited in
 * the org chart's builder lens, `#/org?lens=builder`, which validates every
 * change against the engine before it saves. An integration is connected from
 * the Integrations screen and a credential is stored from Secrets. Anything
 * else is `PUT` or `PATCH /config`, or `crewlet config import`. So an empty
 * state here points at the builder, and names the command line beside it.
 */

import { useId, useMemo } from "react";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, RECORD_MAX_HEIGHT, recordTable } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, tsKey } from "~/lib/format.ts";
import type { RevisionMeta } from "~/protocol/index.ts";
import {
  ButtonLink,
  Callout,
  Card,
  CodeBlock,
  CopyButton,
  DataTable,
  EmptyState,
  EmptyValue,
  InlineCode,
  PageHeader,
  RelativeTime,
  SegmentedControl,
  Skeleton,
  TabPanel,
  Tag,
  useNow,
} from "@crewlethq/ui";
import {
  DescriptionGlyph,
  DifferenceGlyph,
  InfoGlyph,
  ManufacturingGlyph,
  ScheduleGlyph,
} from "@crewlethq/icons/glyphs";

type Lens = "active" | "audit" | "diff";

export function ConfigScreen() {
  const now = useNow();
  const [lens, setLens] = useParam("lens", "active", "section");
  const [revision] = useParam("revision", "");
  // THE SIDE A DIFF IS READ AGAINST. Empty is the active revision, which is
  // what a row picked from the history compares with. A link can name the
  // revision's parent instead, which is the only way to show what one save
  // changed: once that save is active, it is byte-identical to itself.
  const [against] = useParam("against", "");
  const nav = useNavigator();
  const panel = useId();

  const active = useQuery("config", undefined, { enabled: lens === "active" });
  const audit = useQuery("config_audit", { limit: 100 }, { enabled: lens !== "active" });
  const diff = useQuery(
    "config_diff",
    { revision_id: revision, against: against || "active" },
    { enabled: lens === "diff" && !!revision },
  );

  const pretty = useMemo(
    () => (active.data ? JSON.stringify(active.data, null, 2) : ""),
    [active.data],
  );

  return (
    <>
      <PageHeader
        title="Configuration"
        description="The founder-owned company document, versioned in the store and applied live. Secrets are redacted by the engine before it leaves the process."
        actions={
          <SegmentedControl<Lens>
            label="Configuration view"
            semantics="tabs"
            panelId={panel}
            value={lens as Lens}
            onValueChange={setLens}
            options={[
              { value: "active", label: "Active", icon: <DescriptionGlyph /> },
              { value: "audit", label: "History", icon: <ScheduleGlyph /> },
              { value: "diff", label: "Diff", icon: <DifferenceGlyph /> },
            ]}
          />
        }
      />

      <TabPanel id={panel} value={lens}>
        {lens === "active" && (
          <>
            {active.loading && (
              <Skeleton label="Loading the configuration" variant="text" rows={6} />
            )}
            <QueryState error={active.error} loading={active.loading}>
              {active.data ? (
                <Card as="section">
                  <Card.Header
                    icon={<DescriptionGlyph size="sm" />}
                    subtitle="as the engine resolved it"
                    actions={
                      <CopyButton
                        variant="secondary"
                        text={pretty}
                        title="the active revision, as JSON"
                      />
                    }
                  >
                    <Card.Title>Active revision</Card.Title>
                  </Card.Header>
                  <div className="col gap-1">
                    <CodeBlock
                      plain
                      wrap
                      selectable
                      label="The active company configuration, as JSON"
                      code={pretty}
                      maxHeight={RECORD_MAX_HEIGHT}
                    />
                    <span className="t-caption">
                      Click into the revision, and ⌘A / Ctrl+A selects it alone rather than the
                      page.
                    </span>
                  </div>
                </Card>
              ) : (
                <EmptyState
                  icon={<ManufacturingGlyph />}
                  title="No company configuration is active"
                  description="The engine is running with nothing to run: no seats are spawned and every inbound webhook is dropped. Create the company from the org chart, or import one with crewlet config import or PUT /config."
                  action={
                    <ButtonLink variant="primary" href={href(["org"], { lens: "builder" })}>
                      Create the company
                    </ButtonLink>
                  }
                />
              )}
            </QueryState>
          </>
        )}

        {lens !== "active" && (
          <>
            {audit.loading && (
              <Skeleton label="Loading the revision history" variant="text" rows={5} />
            )}
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
              <Card as="section" padding="none">
                <Card.Header
                  divided
                  style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
                  icon={<ScheduleGlyph size="sm" />}
                  count={(audit.data ?? []).length}
                >
                  <Card.Title>Revisions</Card.Title>
                </Card.Header>
                <DataTable
                  getRowKey={(r) => r.revision_id}
                  defaultSort={{ key: "at", direction: "desc" }}
                  onRowClick={(r) => {
                    // A picked row is compared with the active revision, so
                    // the revision a link compared against is dropped with it.
                    nav.filter({ revision: r.revision_id, against: null });
                    setLens("diff");
                  }}
                  isSelected={(r) => r.revision_id === revision}
                  {...recordTable(audit.data ?? [], [
                    {
                      key: "at",
                      header: "When",
                      shrink: true,
                      sortable: true,
                      firstDirection: "desc",
                      sortValue: (r) => tsKey(r.created_at),
                      render: (r) => (
                        <RelativeTime className="t-caption" value={r.created_at} now={now} />
                      ),
                    },
                    {
                      key: "id",
                      header: "Revision",
                      render: (r) => (
                        <span className="row gap-1">
                          <InlineCode>{r.revision_id.slice(0, 10)}</InlineCode>
                          {r.is_active && <Tag variant="success">active</Tag>}
                        </span>
                      ),
                    },
                    {
                      key: "summary",
                      header: "Summary",
                      sortable: true,
                      sortValue: (r) => r.summary,
                      render: (r) => (
                        <span className="truncate">
                          {r.summary || <EmptyValue label="No summary" />}
                        </span>
                      ),
                    },
                    {
                      key: "author",
                      header: "By",
                      shrink: true,
                      sortable: true,
                      sortValue: (r) => r.created_by,
                      render: (r) => r.created_by || <EmptyValue label="Not recorded" />,
                    },
                  ])}
                />
              </Card>
            </QueryState>

            {lens === "diff" && (
              <Card as="section">
                <Card.Header
                  icon={<DifferenceGlyph size="sm" />}
                  subtitle={
                    against
                      ? `against revision ${against.slice(0, 10)}`
                      : "against the active revision"
                  }
                >
                  <Card.Title>
                    {revision ? `Changes in ${revision.slice(0, 10)}` : "Diff"}
                  </Card.Title>
                </Card.Header>
                {!revision ? (
                  <EmptyState
                    size="compact"
                    icon={<DifferenceGlyph />}
                    title="Pick a revision above"
                    description="Its differences against the currently active document are shown here."
                  />
                ) : diff.loading ? (
                  <Skeleton label="Loading the differences" variant="text" rows={4} />
                ) : (
                  <QueryState
                    error={diff.error}
                    loading={diff.loading}
                    empty={
                      diff.data?.changes?.length
                        ? undefined
                        : {
                            title: "No differences",
                            hint: against
                              ? "The two revisions are byte-identical. Re-activating an unchanged revision is the credential-rotation gesture."
                              : "This revision is byte-identical to the active one. Re-activating an unchanged revision is the credential-rotation gesture.",
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
                    </div>
                  </QueryState>
                )}
              </Card>
            )}
          </>
        )}
      </TabPanel>

      <Callout variant="neutral" icon={<InfoGlyph size="sm" />}>
        <span className="col" style={{ gap: 4 }}>
          <span>
            This screen reads. The organization is created and edited from the{" "}
            <a className="t-link" href={href(["org"], { lens: "builder" })}>
              org chart
            </a>
            ; any other revision is <InlineCode>PUT /config</InlineCode>,{" "}
            <InlineCode>PATCH /config</InlineCode> or <InlineCode>crewlet config import</InlineCode>
            , each validated against the generated schema before anything is stored.
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
