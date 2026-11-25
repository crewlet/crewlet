/**
 * An integration page's address: its tab (`tab=`, an object tab the reader
 * calls, so pushed) and its settings form (`edit=`, held by the Settings tab
 * and replaced as Principles' `edit=` is), so Back, a reload, Copy link and
 * Ask Crewlet's links land on the exact tab or form.
 */

import { href } from "~/app/router.tsx";
import { INTEGRATION_TABS, type IntegrationTab } from "~/app/routes.ts";
import type { Intent } from "./model.ts";

/** The forms the Settings tab opens in place: its settings, or Replace key. */
export const INTEGRATION_FORMS = ["settings", "key"] as const;
export type IntegrationForm = (typeof INTEGRATION_FORMS)[number];

/** The tab a `tab=` value names, or null for none this page has. */
export function tabOf(value: string | null): IntegrationTab | null {
  return INTEGRATION_TABS.find((t) => t === value) ?? null;
}

/** The form an `edit=` value names, or null for none. */
export function formOf(value: string | null): IntegrationForm | null {
  return INTEGRATION_FORMS.find((f) => f === value) ?? null;
}

/** The page of `kind` on a tab, or with a form open; neither is its default. */
export function integrationAddress(
  kind: string,
  at: { tab?: IntegrationTab; edit?: IntegrationForm } = {},
): string {
  const query: Record<string, string> | undefined = at.edit
    ? { edit: at.edit }
    : at.tab
      ? { tab: at.tab }
      : undefined;
  return href(["company", "integrations", kind], query);
}

/** Where an issue's fix is made: Replace key, the settings form, or the Agents tab. */
export function fixAddress(kind: string, intent: Intent): string {
  if (intent === "agents") return integrationAddress(kind, { tab: "agents" });
  return integrationAddress(kind, { edit: intent === "rotate" ? "key" : "settings" });
}
