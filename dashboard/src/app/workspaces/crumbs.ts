/**
 * The breadcrumb for any route in the product.
 *
 * ONE FUNCTION over the route table, rather than a crumb each screen renders
 * for itself. A screen that builds its own trail gets it right on the day it
 * is written and then drifts: the previous shell derived a single `<h1>` from
 * the first path segment, so an item page said "Work", a sprint said "Work",
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
  if (rest.length === 0) {
    // A WORKSPACE OWNS MORE THAN ONE FIRST SEGMENT in one case — Work owns
    // `goals` as well as `work` — and the trail has to name which of them the
    // reader is on. Without this `#/goals` read "Work", so the goals list and
    // the board were indistinguishable in the page bar and in the tab title.
    const own = DESTINATIONS.find(
      (d) => d.path.length === 1 && d.path[0] === head && d.workspace === workspace,
    );
    return own && head !== row?.path[0]
      ? [{ ...root, path: row?.path }, { label: own.label }]
      : [root];
  }

  switch (head) {
    case "work":
      return [root, ...workCrumbs(rest, labels)];
    case "goals":
      return [
        { label: "Work", path: ["work"] },
        { label: "Goals", path: ["goals"] },
        { label: named(labels, rest[0] ?? "") },
      ];
    case "company":
      return [root, ...companyCrumbs(rest, labels)];
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
  if (first === "views") {
    return second
      ? [{ label: "Saved views", path: ["work", "views"] }, { label: named(labels, second) }]
      : [{ label: "Saved views" }];
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
  if (second === "sprints") {
    return third
      ? [
          project,
          { label: "Sprints", path: ["work", first, "sprints"] },
          { label: labels[`sprint:${third}`] ?? `Sprint ${third}` },
        ]
      : [project, { label: "Sprints" }];
  }
  return [project];
}

function companyCrumbs(rest: string[], labels: Labels): Crumb[] {
  const [first = "", second] = rest;
  if (first === "people") {
    return second
      ? [{ label: "People", path: ["company", "people"] }, { label: named(labels, second) }]
      : [{ label: "People" }];
  }
  if (first === "units") {
    return second ? [{ label: "Units" }, { label: named(labels, second) }] : [{ label: "Units" }];
  }
  return [{ label: named(labels, first) }];
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
  if (first === "config" && tail[0] === "revisions") {
    return [
      { label: known, path: ["admin", "config"] },
      { label: "Revisions" },
      { label: named(labels, tail[1] ?? ""), mono: true },
    ];
  }
  if (first === "tools" && tail[0] === "servers") {
    return [
      { label: known, path: ["admin", "tools"] },
      { label: "Servers" },
      { label: named(labels, tail[1] ?? ""), mono: true },
    ];
  }
  const id = tail.join("/");
  return [
    { label: known, path: ["admin", first] },
    { label: labels[id] ?? id, mono: !labels[id] },
  ];
}
