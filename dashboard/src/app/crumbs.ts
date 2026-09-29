/**
 * The breadcrumb for any route in the product, from the route table.
 *
 * ONE FUNCTION over `routes.ts`'s resolver, rather than a crumb each screen
 * renders for itself and rather than a second reading of the path. A screen
 * that builds its own trail gets it right on the day it is written and then
 * drifts, and a trail that parsed the path on its own had its own idea of
 * which tails were valid: `#/activity/traces` was a page to the trail and the
 * turns list to the dispatch.
 *
 * The first crumb is the WORKSPACE, with its glyph — the one mark of where
 * the reader is that survives a sidebar folded into a drawer. A SECTION
 * follows it unless the section IS the workspace's landing page ("Agents"
 * alone on the chart, "Agents › Roster" on the roster): the tab under the bar
 * says the same, but the trail is what titles the browser tab and what is
 * left on a phone where the tabs have scrolled away. The last crumb is the
 * OBJECT and carries no link: a trail whose final crumb links to the page you
 * are already on teaches a reader that the control does nothing.
 *
 * `labels` is what a screen knows and the route does not — a project's name
 * for its key, a seat's display name for its handle, a page's title for its
 * id. A label that has not arrived yet falls back to the segment itself,
 * which is an identifier a reader can still act on rather than a spinner in
 * the chrome, and it is drawn in the mono face so it reads as one.
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";
import { workspaceRow, type Section, type WorkspaceRow } from "./nav.ts";
import { ITEM_KEY_SHAPE, resolve } from "./routes.ts";

export type Labels = Record<string, string>;

export interface Crumb {
  label: string;
  /** Absent on the last segment: the object is not a link to itself. */
  path?: string[];
  query?: Record<string, string>;
  /** Rendered in the mono face — a key, a handle, an id. */
  mono?: boolean;
  /** The workspace's glyph, on the first crumb only. */
  icon?: GlyphName;
  /**
   * A short identifier drawn as a chip before a NAMED label — a project's key
   * beside its name ("ENG Core platform"), the way every row and rail in the
   * product draws a project. Only where the label is a name: a crumb still
   * showing the raw key has nothing to add, and would say it twice.
   */
  tag?: string;
  /**
   * A seat's badge before its name, the way every row and card draws a seat —
   * the kind is its outline. Only on the seat's own crumb.
   */
  seat?: { name: string; kind: "agent" | "human" };
}

/**
 * The label keys a seat's page publishes besides its name: the unit the seat
 * sits in — its NAME, which is the Teams address, and its path as a reader
 * names it ("Engineering · Core") — and whether it is an agent or a person.
 *
 * NO ROUTE SEGMENT CAN SPELL THEM: each starts with a NUL, which a path
 * segment never carries once decoded, so a seat's unit can never title a
 * crumb on another screen whose segment happens to read the same.
 */
export const seatUnitKey = (handle: string) => `\u0000unit:${handle}`;
export const seatPlaceKey = (handle: string) => `\u0000place:${handle}`;
export const seatKindKey = (handle: string) => `\u0000kind:${handle}`;

/**
 * The label keys a turn's page publishes besides its own name: the seat that
 * ran it, by name and by handle, which the trail draws as its middle crumb —
 * Live › {agent} › Turn n · KEY — leading back to what is running for that
 * seat. NUL-prefixed for the reason the seat keys are.
 */
/**
 * The label keys a page's screen publishes for its trail: the space it is
 * filed in, and the id of each page above it, outermost first — whose titles
 * are ordinary labels keyed by their ids. NUL-prefixed for the reason the
 * seat keys are. Knowledge › Engineering › Runbooks › Provisioner runbook.
 */
export const pageSpaceKey = (id: string) => `\u0000page-space:${id}`;
export const pageUpKey = (id: string, depth: number) => `\u0000page-up:${id}:${depth}`;

export const turnSeatKey = (turnId: string) => `\u0000turn-seat:${turnId}`;
export const turnHandleKey = (turnId: string) => `\u0000turn-handle:${turnId}`;

/** A title for the browser tab, derived from the same trail. */
export function titleOf(crumbs: Crumb[]): string {
  const last = crumbs[crumbs.length - 1];
  return last ? last.label : "Crewlet";
}

/** The label a screen supplied, or the raw segment drawn as an identifier. */
function named(labels: Labels, key: string): Crumb {
  const label = labels[key];
  return label ? { label } : { label: key, mono: true };
}

