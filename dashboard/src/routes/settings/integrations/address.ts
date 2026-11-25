/**
 * An integration page's address: its tab (`tab=`, an object tab the reader
 * calls, so pushed) and its settings form (`edit=`, held by the Settings tab
 * and replaced as Principles' `edit=` is), so Back, a reload, Copy link and
 * Ask Crewlet's links land on the exact tab or form.
 */

import { href } from "~/app/router.tsx";
import {
  GOOGLE_PRODUCTS,
  GOOGLE_TABS,
  INTEGRATION_TABS,
  type GoogleProduct,
  type GoogleTab,
  type IntegrationTab,
} from "~/app/routes.ts";
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

/** The Google page's key: where every Google product's shared setup is made. */
export const GOOGLE = "google";

/** The forms the Google page's Settings tab opens in place: the connection, or the sign-in client. */
export const GOOGLE_FORMS = ["settings", "client"] as const;
export type GoogleForm = (typeof GOOGLE_FORMS)[number];

/** The Google tab a `tab=` value names, or null. */
export function googleTabOf(value: string | null): GoogleTab | null {
  return GOOGLE_TABS.find((t) => t === value) ?? null;
}

/** The Google form an `edit=` value names, or null. */
export function googleFormOf(value: string | null): GoogleForm | null {
  return GOOGLE_FORMS.find((f) => f === value) ?? null;
}

/** The Google product a `product=` value names, or null. */
export function googleProductOf(value: string | null): GoogleProduct | null {
  return GOOGLE_PRODUCTS.find((p) => p === value) ?? null;
}

/** Whether `kind` is one of the products the Google page holds. */
export function isGoogleProduct(kind: string): kind is GoogleProduct {
  return (GOOGLE_PRODUCTS as readonly string[]).includes(kind);
}

/** The Google page's query: a form holds its tab, and a product rides along. */
export function googleQuery(
  at: { tab?: GoogleTab; edit?: GoogleForm; product?: GoogleProduct | null } = {},
): Record<string, string> {
  const query: Record<string, string> = at.edit ? { edit: at.edit } : at.tab ? { tab: at.tab } : {};
  if (at.product) query.product = at.product;
  return query;
}

/** The Google page on a tab, with a form open, or with one product's row opened. */
export function googleAddress(
  at: { tab?: GoogleTab; edit?: GoogleForm; product?: GoogleProduct | null } = {},
): string {
  const query = googleQuery(at);
  return href(
    ["company", "integrations", GOOGLE],
    Object.keys(query).length > 0 ? query : undefined,
  );
}
