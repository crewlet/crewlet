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
 *  - A KEY links to its work item only when its project is one of the
 *    company's (`lib/projectKeys.ts`): `ISO-8601` is a key's shape and a word.
 *  - A MENTION is drawn with its seat's face and links to the seat only when
 *    the chart holds that handle.
 *
 * Anything that resolves to nothing is its words, exactly as written.
 *
 * `links` false draws every token as words with its mark: inside a control or
 * a link (an Inbox row, an answer's option, a link's own label), where a link
 * is invalid or steals the press, and in a model's prose (`links: "inert"`),
 * where nothing the model wrote may become a link.
 */

import { useContext, type ReactNode } from "react";
import { ColorChip, InlineRef } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { GlobeGlyph, SquareKanbanGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { describeLink, referenceTarget, type InlineToken as Token } from "~/lib/inlineTokens.ts";
import { useLinkHosts } from "~/lib/linkHosts.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { useProjectKeys } from "~/lib/projectKeys.ts";
import { seatPath, useOrgIndex } from "~/lib/seats.ts";
import { itemPath } from "~/lib/work.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";

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
  token: Extract<Token, { kind: "reference" | "commit" }>;
  links: boolean;
}) {
  const hosts = useLinkHosts();
  const at =
    token.kind === "reference"
      ? { path: token.path, sep: token.sep, id: token.id }
      : token.after === null
        ? null
        : { path: token.after.path, sep: "@" as const, id: token.sha };
  // A COMMIT NAMED BY THE WORD ALONE ("commit 9fceb02") has no repository to
  // point at, and is drawn with the mark of the one code host there is.
  const target = at ? referenceTarget(at.path, at.sep, at.id, hosts) : soleCodeHost(hosts);
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
    <InlineRef
      mark={<SquareKanbanGlyph />}
      kind="Task"
      href={links ? href(itemPath({ id: token.text, key: token.text })) : undefined}
      code
    >
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
    <InlineRef
      markShape="face"
      mark={
        <SeatAvatar name={seat.name} kind={seat.kind} avatar={seat.avatar} size="text" decorative />
      }
      kind={seat.kind === "human" ? "Person" : "Agent"}
      href={links ? href(seatPath(seat)) : undefined}
      title={seat.name}
    >
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
