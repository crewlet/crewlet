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
}

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
    case "general":
      return [root(row, false)];
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
      return [root(row, true), named(labels, where.key)];
    case "item": {
      // AN ITEM KEY NAMES ITS PROJECT, so the trail carries the way to it
      // with no lookup. An id names nothing, and the trail is Work › item.
      const project = ITEM_KEY_SHAPE.test(where.id) ? where.id.replace(/-\d+$/, "") : "";
      const item = named(labels, where.id);
      return project
        ? [root(row, true), { ...named(labels, project), path: ["work", project] }, item]
        : [root(row, true), item];
    }
    case "roster":
      return sectionPage("roster");
    case "teams":
      // A UNIT'S SEGMENT IS ITS NAME, so there is no identifier to mark.
      return where.unit
        ? inside("teams", { label: labels[where.unit] ?? where.unit })
        : sectionPage("teams");
    case "schedules": {
      if (where.scope.length === 0) return sectionPage("schedules");
      // A SCHEDULE IS THREE SEGMENTS, joined rather than crumbed: `role / ceo
      // / standup` in the trail would read as three pages that do not exist.
      const id = where.scope.join("/");
      return inside("schedules", { label: labels[id] ?? where.scope.join(" · ") });
    }
    case "org-edit":
      return [root(row, true), { label: section("edit")?.label ?? "Edit org" }];
    case "seat":
      return [root(row, true), named(labels, where.handle)];
    case "turns":
      return sectionPage("turns");
    case "turn":
      return inside("turns", named(labels, where.id));
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
    case "page":
      return [root(row, true), named(labels, where.id)];
    case "budgets":
      return sectionPage("budgets");
    case "integrations":
      return where.kind
        ? inside("integrations", { label: labels[where.kind] ?? where.kind })
        : sectionPage("integrations");
    case "tools":
      if (where.server) {
        return [
          root(row, true),
          { label: section("tools")?.label ?? "Tools", path: ["settings", "tools"] },
          { label: "Servers" },
          { label: where.server, mono: true },
        ];
      }
      return where.tool ? inside("tools", named(labels, where.tool)) : sectionPage("tools");
    case "secrets":
      return where.name ? inside("secrets", named(labels, where.name)) : sectionPage("secrets");
    case "nodes":
      return where.node ? inside("nodes", named(labels, where.node)) : sectionPage("nodes");
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
        ? inside("backups", { label: where.domain, mono: true })
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
