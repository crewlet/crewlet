/**
 * The breadcrumb for any route in the product.
 *
 * ONE FUNCTION over the route table, rather than a crumb each screen renders
 * for itself. A screen that builds its own trail gets it right on the day it
 * is written and then drifts: the previous shell derived a single `<h1>` from
 * the first path segment, so an item page said "Work", a project said "Work",
 * and a page four levels deep said "Knowledge" — the reader's whole sense of
 * place came from the sidebar row still being marked.
 *
 * The last segment is the OBJECT and carries no link. A trail whose final
 * crumb links to the page you are already on teaches a reader that the control
 * does nothing.
 *
 * `labels` is what a screen knows and the route does not — a project's name
 * for its key, a seat's display name for its handle, a page's title. A label
 * that has not arrived yet falls back to the segment itself, which is an
 * identifier a reader can still act on rather than a spinner in the chrome.
 */

import type { Crumb } from "../frame/PageBar.tsx";
import { DESTINATIONS, railRow, workspaceOf } from "../nav.ts";

export type Labels = Record<string, string>;

/** A title for the browser tab, derived from the same trail. */
export function titleOf(crumbs: Crumb[]): string {
  const last = crumbs[crumbs.length - 1];
  return last ? last.label : "Crewlet";
}

export function crumbsFor(path: string[], labels: Labels = {}): Crumb[] {
  const workspace = workspaceOf(path);
  const row = railRow(workspace);
  const head = path[0] ?? "";

  if (path.length === 0) return [{ label: "Inbox" }];

  // The workspace itself is the first crumb, and it is a link whenever the
  // reader is deeper than its landing page.
  const root: Crumb = row
    ? { label: row.label, path: path.length > 1 ? row.path : undefined }
    : { label: head };

  const rest = path.slice(1);
  // EVERY WORKSPACE OWNS EXACTLY ONE FIRST SEGMENT, so a landing page is one
  // crumb. Work owned a second — `goals` — until goals left the tracker, and
  // the trail had to name which of them the reader was on: without that
  // `#/goals` read "Work", and the goals list and the board were
  // indistinguishable in the page bar and in the tab title. A workspace given
  // a second segment again needs that branch back.
  if (rest.length === 0) return [root];

  switch (head) {
    case "work":
      return [root, ...workCrumbs(rest, labels)];
    case "company":
      return [root, ...companyCrumbs(rest, labels)];
    case "chat":
      return [root, ...chatCrumbs(rest, labels)];
    case "knowledge":
      return [root, ...knowledgeCrumbs(rest, labels)];
    case "activity":
      return [root, ...activityCrumbs(rest, labels)];
    case "cost":
      return [root, { label: destinationLabel("cost", rest[0] ?? "") ?? rest[0] ?? "" }];
    case "admin":
      return [root, ...adminCrumbs(rest, labels)];
    default:
      return [root, ...rest.map((seg) => ({ label: named(labels, seg) }))];
  }
}

/** The label a screen supplied, or the raw segment. */
function named(labels: Labels, key: string): string {
  return labels[key] ?? key;
}

/** A fixed destination's own label, from the one table. */
function destinationLabel(workspace: string, segment: string): string | undefined {
  return DESTINATIONS.find(
    (d) => d.workspace === workspace && d.path.length === 2 && d.path[1] === segment,
  )?.label;
}

function workCrumbs(rest: string[], labels: Labels): Crumb[] {
  const [first = "", second, third] = rest;

  // A FIXED DESTINATION FIRST, from the one table, because everything below
  // reads the segment as a KEY: a project key is uppercase and an item key is
  // `KEY-n`, so a lowercase reserved segment matches neither and fell through
  // to the item branch. `#/work/search` read "Work / Item / search" — a trail
  // naming a page that does not exist, on a screen whose whole job is finding
  // the page that does. Spelling each one here was the previous shape and it
  // is what left `search` out on the day it was added.
  const fixed = destinationLabel("work", first);
  if (fixed) {
    return second
      ? [{ label: fixed, path: ["work", first] }, { label: named(labels, second) }]
      : [{ label: fixed }];
  }

  // A PROJECT KEY IS UPPERCASE and an item key is `KEY-n`, which is what tells
  // them apart with no lookup: `#/work/ENG` is a project and `#/work/ENG-42`
  // is one of its items. An id — a uuid — matches neither and is treated as an
  // item, because every internal link to an item carries one.
  if (/-\d+$/.test(first) || !/^[A-Z][A-Z0-9_]*$/.test(first)) {
    const projectKey = first.replace(/-\d+$/, "");
    const project: Crumb = /^[A-Z][A-Z0-9_]*$/.test(projectKey)
      ? { label: named(labels, projectKey), path: ["work", projectKey], mono: !labels[projectKey] }
      : { label: "Item" };
    return [project, { label: named(labels, first), mono: !labels[first] }];
  }

  const project: Crumb = {
    label: named(labels, first),
    path: second ? ["work", first] : undefined,
    mono: !labels[first],
  };
  return [project];
}

