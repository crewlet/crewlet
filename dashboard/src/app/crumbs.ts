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
 * follows it unless the section IS the workspace's landing page ("Org chart"
 * alone on the chart, "Org chart › Roster" on the roster), and always in
 * Settings, whose column lists its landing like any other section: the tab
 * under the bar
 * says the same, but the trail is what titles the browser tab and what is
 * left on a phone where the tabs have scrolled away. The last crumb is the
 * OBJECT and carries no link: a trail whose final crumb links to the page you
 * are already on teaches a reader that the control does nothing.
 *
 * WORKLET'S PAGES END IN THEIR SCOPE, All projects or one project, and that
 * crumb SWITCHES it ([Crumb.switch]): Worklet › All projects on its lenses,
 * Worklet › FAIR Product and Engineering on a project's. On Worklet's landing
 * the workspace is plain words, because a link there would lead to the page it
 * is on.
 *
 * `labels` is what a screen knows and the route does not — a project's name
 * for its key, a seat's display name for its handle, a page's title for its
 * id. A label that has not arrived yet falls back to the segment itself,
 * which is an identifier a reader can still act on rather than a spinner in
 * the chrome, and it is drawn in the mono face so it reads as one.
 */

import { avatarOf, type AgentAvatar } from "~/lib/avatar.ts";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { unitPlace } from "~/lib/seats.ts";
import { originOf } from "./router.tsx";
import {
  SIDEBAR_GROUPS,
  listsLanding,
  ownedBy,
  workspaceRow,
  type Section,
  type WorkspaceRow,
} from "./nav.ts";
import {
  ITEM_KEY_SHAPE,
  chatViewOf,
  resolve,
  structureTabOf,
  type SignInScreen,
} from "./routes.ts";
import { ALL_PROJECTS, LENS_LABELS, type ScopeAt } from "./scope.ts";

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
  seat?: { name: string; kind: "agent" | "human"; avatar?: AgentAvatar | null };
  /**
   * The crumb names a scope the reader can switch, and is drawn as the
   * switcher's button (`header/ProjectSwitcher.tsx`). Only Worklet's last crumb
   * carries it, and a project is the one kind of scope there is.
   */
  switch?: "project";
  /** The project is archived: the switcher's button carries a tag saying so. */
  archived?: boolean;
}

/**
 * The label keys a seat's page publishes besides its name: the unit the seat
 * sits in (its KEY, which is the unit panel's address, `lib/seats.ts`'s
 * `unitPlace`), and its path as a reader names it ("Engineering · Core"), and
 * whether it is an agent or a person. The KEY and never the name: two units
 * may share a name, and a crumb addressed by one led to whichever came first.
 *
 * NO ROUTE SEGMENT CAN SPELL THEM: each starts with a NUL, which a path
 * segment never carries once decoded, so a seat's unit can never title a
 * crumb on another screen whose segment happens to read the same.
 */
export const seatUnitKey = (handle: string) => `\u0000unit:${handle}`;
export const seatPlaceKey = (handle: string) => `\u0000place:${handle}`;
export const seatKindKey = (handle: string) => `\u0000kind:${handle}`;
/**
 * How an agent seat is drawn, as its page publishes it for the crumb's badge:
 * the character and the colour as one label ([avatarLabel]), read back with
 * [avatarOfLabel]. A label is a string, and the badge needs both parts.
 */
export const seatAvatarKey = (handle: string) => `\u0000avatar:${handle}`;

/** An agent's avatar as the one label [seatAvatarKey] carries. */
export const avatarLabel = (avatar: AgentAvatar): string => `${avatar.character} ${avatar.color}`;

/** The avatar a [seatAvatarKey] label names, or nothing where no page published one. */
function avatarOfLabel(label: string | undefined): AgentAvatar | undefined {
  if (!label) return undefined;
  const [character, color] = label.split(" ");
  return avatarOf({ character, color });
}

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
 * seat keys are. Doclet › Engineering › Runbooks › Provisioner runbook.
 */
export const pageSpaceKey = (id: string) => `\u0000page-space:${id}`;
export const pageUpKey = (id: string, depth: number) => `\u0000page-up:${id}:${depth}`;

