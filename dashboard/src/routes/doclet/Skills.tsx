/**
 * Agent skills (`#/doclet/skills`): what the engine teaches its agents, and
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
 * cut at the answer's limit and the total is the seat's own. A seat's memory
 * is its TRAIL, which the engine answers to an admin and to whoever leads the
 * seat (`mayReadTrail`); for anybody else it is drawn locked and never asked.
 *
 * # Counts are totals
 *
 * Every number here is the engine's total, never the length of what one window
 * carried: a catalogue of six hundred skills says six hundred with the first
 * five hundred drawn and "Load more", and a seat with forty learned skills
 * says forty over the twenty-five listed.
 *
 * # The tool skills' order is the engine's
 *
 * The listing is read a window at a time, so the ENGINE orders it: by name
 * (`order: "title"`), or newest first (`order: "newest"`, from the skill rows'
 * own index), chosen in the toolbar and kept in `sort=` — `title` or
 * `-updated`, the grid's own key, so the column the engine ordered by draws
 * the arrow. The heads are not buttons. Sorted here, a head would order the
 * rows one window held: past five hundred skills "Updated" put the newest of
 * the alphabetically first ones at the top, which reads exactly like the
 * newest skill. "Loaded by" is the usage domain's and no listing is ordered by
 * it, so it is not sortable either.
 */

import { useMemo } from "react";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import {
  AvatarStack,
  Button,
  Card,
  EmptyState,
  EmptyValue,
  SegmentedControl,
  Select,
  TableDateCell,
  TableNumberCell,
  TableTextCell,
} from "@crewlethq/ui";
import { WandSparklesGlyph } from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";

import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ADMIN } from "~/lib/access.ts";
import { GrantRequired } from "~/app/frame/GrantRequired.tsx";
import { TRAIL_WORDS, mayReadTrail } from "~/routes/agents/seat/profile.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, tsKey, fmtCount } from "~/lib/format.ts";
import { loadedBy } from "~/lib/pageReads.ts";
import { seatBadge } from "~/ui/SeatAvatar.tsx";
import { PAGE_LIST_ORDERS, type SkillLoad } from "~/contract/pages.ts";
import type { PageSummary } from "~/protocol/index.ts";
import { usePagedPages } from "./usePagedPages.ts";

/** The tool-skill pages: published, and only skills. */
const SKILL_PAGES = { skills: true, status: "published" } as const;

type ListOrder = (typeof PAGE_LIST_ORDERS)[number];

/**
 * Each order's `sort=`: the grid's own key for the column the engine orders
 * by, so the head draws the arrow the rows were read in.
 */
const ORDER_SORT: Record<ListOrder, string> = { title: "title", newest: "-updated" };

const ORDER_WORDS: Record<ListOrder, string> = { title: "Name", newest: "Newest" };

/** The order a `sort=` asks for, the knowledge base's own for any other value. */
export function skillOrderOf(sort: string): ListOrder {
  return PAGE_LIST_ORDERS.find((order) => ORDER_SORT[order] === sort) ?? "title";
}

export function Skills() {
  const [kind, setKind] = useParam("kind", "pages");
  const [sort, setSort] = useParam("sort", ORDER_SORT.title, "filter");
  const order = skillOrderOf(sort);
  return (
    <>
      <div className="toolbar">
        <SegmentedControl
          semantics="radio"
          activate="manual"
          label="Which skills"
          value={kind}
          onValueChange={setKind}
          options={[
            { value: "pages", label: "Tool skills", title: "Pages the engine offers a phase" },
            { value: "learned", label: "Learned", title: "What one agent drew from its own work" },
          ]}
        />
        {kind !== "learned" && (
          <>
            <span className="spacer" />
            <SegmentedControl
              semantics="radio"
              activate="manual"
              label="Order"
              value={ORDER_SORT[order]}
              onValueChange={setSort}
              options={PAGE_LIST_ORDERS.map((o) => ({
                value: ORDER_SORT[o],
                label: ORDER_WORDS[o],
              }))}
            />
          </>
        )}
      </div>
      {kind === "learned" ? <LearnedSkills /> : <ToolSkills order={order} />}
    </>
  );
}

function ToolSkills({ order }: { order: ListOrder }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const asked = useMemo(() => ({ ...SKILL_PAGES, order }), [order]);
  const listing = usePagedPages(asked, { pollMs: 60_000 });
  // IN THE ENGINE'S ORDER, as answered: see the module doc.
  const rows = listing.rows;
  const name = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const kindOf = (handle: string) => index.byHandle.get(handle)?.kind;
  const avatarOf = (handle: string) => index.byHandle.get(handle)?.avatar;
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
              hint: "Add a page to the skills space that names a tool.",
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
          defaultSort={ORDER_SORT.title}
          // THE ENGINE ORDERED THE WHOLE LISTING; a head that re-sorted the
          // windows loaded would answer another question under the same arrow.
          serverSorted
          rowHref={(r) => href(["doclet", "pages", r.id])}
          columns={[
            {
              key: "title",
              header: "Skill",
              cell: (r) => (
                <TableTextCell icon={<NamedGlyph name={"wand-sparkles"} size="xs" />}>
                  {r.title}
                </TableTextCell>
              ),
            },
            {
              key: "loaded",
              header: "Loaded by, 30 days",
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
                        ...seatBadge(name(x.handle), kindOf(x.handle), avatarOf(x.handle)),
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
              cell: (r) => {
                const l = loads(r);
                return l === null ? (
                  <EmptyValue label="Not recorded on this node" />
                ) : (
                  <TableNumberCell value={l.reduce((n, x) => n + x.loaded, 0)} format={fmtCount} />
                );
              },
            },
            {
              key: "updated",
              header: "Updated",
              shrink: true,
              align: "right",
              cell: (r) => <TableDateCell value={r.updated_at} now={now} />,
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
  const viewer = useViewer();
  const trail = seat !== "" && mayReadTrail(viewer, index, seat);
  const locked = seat !== "" && !viewer.loading && !trail;
  const memory = useQuery("agent_memory", { id: seat }, { enabled: trail, pollMs: 60_000 });
  const skills = memory.data?.skills ?? [];
  const total = memory.data?.skills_total ?? 0;
  const who = index.byHandle.get(seat);

  if (agents.length === 0) {
    return (
      <EmptyState
        icon={<WandSparklesGlyph size="xl" />}
        title="No agent seats"
        description="Add an agent in Edit org."
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
      {locked ? (
        <GrantRequired grants={[ADMIN]} reader={viewer.login} size="compact" also={TRAIL_WORDS} />
      ) : (
        <QueryState
          error={memory.error}
          refusal={memory.refusal}
          loading={memory.loading}
          empty={
            skills.length
              ? undefined
              : {
                  title: `${who?.name ?? seat} has learned no skills yet`,
                  hint: "Skills come from repeated work.",
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
                cell: (r) => <TableNumberCell value={r.uses} format={fmtCount} />,
              },
              {
                key: "version",
                header: "Version",
                shrink: true,
                align: "right",
                sortValue: (r) => r.version,
                cell: (r) => <TableNumberCell value={r.version} format={fmtCount} />,
              },
              {
                key: "updated",
                header: "Updated",
                shrink: true,
                align: "right",
                sortValue: (r) => tsKey(r.updated_at),
                cell: (r) => <TableDateCell value={r.updated_at} now={now} />,
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
              <a
                className="t-link"
                href={href(["company", "org", "seats", seat], { tab: "memory" })}
              >
                {who?.name ?? seat}&rsquo;s memory →
              </a>
            </span>
          </Card.Footer>
        </QueryState>
      )}
    </Card>
  );
}
