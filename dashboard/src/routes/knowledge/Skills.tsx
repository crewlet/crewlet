/**
 * Agent skills (`#/knowledge/skills`): what the engine teaches its agents, and
 * what they taught themselves.
 *
 * # Two kinds, one address
 *
 * `kind=pages` (the default) is the TOOL SKILLS — pages in the knowledge base
 * the engine offers a phase, one line each in its catalogue, and whose body a
 * seat loads when it wants the whole procedure. Each row says who they reached
 * over thirty company days, from BOTH ways a skill does (`skill_loaded_by`:
 * loaded by the seat, offered in a phase's catalogue), answered with the
 * listing in one read rather than per row.
 *
 * `kind=learned` is what one seat DREW from its own repeated work, answered by
 * the node holding that seat (`agent_memory`), with "n of total" — the page is
 * cut at the answer's limit and the total is the seat's own.
 *
 * # Counts are totals
 *
 * Every number here is the engine's total, never the length of what one window
 * carried: a catalogue of six hundred skills says six hundred with the first
 * five hundred drawn and "Load more", and a seat with forty learned skills
 * says forty over the twenty-five listed.
 */

import { useMemo } from "react";
import { AvatarStack, Button, Card, EmptyState, EmptyValue, Select } from "@crewlethq/ui";
import { WandSparklesGlyph } from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, NumberCell, TextCell } from "~/app/frame/cells.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, tsKey } from "~/lib/format.ts";
import { loadedBy } from "~/lib/pageReads.ts";
import { seatBadge } from "~/ui/SeatAvatar.tsx";
import { Segmented } from "~/ui/primitives.tsx";
import type { SkillLoad } from "~/contract/pages.ts";
import type { PageSummary } from "~/protocol/index.ts";
import { usePagedPages } from "./usePagedPages.ts";

/** The tool-skill pages: published, and only skills. */
const SKILL_PAGES = { skills: true, status: "published" } as const;

export function Skills() {
  const [kind, setKind] = useParam("kind", "pages");
  return (
    <>
      <PageNote>
        Tool skills are pages the engine offers an agent&rsquo;s phases; learned skills are what an
        agent drew from its own repeated work.
      </PageNote>
      <div className="toolbar">
        <Segmented
          ariaLabel="Which skills"
          value={kind}
          onChange={setKind}
          options={[
            { value: "pages", label: "Tool skills", title: "Pages the engine offers a phase" },
            { value: "learned", label: "Learned", title: "What one agent drew from its own work" },
          ]}
        />
      </div>
      {kind === "learned" ? <LearnedSkills /> : <ToolSkills />}
    </>
  );
}

function ToolSkills() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const listing = usePagedPages(SKILL_PAGES, { pollMs: 60_000 });
  const rows = useMemo(
    () => [...listing.rows].sort((a, b) => a.title.localeCompare(b.title)),
    [listing.rows],
  );
  const name = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const kindOf = (handle: string) => index.byHandle.get(handle)?.kind;
  const loads = (row: PageSummary): SkillLoad[] | null =>
    listing.loadedBy ? (listing.loadedBy[row.id] ?? []) : null;

  return (
    <QueryState
      error={listing.error}
      refusal={listing.refusal}
      loading={listing.loading}
      empty={
        rows.length
          ? undefined
          : {
              title: "No tool skills yet",
              hint: "A tool skill is a page in the skills space whose front matter names the tool it teaches. The engine offers each one to the phases that can call that tool.",
            }
      }
    >
      <Card>
        <Card.Header icon={<WandSparklesGlyph size="sm" />} count={listing.total ?? rows.length}>
          <Card.Title>Tool skills</Card.Title>
        </Card.Header>
        <DataGrid<PageSummary>
          rows={rows}
          rowKey={(r) => r.id}
          defaultSort="title"
          rowHref={(r) => href(["knowledge", "pages", r.id])}
          columns={[
            {
              key: "title",
              header: "Skill",
              sortValue: (r) => r.title,
              cell: (r) => <TextCell icon="wand-sparkles">{r.title}</TextCell>,
            },
            {
              key: "loaded",
              header: "Loaded by, 30 days",
              sortValue: (r) => (loads(r) ?? []).reduce((n, l) => n + l.loaded, 0),
              cell: (r) => {
                const l = loads(r);
                if (l === null) return <span className="muted">Not recorded on this node</span>;
                const said = loadedBy(l, name);
                if (!said) return <span className="muted">Nobody</span>;
                const who = l.filter((x) => (said.verb === "Loaded" ? x.loaded : x.offered) > 0);
                return (
                  <span className="row gap-2" title={who.map((x) => name(x.handle)).join(", ")}>
                    <AvatarStack
                      size="xs"
                      max={3}
                      decorative
                      members={who.map((x) => ({
                        id: x.handle,
                        ...seatBadge(name(x.handle), kindOf(x.handle)),
                      }))}
                    />
                    <span className="truncate">
                      {said.verb === "Loaded" ? "" : "offered to "}
                      {said.names}
                      {said.more ? ` +${said.more}` : ""}
                    </span>
                  </span>
                );
              },
            },
            {
              key: "loads",
              header: "Loads",
              shrink: true,
              align: "right",
              sortValue: (r) => (loads(r) ?? []).reduce((n, l) => n + l.loaded, 0),
              cell: (r) => {
                const l = loads(r);
                return l === null ? (
                  <EmptyValue label="Not recorded on this node" />
                ) : (
                  <NumberCell value={l.reduce((n, x) => n + x.loaded, 0)} />
                );
              },
            },
            {
              key: "updated",
              header: "Updated",
              shrink: true,
              align: "right",
              sortValue: (r) => tsKey(r.updated_at),
              cell: (r) => <DateCell at={r.updated_at} now={now} />,
            },
          ]}
        />
        {listing.more && (
          <Card.Footer variant="meta">
            <span className="row wrap gap-2">
              <span>
                {rows.length.toLocaleString()} of {(listing.total ?? 0).toLocaleString()} skills
                loaded.
              </span>
              <Button
                size="small"
                variant="secondary"
                onClick={listing.loadMore}
                loading={listing.paging}
              >
                Load more
              </Button>
            </span>
          </Card.Footer>
        )}
      </Card>
    </QueryState>
  );
}