/**
 * The label keys a folder's page publishes for its trail: the id of each
 * folder above it, outermost first, whose names are ordinary labels keyed by
 * their ids. NUL-prefixed for the reason the seat keys are. Artifacts ›
 * Folders › Marketing › Q4 review.
 */
export const folderUpKey = (id: string, depth: number) => `\u0000folder-up:${id}:${depth}`;

export const turnSeatKey = (turnId: string) => `\u0000turn-seat:${turnId}`;
export const turnHandleKey = (turnId: string) => `\u0000turn-handle:${turnId}`;

/**
 * A title for the browser tab, derived from the same trail.
 *
 * ON WORKLET'S SCOPE PAGES THE LENS COMES FIRST, given where the reader is
 * ([ScopeAt]): "Tasks · All projects", "About · Product and Engineering", so
 * two tabs open on one project's board and its history read apart.
 */
export function titleOf(crumbs: Crumb[], at?: ScopeAt | null): string {
  const last = crumbs[crumbs.length - 1];
  if (!last) return "Crewlet";
  return at && last.switch ? `${LENS_LABELS[at.lens]} · ${last.label}` : last.label;
}

/** The label a screen supplied, or the raw segment drawn as an identifier. */
function named(labels: Labels, key: string): Crumb {
  const label = labels[key];
  return label ? { label } : { label: key, mono: true };
}

/**
 * A segment that IS the object's name — a node, a secret, a server, a
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

/**
 * The label key a project's page publishes when the project is archived, so
 * its crumb can say so. NUL-prefixed for the reason the seat keys are.
 */
export const projectArchivedKey = (key: string) => `\u0000archived:${key}`;

/** A project: its name with its key as a chip, or the key alone until the name arrives. */
function projectCrumb(labels: Labels, key: string): Crumb {
  const crumb = named(labels, key);
  const archived = labels[projectArchivedKey(key)] ? { archived: true } : {};
  return crumb.mono ? { ...crumb, ...archived } : { ...crumb, tag: key, ...archived };
}

/** What each screen outside the frame is called. */
const SIGN_IN_TITLES: Record<SignInScreen["screen"], string> = {
  login: "Sign in",
  enrol: "Two-step verification",
  invite: "Invitation",
  reset: "Password reset",
};

/**
 * The trail of a page, by its path and what its screen has said about it.
 *
 * A PAGE OPENED FROM A PLACE NAMES IT. A seat's page opened from Structure
 * carries that place's whole address (`from`, see `router.tsx`'s
 * `useTrailOrigin`), and its trail is that place's own ("Structure › Table")
 * followed by the seat: every crumb before the seat goes back to exactly the
 * address the reader left, its tab, chip, closed teams, selection, panel,
 * scroll and zoom included.
 */
export function crumbsFor(
  path: string[],
  labels: Labels = {},
  query: URLSearchParams = new URLSearchParams(),
): Crumb[] {
  const own = ownCrumbs(path, labels);
  const from = originOf({ query });
  if (!from) return own;
  const there = resolve(from.path);
  if (!there.resolved || structureTabOf(there.screen) === null) return own;
  const back = Object.fromEntries(from.query);
  const trail = ownCrumbs(from.path, labels).map((crumb) => ({
    ...crumb,
    path: from.path,
    query: back,
  }));
  // THE SEAT'S OWN CRUMB, and what is under it, stay the page's own.
  const at = own.findIndex((crumb) => crumb.seat !== undefined);
  return [...trail, ...own.slice(at >= 0 ? at : own.length - 1)];
}

