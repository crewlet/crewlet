/**
 * The categories Company › Integrations lists everything under, in the order
 * a company reaches for them, Crewlet's own service first in each. The list
 * beside the page and its home read the one table, so the two never disagree
 * about what is where.
 *
 * A LINE MAY NAME AN APP THIS BUILD DOES NOT SERVE YET: the growth products
 * are filed here ahead of their catalogue entries, so the one that lands needs
 * no change to this table. Such a line is drawn nowhere, and a group left with
 * no line is not drawn at all ([shownDirectory]).
 */

import type { BuiltInService } from "./model.ts";
import { CATALOG, type Entry } from "./catalogue.tsx";

/**
 * One line of the directory: a built-in service, an app, one product of an
 * app, or Google, the one connection the Google products share.
 */
export type Line =
  | { kind: "service"; key: BuiltInService["key"] }
  | { kind: "app"; key: string }
  | { kind: "product"; key: string; name: string; app: string }
  | { kind: "google"; key: "google" };

/** One group of the directory: what a company does with the apps under it. */
export interface Group {
  key: string;
  label: string;
  lines: Line[];
}

/** The directory's groups, in the order a company reaches for them. */
export const DIRECTORY: Group[] = [
  {
    key: "chat",
    label: "Chat",
    lines: [
      { kind: "service", key: "chat" },
      { kind: "app", key: "slack" },
      { kind: "app", key: "mattermost" },
    ],
  },
  {
    key: "work",
    label: "Work tracking",
    lines: [
      { kind: "service", key: "worklet" },
      { kind: "product", key: "jira", name: "Jira", app: "atlassian" },
    ],
  },
  {
    key: "docs",
    label: "Docs and knowledge",
    lines: [
      { kind: "service", key: "doclet" },
      { kind: "product", key: "confluence", name: "Confluence", app: "atlassian" },
    ],
  },
  {
    key: "code",
    label: "Code",
    lines: [
      { kind: "service", key: "gitlet" },
      { kind: "app", key: "github" },
      { kind: "app", key: "gitlab" },
    ],
  },
  { key: "alerts", label: "Observability", lines: [{ kind: "app", key: "datadog" }] },
  {
    key: "cloud",
    label: "Cloud and Infrastructure",
    lines: [
      { kind: "app", key: "aws" },
      { kind: "app", key: "cloudflare" },
      { kind: "app", key: "digitalocean" },
      { kind: "google", key: "google" },
      { kind: "app", key: "google_cloud" },
      { kind: "app", key: "scaleway" },
      { kind: "app", key: "ssh" },
    ],
  },
  {
    key: "mobile",
    label: "Mobile apps",
    lines: [
      { kind: "app", key: "expo" },
      { kind: "app", key: "app_store_connect" },
      { kind: "app", key: "google_play" },
    ],
  },
  {
    key: "advertising",
    label: "Advertising",
    lines: [
      { kind: "app", key: "google_ads" },
      { kind: "app", key: "meta_ads" },
      { kind: "app", key: "apple_ads" },
    ],
  },
  {
    key: "analytics",
    label: "Analytics",
    lines: [
      { kind: "app", key: "google_analytics" },
      { kind: "app", key: "search_console" },
    ],
  },
  {
    key: "ad_revenue",
    label: "Ad revenue",
    lines: [
      { kind: "app", key: "adsense" },
      { kind: "app", key: "admob" },
    ],
  },
];

/** The catalogue entry an app line or a product line stands for. */
export function entryOf(line: Line, catalog: readonly Entry[] = CATALOG): Entry | undefined {
  if (line.kind === "service" || line.kind === "google") return undefined;
  return catalog.find((e) => e.key === (line.kind === "product" ? line.app : line.key));
}

/**
 * The groups as drawn: each line whose app this build serves, and each group
 * with a line left. A built-in service is always served.
 */
export function shownDirectory(catalog: readonly Entry[] = CATALOG): Group[] {
  return DIRECTORY.map((group) => ({
    ...group,
    lines: group.lines.filter(
      (line) =>
        line.kind === "service" ||
        // GOOGLE IS SERVED WHERE ITS CONNECTION, Google Cloud, is.
        (line.kind === "google"
          ? catalog.some((e) => e.key === "google_cloud")
          : entryOf(line, catalog)),
    ),
  })).filter((group) => group.lines.length > 0);
}

/** The directory this build draws: the catalogue is fixed at build, so it is worked out once. */
export const SHOWN = shownDirectory();
