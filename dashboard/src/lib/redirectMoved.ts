/**
 * The OAuth clients a person registered whose last sign-in came back to an
 * earlier engine address (`oauth_redirect_moved`): the engine reads the notice
 * off each grant's recorded redirect, and these are its words and its links.
 */

import { OAUTH_REDIRECT_MOVED } from "~/contract/advisor.ts";
import type { AdvisorNotice, AdvisorRegisteredClient } from "~/protocol/advisor.ts";

/** Where a client is signed in again: a Google product's settings form, an MCP server's Connection tab. */
export function signInPlace(client: AdvisorRegisteredClient): {
  path: string[];
  query: Record<string, string>;
  label: string;
} {
  const first = client.uses[0];
  if (client.vendor === "mcp") {
    const server = first?.name ?? client.label;
    return {
      path: ["settings", "mcp-tools", "servers", server],
      query: { tab: "connection" },
      label: `Open ${server}`,
    };
  }
  return {
    path: ["company", "integrations", first?.name ?? ""],
    query: { open: "settings" },
    label: `Open ${first?.title ?? "the integration"}`,
  };
}

/** The host an address names, or the address whole where it names none. */
export function hostOf(address: string): string {
  try {
    return new URL(address).host || address;
  } catch {
    return address;
  }
}

/** The redirect notice's client that `name` (an integration kind or an MCP server) signs in as, where it is stale. */
export function staleClientOf(
  notices: readonly AdvisorNotice[] | undefined,
  vendor: AdvisorRegisteredClient["vendor"],
  name: string,
): AdvisorRegisteredClient | undefined {
  const moved = notices?.find((n) => n.class === OAUTH_REDIRECT_MOVED)?.redirect;
  return moved?.clients.find((c) => c.vendor === vendor && c.uses.some((u) => u.name === name));
}