/**
 * A segment that IS the object's name — a unit, a node, a secret, a server, a
 * model's config key, a backup domain — drawn as a name, never as an
 * identifier awaiting one.
 *
 * NOT `named`: its fallback is the mono face, which says "a key, until the
 * screen supplies the name", and no screen ever will for these — the page
 * titles itself with this same word, in the body face. Drawn mono, the trail
 * printed `harness-0` in one face over a title printing it in another, which
 * reads as two different values for one node.
 */
function ownName(labels: Labels, key: string): Crumb {
  return { label: labels[key] ?? key };
}

/** A project: its name with its key as a chip, or the key alone until the name arrives. */
function projectCrumb(labels: Labels, key: string): Crumb {
  const crumb = named(labels, key);
  return crumb.mono ? crumb : { ...crumb, tag: key };
}

export function crumbsFor(path: string[], labels: Labels = {}): Crumb[] {
  const where = resolve(path);
  const row = workspaceRow(where.workspace);
  if (!where.resolved) {
    return row ? [root(row, true), { label: "Not found" }] : [{ label: "Not found" }];
  }
  if (!row) return [{ label: "Not found" }];

  // A SECTION'S OWN PAGE: the workspace, then the section unless it IS the
  // workspace's landing page — Agents › Roster, but Agents alone for the chart.
  const section = (key: string): Section | undefined => row.sections.find((s) => s.key === key);
  const sectionPage = (key: string): Crumb[] => {
    const s = section(key);
    if (!s || s.path.length === row.path.length) return [root(row, false)];
    return [root(row, true), { label: s.label }];
  };
  // AN OBJECT INSIDE A SECTION: the workspace, the section as a way back to its
  // list, and the object.
  const inside = (key: string, object: Crumb): Crumb[] => {
    const s = section(key);
    if (!s || s.path.length === row.path.length) return [root(row, true), object];
    return [root(row, true), { label: s.label, path: s.path }, object];
  };

  switch (where.screen) {
    case "home":
    case "inbox":
    case "work":
    case "org-chart":
    case "live":
    case "knowledge":
    case "spend":
      return [root(row, false)];
    case "general":
      // SETTINGS' LANDING IS ONE OF ITS SECTIONS, unlike every other
      // workspace's: the column beside it lists General as a row like any
      // other, so the trail names it. "Settings" alone was the page's `h1`
      // over the company's charter, and nothing on screen but the selected
      // row said which section it was.
      return [root(row, true), { label: section("general")?.label ?? "General" }];
    case "me":
      return sectionPage(where.section);
    case "work-projects":
      return sectionPage("projects");
    case "work-history":
      return sectionPage("history");
    case "work-search":
      return sectionPage("search");
    case "work-views":
      return where.id ? inside("views", named(labels, where.id)) : sectionPage("views");
    case "project":
      return [root(row, true), projectCrumb(labels, where.key)];
    case "item": {
      // AN ITEM KEY NAMES ITS PROJECT, so the trail carries the way to it
      // with no lookup. An id names nothing, and the trail is Work › item.
      const project = ITEM_KEY_SHAPE.test(where.id) ? where.id.replace(/-\d+$/, "") : "";
      const item = named(labels, where.id);
      return project
        ? [root(row, true), { ...projectCrumb(labels, project), path: ["work", project] }, item]
        : [root(row, true), item];
    }
    case "roster":
      return sectionPage("roster");
    case "teams":
      // A UNIT'S SEGMENT IS ITS NAME, so there is no identifier to mark.
      return where.unit ? inside("teams", ownName(labels, where.unit)) : sectionPage("teams");
    case "schedules": {
      if (where.scope.length === 0) return sectionPage("schedules");
      // A SCHEDULE IS THREE SEGMENTS, joined rather than crumbed: `role / ceo
      // / standup` in the trail would read as three pages that do not exist.
      const id = where.scope.join("/");
      return inside("schedules", { label: labels[id] ?? where.scope.join(" · ") });
    }
    case "org-edit":
      return [root(row, true), { label: section("edit")?.label ?? "Edit org" }];
    case "seat": {
      // WHERE THE SEAT SITS, as a way to its unit — "Agents › Engineering ·
      // Core › SWE" — once the page has said; a seat above every unit, or one
      // the page has not resolved yet, goes straight from the workspace.
      const unit = labels[seatUnitKey(where.handle)];
      const place = labels[seatPlaceKey(where.handle)];
      const kind = labels[seatKindKey(where.handle)];
      const seat = named(labels, where.handle);
      const object: Crumb =
        !seat.mono && (kind === "agent" || kind === "human")
          ? { ...seat, seat: { name: seat.label, kind: kind as "agent" | "human" } }
          : seat;
      return unit
        ? [root(row, true), { label: place || unit, path: ["agents", "teams", unit] }, object]
        : [root(row, true), object];
    }
    case "turns":
      return sectionPage("turns");
    case "turn": {
      // THE SEAT, where the page has said which: the trail reads Live ›
      // {agent} › Turn n, and the agent crumb is what is running for that
      // seat now. Until the page knows, the Turns section stands in, as it
      // always did.
      const seat = labels[turnSeatKey(where.id)];
      const handle = labels[turnHandleKey(where.id)];
      if (!seat) return inside("turns", named(labels, where.id));
      return [
        root(row, true),
        {
          label: seat,
          path: ["live"],
          ...(handle ? { query: { seat: handle } } : {}),
          seat: { name: seat, kind: "agent" },
        },
        named(labels, where.id),
      ];
    }
    case "runs":
      return where.id ? inside("runs", named(labels, where.id)) : sectionPage("runs");
    case "a2a":
      return where.id ? inside("a2a", named(labels, where.id)) : sectionPage("a2a");
    case "trace":
      // A TRACE HAS NO LIST, so its parent is the workspace itself: the reader
      // who clicks up lands on what is running rather than on a segment named
      // after a route with no page.
      return [root(row, true), named(labels, where.id)];
    case "events":
      return sectionPage("events");
    case "event":
      return inside("events", named(labels, where.id));
    case "container":
      return [root(row, true), named(labels, where.key)];
    case "page": {
      // THE PAGE'S PLACE, which its id cannot say: the space it is filed in
      // and the pages above it, from what the page's own screen publishes
      // under [pageSpaceKey] and [pageUpKey] — until it has, the workspace
      // and the page, which is still a trail a reader can act on.
      const space = labels[pageSpaceKey(where.id)];
      const up: Crumb[] = [];
      for (let i = 0; labels[pageUpKey(where.id, i)] !== undefined; i++) {
        const ancestor = labels[pageUpKey(where.id, i)]!;
        up.push({ label: labels[ancestor] ?? ancestor, path: ["knowledge", "pages", ancestor] });
      }
      return [
        root(row, true),
        ...(space ? [{ label: labels[space] ?? space, path: ["knowledge", space] }] : []),
        ...up,
        named(labels, where.id),
      ];
    }
    case "skills":
      return sectionPage("skills");
    case "diaries":
      return sectionPage("diaries");
    case "diary":
      return inside("diaries", named(labels, where.handle));
    case "budgets":
      return sectionPage("budgets");
    case "spend-tasks":
      return sectionPage("tasks");
    case "people":
      return sectionPage("people");
    case "integrations":
      return where.kind
        ? inside("integrations", { label: labels[where.kind] ?? where.kind })
        : sectionPage("integrations");
    case "tools":
      if (where.server) {
        return [
          root(row, true),
          { label: section("tools")?.label ?? "Tools & MCP", path: ["settings", "tools"] },
          { label: "Servers" },
          ownName(labels, where.server),
        ];
      }
      return where.tool ? inside("tools", named(labels, where.tool)) : sectionPage("tools");
    case "models":
      // THE ENTRY'S CONFIG KEY IS ITS NAME: the model's page is titled by it,
      // in the body face, and so is its crumb — the key a seat's `llm:` writes
      // is the only name the entry has.
      return where.id ? inside("models", ownName(labels, where.id)) : sectionPage("models");
    case "secrets":
      return where.name ? inside("secrets", ownName(labels, where.name)) : sectionPage("secrets");
    case "nodes":
      return where.node ? inside("nodes", ownName(labels, where.node)) : sectionPage("nodes");
    case "config": {
      if (!where.revisions) return sectionPage("config");
      const config: Crumb = {
        label: section("config")?.label ?? "Configuration",
        path: ["settings", "config"],
      };
      // A TRAIL NEVER ENDS IN NOTHING: the list of revisions is a live
      // address, so it is named rather than drawn as a blank last crumb —
      // which the tab title would then read as " · Crewlet".
      if (!where.revision) return [root(row, true), config, { label: "Revisions" }];
      return [
        root(row, true),
        config,
        { label: "Revisions", path: ["settings", "config", "revisions"] },
        named(labels, where.revision),
      ];
    }
    case "backups":
      return where.domain
        ? inside("backups", ownName(labels, where.domain))
        : sectionPage("backups");
    case "audit":
      return sectionPage("audit");
  }
}

function root(row: WorkspaceRow, link: boolean): Crumb {
  return link
    ? { label: row.label, path: row.path, icon: row.icon }
    : { label: row.label, icon: row.icon };
}
