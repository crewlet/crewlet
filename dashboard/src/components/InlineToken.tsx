/**
 * One inline token, drawn: what `lib/inlineTokens.ts` found in a run of
 * prose, as the reference it is, with the mark that says what it is.
 *
 * THE RECOGNISER SAYS WHERE A TOKEN IS; THIS SAYS WHAT IT BECOMES, from the
 * company's own records and nothing the prose claims:
 *
 *  - A COLOUR is its words after a swatch of it.
 *  - AN ADDRESS carries the mark of the app its PARSED host belongs to
 *    (`describeLink`), and says the reference it is (`gophrase/backend#14`)
 *    in place of the whole address, which its title keeps.
 *  - A REFERENCE or a COMMIT carries its code host's mark and links there
 *    when the company configured that host (`referenceTarget`); a reference
 *    two hosts could own is its words.
 *  - A REPOSITORY (a bare `owner/repo` under an owner the code hosts name)
 *    carries its owner's code host's mark and links to the repository
 *    (`repositoryTarget`).
 *  - A KEY links to its work item only when its project is one of the
 *    company's (`lib/projectKeys.ts`): `ISO-8601` is a key's shape and a word.
 *  - A MENTION links to the seat only when the chart holds that handle.
 *
 * A KEY AND A MENTION ARE THEIR WORDS, with no mark: they name the company's
 * own things, which a reader knows by name, and a glyph or a face before each
 * one made a line of them hard to read. A mark is kept for what lives outside
 * the company (a code host, a chat, a wiki) and for a colour, where it says
 * what the words alone do not.
 *
 * Anything that resolves to nothing is its words, exactly as written.
 *
 * `links` false draws every token as words with its mark: inside a control or
 * a link (an Inbox row, an answer's option, a link's own label), where a link
 * is invalid or steals the press, and in a model's prose (`links: "inert"`),
 * where nothing the model wrote may become a link.
 */

