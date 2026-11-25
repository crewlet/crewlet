/**
 * The OAuth clients a moved engine address left behind (`oauth_redirect_moved`):
 * the Inbox row's new URI and clients, and the line a client's own page places
 * beside its redirect URI while that client lists only an earlier address.
 */

import {
  ButtonLink,
  Callout,
  Copyable,
  EntityList,
  InlineCode,
  ReadingPaneSection,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { ADVISOR_POLL_MS, useReadsNotices } from "~/lib/useAttention.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { andList } from "~/lib/words.ts";
import { hostOf, signInPlace, staleClientOf } from "~/lib/redirectMoved.ts";
import type { AdvisorRedirectMoved, AdvisorRegisteredClient } from "~/protocol/advisor.ts";

/** What a client is, and the address its last sign-in came back to. */
function clientFact(c: AdvisorRegisteredClient): string {
  const what =
    c.vendor === "mcp"
      ? "The MCP server's own client"
      : `Google client of ${andList(c.uses.map((u) => u.title))}`;
  return `${what}, last signed in through ${hostOf(c.redirect)}`;
}

/** The notice's new redirect URI to copy, and each client to add it to, linked to where it signs in again. */
export function RedirectMovedClients({ moved }: { moved: AdvisorRedirectMoved }) {
  return (
    <>
      <ReadingPaneSection label="New redirect URI">
        <Copyable
          value={moved.uri}
          variant="block"
          monospace
          truncate={false}
          ariaLabel="Copy the new redirect URI"
        />
      </ReadingPaneSection>
      <ReadingPaneSection label="Clients to add it to">
        <EntityList
          label="OAuth clients to add the new redirect URI to"
          items={moved.clients.map((c) => {
            const place = signInPlace(c);
            return {
              id: `${c.vendor}:${c.id}`,
              name: c.label,
              fact: clientFact(c),
              action: (
                <ButtonLink variant="ghost" size="small" href={href(place.path, place.query)}>
                  {place.label}
                </ButtonLink>
              ),
            };
          })}
        />
      </ReadingPaneSection>
    </>
  );
}

/**
 * Says, beside a redirect URI, that the client `name` signs in as (an
 * integration kind for `google`, a server for `mcp`) last came back through an
 * earlier address; nothing while it lists the current one, or for a reader not
 * told the notices.
 */
export function RedirectStale({ vendor, name }: { vendor: "google" | "mcp"; name: string }) {
  const reads = useReadsNotices();
  const { data } = useQuery("advisor_notices", undefined, {
    pollMs: ADVISOR_POLL_MS,
    enabled: reads,
  });
  const notices = reads && Array.isArray(data?.notices) ? data.notices : undefined;
  const client = staleClientOf(notices, vendor, name);
  if (!client) return null;
  const google = vendor === "google";
  return (
    <Callout variant="warning" title="Redirect URI out of date">
      This client last signed in through <InlineCode>{client.redirect}</InlineCode>, an earlier
      address of this engine. Add the current redirect URI to its{" "}
      {google ? "Authorized redirect URIs in Google Cloud" : "redirect URIs at the vendor"} before
      the next sign-in, or {google ? "Google" : "the vendor"} refuses it.
    </Callout>
  );
}
