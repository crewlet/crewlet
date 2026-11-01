/**
 * The categories Company › Integrations lists everything under, in the order
 * a company reaches for them, Crewlet's own service first in each. The list
 * beside the page and its home read the one table, so the two never disagree
 * about what is where.
 */

import type { BuiltInService } from "./model.ts";
import { CATALOG, type Entry } from "./catalogue.tsx";

/** One line of the directory: a built-in service, an app, or one product of an app. */
export type Line =
  | { kind: "service"; key: BuiltInService["key"] }
  | { kind: "app"; key: string }
  | { kind: "product"; key: string; name: string; app: string };

/** The directory's groups, in the order a company reaches for them. */
export const DIRECTORY: { key: string; label: string; lines: Line[] }[] = [
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
      { kind: "app", key: "github" },
      { kind: "app", key: "gitlab" },
    ],
  },
  { key: "alerts", label: "Observability", lines: [{ kind: "app", key: "datadog" }] },
];

/** The catalogue entry an app line or a product line stands for. */
export function entryOf(line: Line): Entry | undefined {
  if (line.kind === "service") return undefined;
  return CATALOG.find((e) => e.key === (line.kind === "product" ? line.app : line.key));
}
