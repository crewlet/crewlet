/**
 * The pinned shelf: the reader's stars, on Home, with the one or two facts
 * that make opening each one unnecessary on most mornings.
 *
 * A PIN IS A STAR. The dashboard already keeps, per reader, a list of the
 * things they chose to keep (`lib/starred.ts`, the star in every page's bar
 * and the sidebar's Starred group). A second "pinned on Home" list would be a
 * second bookmark for the same decision, so the shelf draws the stars, and
 * pinning here stars the thing everywhere.
 *
 * IN THE SIDEBAR'S ORDER, the order the stars were made: one list, read the
 * same way in both places.
 */

import { useServicesRun, useServicesAsk } from "~/lib/services.ts";
import { useMemo, type ReactNode } from "react";
import { Button, Card, PinTile, Tag } from "@crewlethq/ui";
import { PinGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";
import { pathShown, workspaceRow, type Workspace } from "~/app/nav.ts";
import { glyphFor } from "@crewlethq/icons/glyphs/registry";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { StateBadge } from "~/components/common.tsx";
import { useStarred, useToggleStar, type Star } from "~/lib/starred.ts";
import { useRecents } from "~/lib/recents.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatPath } from "~/lib/seats.ts";
import { projectPath } from "~/lib/work.ts";
import { censusLine } from "./Projects.tsx";
import { PARTIAL_FIGURE, gapsOf } from "./board/kit.tsx";

const keyOf = (path: readonly string[]): string => path.join("/");

interface Facts {
  kind: string;
  leading: ReactNode;
  meta?: ReactNode;
  status?: ReactNode;
  figure?: ReactNode;
}

/**
 * What is true of one pinned thing now: read from what the page already holds,
 * and a pinned project read BY ITS KEY.
 *
 * A PINNED PROJECT IS ASKED FOR ITSELF (`work_project`), as its own page asks.
 * It was looked up in the first fifty of the company's projects, so a pin on
 * a project past them drew no census at all, as though the engine knew
 * nothing about it. A board holds a handful of pins, so asking for each is a
 * handful of reads of one row each.
 */
function useFacts(star: Star): Facts {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const agents = useAgents();
  const viewer = useViewer();
  const ask = useServicesAsk();
  const route = resolve(star.path);
  const projectKey = route.resolved && route.screen === "project" ? route.key : "";
  const project = useQuery("work_project", projectKey ? { key: projectKey } : undefined, {
    enabled: projectKey !== "" && viewer.worklet && ask.tracker,
  });
  const glyph = (name: Workspace | "") => {
    const Glyph = glyphFor(workspaceRow(name)?.icon ?? "pin");
    return Glyph ? <Glyph size="md" /> : null;
  };
  if (!route.resolved) return { kind: "Page", leading: glyph("") };
  switch (route.screen) {
    case "seat": {
      const seat = index.byHandle.get(route.handle);
      const agent = agents.find((a) => a.handle === route.handle);
      return {
        kind: seat?.kind === "human" ? "Person" : "Agent",
        leading: (
          <SeatAvatar
            name={seat?.name ?? star.label}
            size="sm"
            kind={seat?.kind === "human" ? "human" : "agent"}
            avatar={seat?.avatar ?? null}
          />
        ),
        meta: seat?.unit?.name ?? "Reports to nobody",
        // The state where a project's figure goes, so the name keeps its width.
        figure: seat?.kind === "human" ? undefined : <StateBadge agent={agent} />,
      };
    }
    case "project": {
      const row = project.data;
      if (!row) return { kind: "Project", leading: glyph("worklet") };
      const { todo, active, done } = row.task_counts;
      const whole = todo + active + done;
      // A CENSUS OVER RECORDS THIS BUILD COULD NOT READ MAY BE WRONG, and
      // the tile says so in place of the census it would have drawn.
      const partial = gapsOf([row]).length > 0;
      return {
        kind: "Project",
        leading: glyph("worklet"),
        meta: partial
          ? PARTIAL_FIGURE
          : whole === 0
            ? "No work yet"
            : censusLine(done, active, todo),
        figure: whole > 0 && !partial ? `${Math.round((done / whole) * 100)}%` : undefined,
      };
    }
    case "item":
      return { kind: "Task", leading: glyph("worklet"), meta: route.id };
    case "page":
      return { kind: "Page", leading: glyph("doclet") };
    default:
      return {
        kind: workspaceRow(route.workspace)?.label ?? "Page",
        leading: glyph(route.workspace),
      };
  }
}

/** What to offer when nothing is pinned: places this reader went, and the company's agents and projects. */
function useSuggestions(pins: readonly Star[]): Omit<Star, "at">[] {
  const recents = useRecents();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const viewer = useViewer();
  const runs = useServicesRun();
  const ask = useServicesAsk();
  const projects = useQuery(
    "work_projects",
    { limit: 10 },
    { enabled: viewer.worklet && ask.tracker },
  );
  const pinned = new Set(pins.map((p) => keyOf(p.path)));
  const out: Omit<Star, "at">[] = [];
  const offer = (s: Omit<Star, "at">) => {
    if (!pinned.has(keyOf(s.path)) && !out.some((o) => keyOf(o.path) === keyOf(s.path)))
      out.push(s);
  };
  for (const r of recents) {
    if (resolve(r.path).resolved && r.path.length > 1 && pathShown(r.path, runs)) offer(r);
  }
  for (const seat of index.seats)
    if (seat.kind !== "human") offer({ path: seatPath(seat), label: seat.name, workspace: "org" });
  for (const p of projects.data?.projects ?? [])
    if (!p.archived)
      offer({ path: projectPath(p.key), label: p.name || p.key, workspace: "worklet" });
  return out.slice(0, 4);
}

export function PinnedShelf() {
  // NOT A PLACE THE COMPANY DOES NOT RUN THE SERVICE FOR, whose star is kept
  // until it runs it again.
  const runs = useServicesRun();
  const pins = useStarred().filter((s) => pathShown(s.path, runs));
  const toggle = useToggleStar();
  const suggestions = useSuggestions(pins);
  return (
    <Card padding="none" className="home-card">
      <Card.Header
        icon={<PinGlyph size="sm" />}
        actions={
          pins.length > 0 ? (
            <span className="home-card-caption">Star any page to pin it here</span>
          ) : undefined
        }
      >
        <Card.Title as="h3">
          Pinned
          {pins.length > 0 && (
            <Tag size="xs" variant="neutral" className="home-card-count">
              {pins.length.toLocaleString()}
            </Tag>
          )}
        </Card.Title>
      </Card.Header>
      {pins.length === 0 ? (
        <div className="home-pinned-empty">
          <p className="home-pinned-empty-text">
            Pin the agents, projects and pages you check every day. They stay here, with what is
            true of each right now.
          </p>
          {suggestions.length > 0 && (
            <div className="home-pinned-suggest" role="group" aria-label="Suggested pins">
              {suggestions.map((s) => (
                <Button
                  key={keyOf(s.path)}
                  size="small"
                  variant="secondary"
                  leadingIcon={<PlusGlyph size="sm" />}
                  onClick={() => toggle(s)}
                >
                  {s.label}
                </Button>
              ))}
            </div>
          )}
        </div>
      ) : (
        <ul className="home-pinned-grid" aria-label="Pinned">
          {pins.map((star) => (
            <li key={keyOf(star.path)}>
              <Pin star={star} onUnpin={() => toggle(star)} />
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

/** One pin, with what is true of it now. */
function Pin({ star, onUnpin }: { star: Star; onUnpin: () => void }) {
  const f = useFacts(star);
  return (
    <PinTile
      title={star.label}
      href={href(star.path)}
      kind={f.kind}
      meta={f.meta}
      leading={f.leading}
      status={f.status}
      figure={f.figure}
      onUnpin={onUnpin}
    />
  );
}