function LearnedSkills() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  // EVERY AGENT SEAT — a person learns no skills the engine keeps.
  const agents = useMemo(
    () =>
      index.seats
        .filter((s) => s.kind !== "human" && s.handle)
        .sort((a, b) => a.name.localeCompare(b.name)),
    [index],
  );
  const [picked, setPicked] = useParam("seat", "");
  const seat = picked || agents[0]?.handle || "";
  const memory = useQuery("agent_memory", { id: seat }, { enabled: seat !== "", pollMs: 60_000 });
  const skills = memory.data?.skills ?? [];
  const total = memory.data?.skills_total ?? 0;
  const who = index.byHandle.get(seat);

  if (agents.length === 0) {
    return (
      <EmptyState
        icon={<WandSparklesGlyph size="xl" />}
        title="No agent seats"
        description="Learned skills belong to agents, and this company's chart has none."
      />
    );
  }
  return (
    <Card>
      <Card.Header icon={<WandSparklesGlyph size="sm" />}>
        <Card.Title>Learned by</Card.Title>
        <Select
          width="auto"
          ariaLabel="Whose skills"
          value={seat}
          onChange={(v) => setPicked(String(v))}
          options={agents.map((a) => ({ value: a.handle, label: a.name }))}
        />
      </Card.Header>
      <QueryState
        error={memory.error}
        refusal={memory.refusal}
        loading={memory.loading}
        empty={
          skills.length
            ? undefined
            : {
                title: `${who?.name ?? seat} has learned no skills yet`,
                hint: "A skill is drafted after the same kind of work goes well more than once. Its episodes are on the agent's memory tab.",
              }
        }
      >
        <DataGrid
          rows={skills}
          rowKey={(r) => r.id}
          defaultSort="-uses"
          columns={[
            {
              key: "title",
              header: "Skill",
              sortValue: (r) => r.title,
              cell: (r) => (
                <span className="col">
                  <span>{r.title || r.key}</span>
                  {r.summary && <span className="t-caption truncate">{r.summary}</span>}
                </span>
              ),
            },
            {
              key: "uses",
              header: "Uses",
              shrink: true,
              align: "right",
              sortValue: (r) => r.uses,
              cell: (r) => <NumberCell value={r.uses} />,
            },
            {
              key: "version",
              header: "Version",
              shrink: true,
              align: "right",
              sortValue: (r) => r.version,
              cell: (r) => <NumberCell value={r.version} />,
            },
            {
              key: "updated",
              header: "Updated",
              shrink: true,
              align: "right",
              sortValue: (r) => tsKey(r.updated_at),
              cell: (r) => <DateCell at={r.updated_at} now={now} />,
            },
          ]}
        />
        <Card.Footer variant="meta">
          <span className="row wrap gap-2">
            <span>
              {skills.length < total
                ? `${skills.length} of ${plural(total, "learned skill")}.`
                : plural(total, "learned skill")}
            </span>
            {memory.data?.held_by && memory.data.held_by !== "none" && (
              <span>Answered by {memory.data.held_by}, the node holding the seat.</span>
            )}
            <a className="t-link" href={href(["agents", "seats", seat], { tab: "memory" })}>
              {who?.name ?? seat}&rsquo;s memory →
            </a>
          </span>
        </Card.Footer>
      </QueryState>
    </Card>
  );
}
