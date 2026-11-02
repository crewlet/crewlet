/**
 * Where the company's own apps live in a browser (`link_hosts`), read ONCE
 * for every surface in the frame.
 *
 * Every Markdown surface draws a vendor's mark beside a link to that vendor
 * and links a bare `owner/repo#14` to the company's code host
 * (`components/InlineToken.tsx`), so a page of comments asks about the hosts
 * once per token. Each token asking for itself would put a standing query per
 * link on the socket; `app/Shell.tsx`'s `FrameReadings` mounts this provider
 * so they all read one answer.
 *
 * NO HOSTS OUTSIDE THE FRAME, rather than a read of its own: a surface
 * rendered without the frame (a signed-out page, a suite) draws every public
 * host's mark (github.com, gitlab.com, Slack, Atlassian Cloud) and links no
 * bare reference, which is the honest answer for a reader nobody asked
 * about. A fallback read here would be the per-caller read this module
 * exists to remove.
 *
 * NOT POLLED: a host changes only when an administrator edits an
 * integration. The answer is asked again on a reconnect and when the reader
 * comes back to the tab, which is when a revision made meanwhile matters.
 */

import { createContext, createElement, useContext, useMemo, type ReactNode } from "react";
import type { LinkHost } from "~/contract/integrations.ts";
import { useQuery } from "./useQuery.ts";

const NONE: readonly LinkHost[] = [];

const Hosts = createContext<readonly LinkHost[]>(NONE);

export function LinkHostsProvider({ children }: { children: ReactNode }) {
  const { data } = useQuery("link_hosts", undefined, { refetchOnFocus: true });
  const hosts = useMemo(() => data?.hosts ?? NONE, [data]);
  return createElement(Hosts.Provider, { value: hosts }, children);
}

/** The company's link hosts, or none outside the frame. */
export function useLinkHosts(): readonly LinkHost[] {
  return useContext(Hosts);
}