import { useContext, useMemo, type ReactNode } from "react";
import { ColorChip, Highlight, InlineRef } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { GlobeGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import {
  codeHostOwners,
  describeLink,
  inlineTokens,
  referenceTarget,
  repositoryTarget,
  type InlineToken as Token,
} from "~/lib/inlineTokens.ts";
import { useLinkHosts } from "~/lib/linkHosts.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { useProjectKeys } from "~/lib/projectKeys.ts";
import { seatPath, useOrgIndex } from "~/lib/seats.ts";
import { itemPath } from "~/lib/work.ts";

/**
 * One run of prose with its tokens drawn, the words between them marked where
 * a search matched (`highlight`).
 *
 * A COMPONENT, so the run is read once: the recogniser runs when the run's
 * words or the company's code host owners change, and a re-render of the
 * surface around it (a page of comments, an Inbox list ticking its times)
 * reads nothing again. The owners come from the frame's `link_hosts`, which
 * a plain function of the text could not read.
 */
export function TokenRun({
  text,
  links,
  highlight,
}: {
  text: string;
  links: boolean;
  highlight?: readonly string[] | undefined;
}) {
  const hosts = useLinkHosts();
  const owners = useMemo(() => codeHostOwners(hosts), [hosts]);
  const tokens = useMemo(() => inlineTokens(text, owners), [text, owners]);
  const words = (part: string, at: number): ReactNode =>
    highlight ? <Highlight key={`w${at}`} text={part} terms={highlight} /> : part;
  const out: ReactNode[] = [];
  let at = 0;
  for (const token of tokens) {
    if (token.at > at) out.push(words(text.slice(at, token.at), at));
    out.push(<InlineToken key={`k${token.at}`} token={token} links={links} />);
    at = token.end;
  }
  if (at < text.length) out.push(words(text.slice(at), at));
  return out;
}

export function InlineToken({ token, links }: { token: Token; links: boolean }) {
  switch (token.kind) {
    case "colour":
      return (
        <InlineRef kind="Colour" mark={<ColorChip value={token.hex} size="text" shape="square" />}>
          {token.text}
        </InlineRef>
      );
    case "address":
      return <AddressToken token={token} links={links} />;
    case "reference":
    case "commit":
    case "repository":
      return <CodeHostToken token={token} links={links} />;
    case "key":
      return <KeyToken token={token} links={links} />;
    case "mention":
      return <MentionToken token={token} links={links} />;
  }
}

function AddressToken({
  token,
  links,
}: {
  token: Extract<Token, { kind: "address" }>;
  links: boolean;
}) {
  const hosts = useLinkHosts();
  const said = describeLink(token.href, hosts);
  // NO WEB ADDRESS (a `mailto:`): no app's and no host to show, so it is the
  // address as written, a link where links may be.
  if (said === null) {
    return <InlineRef href={links ? token.href : undefined}>{token.text}</InlineRef>;
  }
  return (
    <InlineRef
      mark={
        said.vendor ? <VendorMark vendor={said.vendor} data-vendor={said.vendor} /> : <GlobeGlyph />
      }
      href={links ? token.href : undefined}
      kind={said.kind ?? undefined}
      title={token.href}
      code={said.code}
    >
      {said.label}
    </InlineRef>
  );
}

function CodeHostToken({
  token,
  links,
}: {
  token: Extract<Token, { kind: "reference" | "commit" | "repository" }>;
  links: boolean;
}) {
  const hosts = useLinkHosts();
  const target = codeHostTarget(token, hosts);
  if (target === null) return token.text;
  return (
    <InlineRef
      mark={<VendorMark vendor={target.vendor} data-vendor={target.vendor} />}
      kind={CODE_HOST_NAMES[target.vendor]}
      href={links && target.href !== null ? target.href : undefined}
      title={target.href ?? undefined}
      code
    >
      {token.text}
    </InlineRef>
  );
}

/** Where a reference, a commit or a repository points, or null for words. */
function codeHostTarget(
  token: Extract<Token, { kind: "reference" | "commit" | "repository" }>,
  hosts: ReturnType<typeof useLinkHosts>,
) {
  switch (token.kind) {
    case "repository":
      return repositoryTarget(token.owner, token.repo, hosts);
    case "reference":
      return referenceTarget(token.path, token.sep, token.id, hosts);
    case "commit":
      // A COMMIT NAMED BY THE WORD ALONE ("commit 9fceb02") has no repository
      // to point at, and is drawn with the mark of the one code host there is.
      return token.after === null
        ? soleCodeHost(hosts)
        : referenceTarget(token.after.path, "@", token.sha, hosts);
  }
}

/** What a code host's mark says, read before its reference. */
const CODE_HOST_NAMES = { github: "GitHub", gitlab: "GitLab" } as const;

/** The company's one code host, as a commit with no repository is drawn with; null for none or two. */
function soleCodeHost(hosts: ReturnType<typeof useLinkHosts>) {
  const code = hosts.filter((h) => h.kind === "github" || h.kind === "gitlab");
  const only = code.length === 1 ? code[0] : undefined;
  return only ? { vendor: only.kind as "github" | "gitlab", href: null } : null;
}

function KeyToken({ token, links }: { token: Extract<Token, { kind: "key" }>; links: boolean }) {
  const projects = useProjectKeys();
  if (!projects.has(token.project)) return token.text;
  return (
    <InlineRef href={links ? href(itemPath({ id: token.text, key: token.text })) : undefined} code>
      {token.text}
    </InlineRef>
  );
}

/**
 * A handle, drawn from the chart in the store. A surface rendered with no
 * client (a suite, a page outside the app) has no chart to read, and a
 * handle there is its words, as a key with no projects and an address with no
 * hosts are.
 */
function MentionToken(props: { token: Extract<Token, { kind: "mention" }>; links: boolean }) {
  return useContext(ClientContext) === null ? props.token.text : <SeatMention {...props} />;
}

function SeatMention({
  token,
  links,
}: {
  token: Extract<Token, { kind: "mention" }>;
  links: boolean;
}) {
  const index = useOrgIndex();
  const seat = index.byHandle.get(token.handle);
  if (seat === undefined) return token.text;
  return (
    <InlineRef href={links ? href(seatPath(seat)) : undefined} title={seat.name}>
      {token.text}
    </InlineRef>
  );
}

/**
 * A link somebody labelled in Markdown (`[Brand tokens](https://...)`): its
 * words as written, after the mark of the app its ADDRESS belongs to, so a
 * label that names one place and an address that goes to another is drawn as
 * where it goes. No mark for an address of nobody we know: the words are the
 * link, and a globe before every one of them would say nothing.
 *
 * `live` false draws the words and the mark as no link: a model's link
 * (`links: "inert"`), whose address the renderer writes after it.
 */
export function LabelledLink({
  href,
  live,
  children,
}: {
  href: string;
  live: boolean;
  children?: ReactNode;
}) {
  const hosts = useLinkHosts();
  const said = describeLink(href, hosts);
  const vendor = said?.vendor ?? null;
  return (
    <InlineRef
      mark={vendor ? <VendorMark vendor={vendor} data-vendor={vendor} /> : undefined}
      kind={vendor ? (said?.kind ?? undefined) : undefined}
      href={live ? href : undefined}
      wrap
    >
      {children}
    </InlineRef>
  );
}
