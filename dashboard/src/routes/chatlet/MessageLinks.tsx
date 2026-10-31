/**
 * The links a message carries, drawn under its body in the room, in a thread
 * and in a room's Shared list alike, so no two of them draw one link two
 * ways.
 *
 * A RECORD UNFURLS: a task, a page, an artifact or a pull request the reader
 * may open is resolved by the engine as the reader (`chat_unfurls`, one read
 * for a message's links) and drawn as the kit's `LinkCard`: the app's mark
 * and name, the record's code and title, a quiet line of details, its status
 * and who holds it. A link the reader may not open, or that names nothing, is
 * absent from that answer and drawn by the link rules alone.
 *
 * A PREVIEW OPENS ONLY WHERE THE DASHBOARD CAN VOUCH FOR IT: the address the
 * engine built from the record, and of those only a page of this dashboard's
 * own (`lib/appRoute.ts`, opened in place) or an address the company's
 * configured host rebuilds exactly (`describeLink`, opened in a new tab).
 * Anything else the answer named is drawn inert rather than followed.
 *
 * AN ADDRESS OF AN APP THE DASHBOARD KNOWS, rebuilt on a host the company
 * configured, that resolved to no record still unfurls into its mark, name and
 * what the address names
 * (`gophrase/backend#14`), read from its PARSED host and path, never from
 * words a writer put beside it. Every other address is the address token
 * ([AddressLink]), by the rule a chat message's words are read with (`own`):
 * a writer chooses a message's links, so none opens as written.
 *
 * AT MOST [MAX_UNFURLS] previews: a message carries up to eight links, and
 * eight previews would bury the words they hang from. The rest are tokens.
 */

import type { ReactNode } from "react";

import { ArtifactKindGlyph, LinkCard, safeHref } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { addressMark, AddressLink } from "~/components/InlineToken.tsx";
import { appRoute } from "~/lib/appRoute.ts";
import { describeLink, type LinkDescription } from "~/lib/inlineTokens.ts";
import { useLinkHosts } from "~/lib/linkHosts.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { LinkHost } from "~/contract/integrations.ts";
import type { ChatUnfurl, ChatUnfurlKind } from "~/protocol/index.ts";

import { ChatFace } from "./Face.tsx";
import { useChatPeople } from "./people.ts";

/** How many of a message's links unfurl into previews. */
export const MAX_UNFURLS = 3;

/** The app a record belongs to, as its preview names it. */
export function unfurlSource(kind: ChatUnfurlKind): { name: string; mark: ReactNode } {
  switch (kind) {
    case "task":
      return { name: "Worklet", mark: <ProductMark product="worklet" size="xs" /> };
    case "page":
      return { name: "Doclet", mark: <ProductMark product="doclet" size="xs" /> };
    case "artifact":
      return { name: "Artifacts", mark: <ArtifactKindGlyph kind="design" size="xs" /> };
    case "pull_request":
      return { name: "GitHub", mark: <VendorMark vendor="github" size={12} /> };
  }
}

/**
 * Where a record's preview may open, or null for an inert preview: a page of
 * this dashboard's own, or an address a configured host rebuilds exactly.
 */
export function unfurlHref(
  href: string,
  hosts: readonly LinkHost[],
): { href: string; external: boolean } | null {
  const own = appRoute(href);
  if (own) return { href: own.canonical, external: false };
  const safe = safeHref(href);
  const said = safe === null ? null : describeLink(safe, hosts);
  return said?.canonical && said.canonical === safe ? { href: safe, external: true } : null;
}

/** One link, as the message draws it. */
type Drawn =
  | { kind: "record"; link: string; unfurl: ChatUnfurl }
  | { kind: "app"; link: string; said: LinkDescription }
  | { kind: "token"; link: string; address: string }
  | { kind: "words"; link: string };

export function MessageUnfurls({ links }: { links: readonly string[] | undefined }) {
  if (!links || links.length === 0) return null;
  return <Unfurled links={links} />;
}

function Unfurled({ links }: { links: readonly string[] }) {
  const hosts = useLinkHosts();
  const people = useChatPeople();
  // ONE READ FOR THE MESSAGE'S LINKS, the safe ones only: an address the
  // scheme allowlist refuses names nothing anybody may open.
  const urls = links.filter((link) => safeHref(link) !== null);
  const read = useQuery("chat_unfurls", { urls }, { enabled: urls.length > 0 });
  const resolved = new Map((read.data?.unfurls ?? []).map((u) => [u.url, u]));

  let cards = 0;
  const drawn = links.map((link): Drawn => {
    const address = safeHref(link);
    if (address === null) return { kind: "words", link };
    const unfurl = resolved.get(link);
    if (unfurl && cards < MAX_UNFURLS) {
      cards += 1;
      return { kind: "record", link, unfurl };
    }
    const said = describeLink(address, hosts);
    // ONLY ONE THE DASHBOARD CAN OPEN unfurls: an address it cannot rebuild
    // on the company's host stays the address token, whole and inert, so a
    // reader sees exactly where it would have gone.
    if (said?.vendor && said.canonical && cards < MAX_UNFURLS) {
      cards += 1;
      return { kind: "app", link: address, said };
    }
    return { kind: "token", link, address };
  });
  const previews = drawn.filter((d) => d.kind === "record" || d.kind === "app");
  const rest = drawn.filter((d) => d.kind === "token" || d.kind === "words");

  return (
    <>
      {previews.length > 0 && (
        <div className="chat-unfurls">
          {previews.map((d, index) => {
            // BY PLACE AND ADDRESS: a message may carry one link twice.
            const key = `${index}:${d.link}`;
            if (d.kind === "record") {
              const { unfurl } = d;
              const to = unfurlHref(unfurl.href, hosts);
              const owner = unfurl.owner ? people.person(unfurl.owner) : null;
              return (
                <LinkCard
                  key={key}
                  source={unfurlSource(unfurl.kind)}
                  title={unfurl.title}
                  address={d.link}
                  {...(unfurl.code ? { code: unfurl.code } : {})}
                  {...(unfurl.meta ? { meta: unfurl.meta } : {})}
                  {...(unfurl.status ? { status: unfurl.status } : {})}
                  {...(unfurl.tone ? { statusTone: unfurl.tone } : {})}
                  {...(owner
                    ? {
                        owner: {
                          face: <ChatFace person={owner} size={16} status={false} />,
                          name: owner.name,
                        },
                      }
                    : {})}
                  {...(to ? { href: to.href, external: to.external } : {})}
                />
              );
            }
            if (d.kind !== "app") return null;
            return (
              <LinkCard
                key={key}
                source={{ name: d.said.kind ?? d.said.label, mark: addressMark(d.said, "xs") }}
                title={d.said.label}
                address={d.link}
                href={d.said.canonical ?? undefined}
                external
              />
            );
          })}
        </div>
      )}
      {rest.length > 0 && (
        <div className="row wrap gap-2 chat-links">
          {rest.map((d, index) => (
            // CUT TO THE ROW, never wrapped: a link may be two kibibytes, and
            // one wider than the row keeps its whole address as its title.
            <span key={`${index}:${d.link}`} className="t-caption truncate">
              {d.kind === "token" ? <AddressLink address={d.address} links="own" /> : d.link}
            </span>
          ))}
        </div>
      )}
    </>
  );
}