function companyCrumbs(rest: string[], labels: Labels): Crumb[] {
  const [first = "", second] = rest;
  if (first === "people") {
    // A SEAT IS ADDRESSED BY A SLUG AND NAMED BY A NAME, so the fallback is mono
    // and the resolved label is not — the same rule every other branch here
    // follows, and the one this branch was missing. Until the screen publishes,
    // or where the handle resolves to no seat at all, the crumb is the raw
    // address and has to LOOK like one; drawn in the proportional face it is
    // indistinguishable from somebody's name.
    //
    // THE UNIT BRANCH BELOW IS DELIBERATELY NOT MONO: a unit's segment IS its
    // name (`UnitScreen` resolves `units.find((u) => u.name === id)`), so there
    // is no identifier there to mark.
    return second
      ? [
          { label: "People", path: ["company", "people"] },
          { label: named(labels, second), mono: !labels[second] },
        ]
      : [{ label: "People" }];
  }
  if (first === "units") {
    return second ? [{ label: "Units" }, { label: named(labels, second) }] : [{ label: "Units" }];
  }
  return [{ label: named(labels, first) }];
}

/**
 * Chat: two fixed lists, and otherwise a room.
 *
 * A ROOM IS A UUID, so the fallback is `mono` — the crumb is an address until
 * the screen publishes the room's name, and drawn in the proportional face an
 * unresolved id reads as something somebody called a channel. The fixed pair
 * come from the one destinations table for the reason `workCrumbs` gives: a
 * hand-written list here is what leaves a new list out on the day it is added.
 *
 * THE THREAD IS NOT A SEGMENT and so is not a crumb: it is `?thread=` on the
 * room, which the trail deliberately ignores the way it ignores every other
 * query — a pane open beside a room is not a place the reader has gone.
 */
function chatCrumbs(rest: string[], labels: Labels): Crumb[] {
  const [first = ""] = rest;
  const fixed = destinationLabel("chat", first);
  if (fixed) return [{ label: fixed }];
  return [{ label: named(labels, first), mono: !labels[first] }];
}

function knowledgeCrumbs(rest: string[], labels: Labels): Crumb[] {
  const [container = "", title] = rest;
  const head: Crumb = {
    label: named(labels, container),
    path: title ? ["knowledge", container] : undefined,
    mono: !labels[container],
  };
  return title ? [head, { label: named(labels, title) }] : [head];
}

function activityCrumbs(rest: string[], labels: Labels): Crumb[] {
  const [first = "", ...tail] = rest;
  // A TRACE HAS NO LIST OF ITS OWN — nothing enumerates traces, and every way
  // in is a link from an event, a turn or a coding run that already holds the
  // id. So its parent crumb is the turns list rather than a `traces` landing
  // page that does not exist: the reader who clicks up lands where the traces
  // actually are instead of on a segment named after a route.
  if (first === "traces") {
    const id = tail.join("/");
    return [
      { label: "Turns", path: ["activity", "turns"] },
      { label: labels[id] ?? id, mono: !labels[id] },
    ];
  }
  const known = destinationLabel("activity", first) ?? first;
  if (tail.length === 0) return [{ label: known }];
  // A SCHEDULE IS THREE SEGMENTS and everything else is one id, so the tail is
  // joined rather than turned into a crumb each: `role / ceo / standup` in the
  // trail would read as three pages that do not exist.
  const id = tail.join("/");
  return [
    { label: known, path: ["activity", first] },
    { label: labels[id] ?? tail.join(" · "), mono: tail.length === 1 && !labels[id] },
  ];
}

function adminCrumbs(rest: string[], labels: Labels): Crumb[] {
  const [first = "", ...tail] = rest;
  const known = destinationLabel("admin", first) ?? first;
  if (tail.length === 0) return [{ label: known }];
  // A TRAIL NEVER ENDS IN NOTHING. Each of these branches ended in
  // `named(labels, tail[1] ?? "")`, which is the empty string when the id
  // segment is absent — and both two-segment forms are live addresses
  // (`#/admin/config/revisions` routes, and `#/admin/tools/servers` is the
  // page of a tool that happens to be called that). Each drew a blank final
  // crumb, and `Shell` titles the tab from the trail, so the browser tab read
  // " · Crewlet".
  if (first === "config" && tail[0] === "revisions") {
    const trail: Crumb[] = [{ label: known, path: ["admin", "config"] }];
    if (!tail[1]) return [...trail, { label: "Revisions" }];
    return [
      ...trail,
      { label: "Revisions", path: ["admin", "config", "revisions"] },
      // MONO ONLY WHILE IT IS THE ID. This branch asserted the mono face
      // unconditionally, from when nothing published a label for a revision
      // and the crumb could only ever be the raw id. The config screen
      // publishes the revision's own summary now, and a sentence set in the
      // mono face reads as a value rather than as prose — which is the rule
      // every other branch here already follows.
      { label: named(labels, tail[1]), mono: !labels[tail[1]] },
    ];
  }
  // TWO SEGMENTS ONLY, because that is what the route discriminates on: one
  // segment under Infrastructure is a NODE, whatever it is called, and a node
  // may legally be called `domains`. A trail that read "Infrastructure /
  // Domains" over the node screen would name a place the reader is not at.
  if (first === "fleet" && tail[0] === "domains" && tail[1]) {
    return [
      { label: known, path: ["admin", "fleet"] },
      { label: "Domains" },
      { label: named(labels, tail[1]), mono: true },
    ];
  }
  if (first === "tools" && tail[0] === "servers") {
    const trail: Crumb[] = [{ label: known, path: ["admin", "tools"] }];
    if (!tail[1]) return [...trail, { label: "Servers" }];
    return [...trail, { label: "Servers" }, { label: named(labels, tail[1]), mono: true }];
  }
  const id = tail.join("/");
  return [
    { label: known, path: ["admin", first] },
    { label: labels[id] ?? id, mono: !labels[id] },
  ];
}