function ownCrumbs(path: string[], labels: Labels): Crumb[] {
  const where = resolve(path);
  const row = workspaceRow(where.workspace);
  if (!where.resolved) {
    return row ? [root(row, true), { label: "Not found" }] : [{ label: "Not found" }];
  }
  // A SIGN-IN SCREEN IS DRAWN OUTSIDE THE FRAME, so nothing draws its trail;
  // what it has is the one crumb a tab's title is taken from.
  if (where.workspace === "") return [{ label: SIGN_IN_TITLES[where.screen] }];
  if (!row) return [{ label: "Not found" }];

  // A SECTION'S OWN PAGE: the workspace, then the section unless it IS the
  // workspace's landing page (Org chart › Roster, but Org chart alone for the
  // chart). SETTINGS NAMES ITS LANDING TOO: the column beside it lists Models
  // & keys as a row like any other, and "Settings" alone over the models
  // said nothing but the selected row did about which section it was. So
  // does Chatlet, whose Rooms panel lists Activity ([listsLanding]).
  //
  // THE LANDING IS THE SECTION AT THE ROW'S OWN ADDRESS, never one as deep as
  // it: a row landing on a section one segment under its head (Gitlet once
  // landed on `#/gitlet/repos`) has sibling sections as deep (its Pushes,
  // `#/gitlet/pushes`) that are not its landing — judged by depth, a sibling
  // lost its own crumb, and the trail and the tab both read the row's name
  // over somebody's pushes.
  const listed = listsLanding(row);
  const section = (key: string): Section | undefined => row.sections.find((s) => s.key === key);
  const landing = (s: Section) =>
    !listed && s.path.length === row.path.length && s.path.every((seg, i) => seg === row.path[i]);
  const sectionPage = (key: string): Crumb[] => {
    const s = section(key);
    if (!s || landing(s)) return [root(row, false)];
    return [root(row, true), { label: s.label }];
  };
  // AN OBJECT INSIDE A SECTION: the workspace, the section as a way back to its
  // list, and the object.
  const inside = (key: string, object: Crumb): Crumb[] => {
    const s = section(key);
    if (!s || landing(s)) return [root(row, true), object];
    return [root(row, true), { label: s.label, path: s.path }, object];
  };

  switch (where.screen) {
    case "principles":
      // ONE RULE FOR THE COMPANY GROUP: a row's own page is its row, as the
      // sidebar reads it and as every other group's rows are. Principles and
      // Brand alone carried a "Company" crumb, under an eyebrow saying it again.
      // BY ITS OWN NAME: this row's root is the group's ([rootLabel]), for a
      // mistyped `#/company/…`, which this page is not.
      return [{ label: row.label, icon: row.icon }];
    case "brand":
      return [root(row, false)];
    case "brand-setup":
      // ITS SETUP ADDS A CRUMB, and Brand becomes the way back.
      return [root(row, true), { label: "Set up" }];
    case "home":
    case "inbox":
    case "budgets":
    case "live":
    case "doclet":
    case "account":
      return [root(row, false)];
    case "me":
      return sectionPage(where.section);
    case "worklet-projects":
      // WORKLET'S LANDING: All projects' first lens, where a link back to
      // Worklet would lead to the page it is on.
      return [root(row, false), allProjects()];
    case "worklet-tasks":
    case "worklet-history":
      return [root(row, true), allProjects()];
    case "worklet-search":
      return sectionPage("search");
    case "worklet-views":
      return where.id ? inside("views", named(labels, where.id)) : sectionPage("views");
    case "project":
      return [root(row, true), { ...projectCrumb(labels, where.key), switch: "project" }];
    case "code-changes":
      return sectionPage("changes");
    case "code-change":
      // A CHANGE IS ONE OF GITLET'S CHANGES: the trail leads back to the
      // listing, and names the change by its reference.
      return [root(row, true), { label: `${where.repo}!${where.number}`, mono: true }];
    case "code-repos":
      return sectionPage("repos");
    case "code-search":
      return sectionPage("search");
    case "code-repo":
      // A REPOSITORY IS ONE OF GITLET'S REPOSITORIES: the trail leads back to
      // the listing, and names the repository as git does, in the code face.
      return inside("repos", { label: where.repo, mono: true });
    case "code-commit":
    case "code-compare":
      // A COMMIT AND A COMPARISON ARE A REPOSITORY'S: the trail leads back to
      // it, and names a commit by its short id, as git abbreviates one.
      return [
        ...inside("repos", {
          label: where.repo,
          mono: true,
          path: ["gitlet", "repos", where.repo],
        }),
        where.screen === "code-commit"
          ? { label: where.sha.slice(0, 7), mono: true }
          : { label: "Compare" },
      ];
    case "code-pushes":
      return sectionPage("pushes");
    case "code-queue":
      return sectionPage("queue");
    case "code-checks":
      return sectionPage("checks");
    case "item": {
      // AN ITEM KEY NAMES ITS PROJECT, so the trail carries the way to it
      // with no lookup. An id names nothing, and the trail is Worklet › item.
      const project = ITEM_KEY_SHAPE.test(where.id) ? where.id.replace(/-\d+$/, "") : "";
      const item = named(labels, where.id);
      return project
        ? [root(row, true), { ...projectCrumb(labels, project), path: ["worklet", project] }, item]
        : [root(row, true), item];
    }
    case "structure":
    case "structure-table":
    case "structure-moved": {
      // BOTH TABS ARE NAMED ("Structure › Org chart"), the landing's too, so
      // a seat page opened from either names the one it came from. The row's
      // own crumb is a way back only from the Table. A moved address is drawn
      // for the one frame before its screen says it again.
      const tab =
        where.screen === "structure-moved"
          ? where.was === "edit"
            ? "chart"
            : "table"
          : (structureTabOf(where.screen) ?? "chart");
      return [root(row, tab === "table"), { label: section(tab)?.label ?? "Org chart" }];
    }
    case "schedules": {
      if (where.scope.length === 0) return sectionPage("schedules");
      // A SCHEDULE IS THREE SEGMENTS, joined rather than crumbed: `role / ceo
      // / standup` in the trail would read as three pages that do not exist.
      const id = where.scope.join("/");
      return inside("schedules", { label: labels[id] ?? where.scope.join(" · ") });
    }
    case "seat": {
      // WHERE THE SEAT SITS, as a way to its unit ("Org chart › Engineering ·
      // Core › SWE") once the page has said; a seat above every unit, or one
      // the page has not resolved yet, goes straight from the workspace.
      const unit = labels[seatUnitKey(where.handle)];
      const place = labels[seatPlaceKey(where.handle)];
      const kind = labels[seatKindKey(where.handle)];
      const avatar = avatarOfLabel(labels[seatAvatarKey(where.handle)]);
      const seat = named(labels, where.handle);
      const object: Crumb =
        !seat.mono && (kind === "agent" || kind === "human")
          ? { ...seat, seat: { name: seat.label, kind: kind as "agent" | "human", avatar } }
          : seat;
      return unit
        ? [
            root(row, true),
            place
              ? { label: place, ...unitPlace({ id: unit }) }
              : { label: unit, ...unitPlace({ id: unit }), mono: true },
            object,
          ]
        : [root(row, true), object];
    }
    case "chatlet":
      // A ROOM IS A UUID, so the fallback is `mono` — the crumb is an address
      // until the screen publishes the room's name, and drawn in the
      // proportional face an unresolved id reads as something somebody called
      // a channel.
      //
      // A PANE IS NOT A SEGMENT and so is not a crumb: it is `?pane=` on the
      // place, which the trail deliberately ignores the way it ignores every
      // other query, since a pane open beside a room is not a place the
      // reader has gone.
      //
      // A ROOM HANGS FROM CHATLET ITSELF, whose crumb leads to its landing:
      // the rooms are a list in the Rooms panel, not a page to go back to.
      return where.id ? [root(row, true), named(labels, where.id)] : [root(row, false)];
    // A VIEW IS A SECTION OF THE ROW, named as the row names it (`nav.ts`).
    case "chatlet-activity":
    case "chatlet-dms":
    case "chatlet-threads":
    case "chatlet-drafts":
    case "chatlet-saved":
    case "chatlet-search":
    case "chatlet-browse":
    case "chatlet-new":
      return sectionPage(chatViewOf(where.screen) ?? "");
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
          seat: {
            name: seat,
            kind: "agent",
            avatar: handle ? avatarOfLabel(labels[seatAvatarKey(handle)]) : undefined,
          },
        },
        named(labels, where.id),
      ];
    }
    case "executors":
      return where.id ? inside("executors", named(labels, where.id)) : sectionPage("executors");
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
      // under [pageSpaceKey] and [pageUpKey]. Until it has, and for a page that
      // does not exist, the workspace and the noun, never the page's uuid.
      const space = labels[pageSpaceKey(where.id)];
      const up: Crumb[] = [];
      for (let i = 0; labels[pageUpKey(where.id, i)] !== undefined; i++) {
        const ancestor = labels[pageUpKey(where.id, i)]!;
        up.push({ label: labels[ancestor] || "Page", path: ["doclet", "pages", ancestor] });
      }
      return [
        root(row, true),
        ...(space ? [{ label: labels[space] ?? space, path: ["doclet", space] }] : []),
        ...up,
        { label: labels[where.id] || "Page" },
      ];
    }
    case "new-page":
      // WHERE THE PAGE WILL BE FILED is the composer's own first row, where
      // the writer can change it, so the trail names only what this is.
      return [root(row, true), { label: "New page" }];
    case "skills":
      return sectionPage("skills");
    case "diaries":
      return sectionPage("diaries");
    case "diary":
      return inside("diaries", named(labels, where.handle));
    case "artifacts":
      return sectionPage(where.section);
    case "artifact-folders":
      return sectionPage("folders");
    case "artifact-folder": {
      // THE FOLDER'S PLACE, which its id cannot say: every folder above it,
      // from what the folder's own page publishes under [folderUpKey], each
      // a way back to its page. Until it has (and for a folder the reader
      // cannot see), Artifacts › Folders › Folder, never the folder's uuid.
      const up: Crumb[] = [];
      for (let i = 0; labels[folderUpKey(where.id, i)] !== undefined; i++) {
        const ancestor = labels[folderUpKey(where.id, i)]!;
        up.push({ label: labels[ancestor] || "Folder", path: ["artifacts", "folders", ancestor] });
      }
      const folders = inside("folders", { label: labels[where.id] || "Folder" });
      return [...folders.slice(0, -1), ...up, folders[folders.length - 1]!];
    }
    case "artifact":
      // WHAT AN ARTIFACT IS CALLED is its own answer, published by its page.
      // Until then, and for one the reader may not open, the trail says
      // "Artifact" rather than its uuid: a 36-character id is nothing a
      // person reads, and the address bar already carries it.
      return [root(row, true), { label: labels[where.id] || "Artifact" }];
    case "new-artifact":
      // THE TRAIL NAMES THE KIND being made ("Artifacts › New doc"): each is
      // its own page, and "New artifact" said nothing the reader did not pick.
      return [
        root(row, true),
        {
          label: where.kind
            ? (section(`new-${where.kind}`)?.label ?? "New artifact")
            : "New artifact",
        },
      ];
    case "integrations":
      return where.kind
        ? [root(row, true), { label: labels[where.kind] ?? where.kind }]
        : [root(row, false)];
    case "spend":
    case "spend-tasks":
    case "limits":
    case "people":
    case "service-accounts":
    case "tokens":
    case "sign-in":
    case "storage":
    case "diagnostics":
      return sectionPage(where.screen);
    case "code-settings":
      return sectionPage("gitlet");
    case "mcp-tools": {
      const tools = section("mcp-tools");
      if (where.server) {
        return [
          root(row, true),
          { label: tools?.label ?? "MCP & tools", path: tools?.path ?? row.path },
          { label: "Servers" },
          ownName(labels, where.server),
        ];
      }
      return where.tool ? inside("mcp-tools", named(labels, where.tool)) : sectionPage("mcp-tools");
    }
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

/** All projects, as the scope Worklet's lenses end in. */
function allProjects(): Crumb {
  return { label: ALL_PROJECTS, switch: "project" };
}

function root(row: WorkspaceRow, link: boolean): Crumb {
  return link
    ? { label: rootLabel(row), path: row.path, icon: row.icon }
    : { label: rootLabel(row), icon: row.icon };
}

/**
 * What the trail calls a workspace: its own name, or, for the row that IS a
 * sidebar group's landing (it owns the group's own segment, as Principles owns
 * `#/company`), the group's, so a mistyped `#/company/…` reads
 * "Company › Not found" rather than "Principles › Not found".
 */
function rootLabel(row: WorkspaceRow): string {
  const group = SIDEBAR_GROUPS.find((g) => g.place === row.place && g.label !== "");
  const owns = ownedBy(row);
  return group && owns.length === 1 && owns[0] === row.place ? group.label : row.label;
}
