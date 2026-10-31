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
 *  - A ROOM is a token only when the reader may read a room of that name
 *    (`lib/roomNames.ts`), and links to it, with the mark the assistant's
 *    room chip carries, so a room's chip and its later mentions in one answer
 *    read as one thing. A code span holding just a room's `#name` is code,
 *    never a swatch ([ColourCode]).
 *  - A MENTION links to the seat only when the chart holds that handle.
 *
 * A MENTION IS ITS WORDS, with no face: a face at the text's size before
 * every handle made a line of them hard to read, and the handle already says
 * who. Every other token keeps the mark that says what it is.
 *
 * Anything that resolves to nothing is its words, exactly as written.
 *
 * WHERE A TOKEN LINKS is the run's `LinkMode` (`lib/inlineTokens.ts`):
 * `text` draws every token as words with its mark (an Inbox row, an answer's
 * option), and `checked` (the assistant's answers, the Inbox, a coding run's
 * question) links only what the dashboard built: every token but an address
 * is built from checked parts already, and an address opens only as it is
 * rebuilt on the company's own host, else it is drawn inert, as written. An
 * address that is no run of prose (a chat message's links) is drawn by the
 * same rule ([AddressLink]).
 */

import { createContext, useContext, useMemo, type ReactNode } from "react";
import { ColorChip, Highlight, InlineCode, InlineRef } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import {
  GlobeGlyph,
  MessageSquareGlyph,
  SquareKanbanGlyph,
  type GlyphSize,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { appRoute } from "~/lib/appRoute.ts";
import {
  checksLinks,
  codeHostOwners,
  codeRoom,
  describeLink,
  inlineTokens,
  linkTarget,
  messageScope,
  NO_SCOPE,
  referenceTarget,
  repositoryTarget,
  type InlineToken as Token,
  type LinkDescription,
  type LinkMode,
  type MessageScope,
} from "~/lib/inlineTokens.ts";
import type { LinkHost } from "~/contract/integrations.ts";
import { useLinkHosts } from "~/lib/linkHosts.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { useProjectKeys } from "~/lib/projectKeys.ts";
import { useRoomNames } from "~/lib/roomNames.ts";
import { seatPath, useOrgIndex } from "~/lib/seats.ts";
import { itemPath } from "~/lib/work.ts";

/** The texts of the message a surface draws in several renders ([TokenScope]). */
const MessageTexts = createContext<readonly string[] | null>(null);

/**
 * One message drawn in several renders: an ask's question, its context, its
 * recommendation and its options are one message to the person reading
 * them, so a bare `#11` in the question is placed by a repository its
 * context names (`messageScope`). Only a render that says it is part of the
 * message (`RenderOptions.scoped`) reads `texts`; any other inside it (a
 * reply in the ask's conversation) is its own message.
 */
export function TokenScope({ texts, children }: { texts: readonly string[]; children: ReactNode }) {
  return <MessageTexts.Provider value={texts}>{children}</MessageTexts.Provider>;
}

/** The scopes read for each set of hosts, by the message's words. */
const scopes = new WeakMap<readonly LinkHost[], Map<string, MessageScope>>();
/**
 * How many messages' scopes are kept per set of hosts: a screen draws a few
 * dozen messages (an Inbox's rows, a thread's comments), and a page is one.
 * Past it the oldest is read again when it is drawn again.
 */
const SCOPES_KEPT = 128;

/**
 * A message's scope, READ ONCE for every run of it: a page of fifty
 * paragraphs is fifty runs of one message, and each reading the whole page
 * for itself read it fifty times.
 */
function scopeOf(texts: readonly string[], hosts: readonly LinkHost[]): MessageScope {
  const key = texts.join("\u0000");
  if (key === "") return NO_SCOPE;
  let kept = scopes.get(hosts);
  if (kept === undefined) {
    kept = new Map();
    scopes.set(hosts, kept);
  }
  const found = kept.get(key);
  if (found !== undefined) return found;
  const scope = messageScope(texts, hosts);
  if (kept.size >= SCOPES_KEPT) kept.delete(kept.keys().next().value!);
  kept.set(key, scope);
  return scope;
}

/**
 * One run of prose with its tokens drawn, the words between them marked where
 * a search matched (`highlight`).
 *
 * A COMPONENT, so the run is read once: the recogniser runs when the run's
 * words, the company's code host owners, the reader's rooms or what its
 * message names change, and a re-render of the surface around it (a page of
 * comments, an Inbox list ticking its times) reads nothing again. The owners
 * and the rooms come from the frame's readings (`link_hosts`,
 * `chat_room_names`), which a plain function of the text could not read.
 *
 * `message` is the message the run was rendered from; undefined, the run is
 * part of the enclosing [TokenScope]'s (or, with none, its own words).
 */
export function TokenRun({
  text,
  links,
  highlight,
  message,
}: {
  text: string;
  links: LinkMode;
  highlight?: readonly string[] | undefined;
  message: string | undefined;
}) {
  const hosts = useLinkHosts();
  const rooms = useRoomNames();
  const surface = useContext(MessageTexts);
  const owners = useMemo(() => codeHostOwners(hosts), [hosts]);
  const scope = scopeOf(message !== undefined ? [message] : (surface ?? [text]), hosts);
  const tokens = useMemo(
    () => inlineTokens(text, owners, scope, rooms),
    [text, owners, scope, rooms],
  );
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

export function InlineToken({ token, links }: { token: Token; links: LinkMode }) {
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
    case "room":
      return <RoomToken token={token} links={links} />;
    case "mention":
      return <MentionToken token={token} links={links} />;
  }
}

function AddressToken({
  token,
  links,
}: {
  token: Extract<Token, { kind: "address" }>;
  links: LinkMode;
}) {
  return <AddressLink address={token.href} text={token.text} links={links} />;
}

/**
 * An address drawn as the address token is, for one that is no run of prose:
 * a chat message's links (`routes/chatlet/MessageLinks.tsx`), each an address
 * on its own. `address` is the address [safeHref] kept, and `text` the words
 * it is shown by where it is drawn as written, the address itself unless
 * given.
 *
 * ONE DRAWING FOR AN ADDRESS, wherever it stands, so a structured link and the
 * same address in a sentence open by one rule: the mark of the app its host
 * belongs to, and under `checked` a link only where the dashboard rebuilt it
 * on a host the company configured, otherwise the address inert, whole and
 * with its title.
 */
export function AddressLink({
  address,
  text = address,
  links,
}: {
  address: string;
  text?: string;
  links: LinkMode;
}) {
  const hosts = useLinkHosts();
  // ONE OF THIS DASHBOARD'S OWN PAGES opens in place under `own`, rebuilt
  // from its path alone (`lib/appRoute.ts`). Left to the host rules below, a
  // Copy link pasted into a room was drawn inert.
  const own = links === "own" ? appRoute(address) : null;
  if (own !== null) {
    return (
      <InlineRef href={own.canonical} title={address}>
        {text}
      </InlineRef>
    );
  }
  const said = describeLink(address, hosts);
  // NO WEB ADDRESS (a `mailto:`): no app's and no host to show, so it is the
  // address as written, a link only where any link may be.
  if (said === null) {
    return <InlineRef href={links === "anchor" ? address : undefined}>{text}</InlineRef>;
  }
  // CHECKED AND NOT THE COMPANY'S: the address as written ([InertAddress]).
  if (checksLinks(links) && said.canonical === null) {
    return <InertAddress said={said} raw={address} written={text} />;
  }
  const to = links === "text" ? undefined : checksLinks(links) ? said.canonical! : address;
  return (
    <InlineRef
      mark={addressMark(said)}
      href={to}
      kind={said.kind ?? undefined}
      title={address}
      code={said.code}
    >
      {said.label}
    </InlineRef>
  );
}

/**
 * The mark an address is drawn with: the mark of the app it belongs to, or a
 * globe for an address of nobody we know. `size` for a control's icon (an
 * evidence button); in a sentence the mark takes `InlineRef`'s square.
 */
export function addressMark(said: LinkDescription, size?: GlyphSize): ReactNode {
  return said.vendor ? (
    <VendorMark vendor={said.vendor} size={size} data-vendor={said.vendor} />
  ) : (
    <GlobeGlyph size={size} />
  );
}

/**
 * An address that opens nothing: its mark, what the mark means said to a
 * screen reader, and the address `written` as its writer wrote it, whole, so a
 * reader sees and can copy exactly where it would go. Never a control and
 * never a label the dashboard did not build: under `checked`, every address
 * that is not rebuilt on the company's own host is drawn this way, in prose,
 * as an ask's evidence and as a chat message's link alike.
 */
export function InertAddress({
  said,
  raw,
  written,
}: {
  said: LinkDescription;
  raw: string;
  written: string;
}) {
  return (
    <InlineRef mark={addressMark(said)} kind={said.kind ?? undefined} title={raw}>
      {written}
    </InlineRef>
  );
}

function CodeHostToken({
  token,
  links,
}: {
  token: Extract<Token, { kind: "reference" | "commit" | "repository" }>;
  links: LinkMode;
}) {
  const hosts = useLinkHosts();
  const target = codeHostTarget(token, hosts);
  if (target === null) return token.text;
  return (
    <InlineRef
      mark={<VendorMark vendor={target.vendor} data-vendor={target.vendor} />}
      kind={CODE_HOST_NAMES[target.vendor]}
      href={links !== "text" && target.href !== null ? target.href : undefined}
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

function KeyToken({ token, links }: { token: Extract<Token, { kind: "key" }>; links: LinkMode }) {
  const projects = useProjectKeys();
  if (!projects.has(token.project)) return token.text;
  return (
    <InlineRef
      mark={<SquareKanbanGlyph />}
      kind="Task"
      href={links !== "text" ? href(itemPath({ id: token.text, key: token.text })) : undefined}
      code
    >
      {token.text}
    </InlineRef>
  );
}

/**
 * A room the reader may read, linked to it: a route the dashboard builds from
 * a name the engine said is a room, so it is live under `checked` as a key and
 * a handle are.
 */
function RoomToken({ token, links }: { token: Extract<Token, { kind: "room" }>; links: LinkMode }) {
  const id = useRoomNames().get(token.name);
  if (id === undefined) return token.text;
  return (
    <InlineRef
      mark={<MessageSquareGlyph />}
      kind="Room"
      href={links !== "text" ? href(["chatlet", id]) : undefined}
    >
      {token.text}
    </InlineRef>
  );
}

/**
 * A code span holding one colour and nothing else (`codeColour`), drawn with a
 * swatch of it, unless it is one of the reader's rooms' `#name` (`#e2e`),
 * which is code naming the room (`lib/inlineTokens.ts`, rule 1).
 *
 * A COMPONENT, because the rooms are the frame's reading, which the Markdown
 * renderer's plain walk cannot read.
 */
export function ColourCode({ code, colour }: { code: string; colour: string }) {
  const rooms = useRoomNames();
  if (codeRoom(code, rooms)) return <InlineCode whole>{code}</InlineCode>;
  return (
    <InlineCode whole>
      {/* THE SAME MARK BOX as a colour in words, so the two swatches stand at
          one height and both say "Colour" to a screen reader. */}
      <InlineRef kind="Colour" mark={<ColorChip value={colour} size="text" shape="square" />}>
        {code}
      </InlineRef>
    </InlineCode>
  );
}

/**
 * A handle, drawn from the chart in the store. A surface rendered with no
 * client (a suite, a page outside the app) has no chart to read, and a
 * handle there is its words, as a key with no projects and an address with no
 * hosts are.
 */
function MentionToken(props: { token: Extract<Token, { kind: "mention" }>; links: LinkMode }) {
  return useContext(ClientContext) === null ? props.token.text : <SeatMention {...props} />;
}

function SeatMention({
  token,
  links,
}: {
  token: Extract<Token, { kind: "mention" }>;
  links: LinkMode;
}) {
  const index = useOrgIndex();
  const seat = index.byHandle.get(token.handle);
  if (seat === undefined) return token.text;
  return (
    <InlineRef href={links !== "text" ? href(seatPath(seat)) : undefined} title={seat.name}>
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
 * Under `checked` it opens only the address rebuilt on the company's own host
 * ([LinkDescription.canonical]); any other is drawn inert, its words and then
 * the host and path it names, so a reader sees where it would have gone and
 * can copy it. An app route a writer spelled (`#/settings/...`) is inert there
 * too: the dashboard's own links into itself are the ones it builds, a key's
 * and a handle's, never one a model or an agent chose.
 */
export function LabelledLink({
  href,
  mode,
  children,
}: {
  href: string;
  mode: Exclude<LinkMode, "text">;
  children?: ReactNode;
}) {
  const hosts = useLinkHosts();
  const said = describeLink(href, hosts);
  const vendor = said?.vendor ?? null;
  // A LINK TO ONE OF THIS DASHBOARD'S OWN PAGES (a page link a writer
  // inserted, `#/doclet/pages/…`) opens in place under `own`, rebuilt from its
  // path alone (`lib/appRoute.ts`); it was drawn inert, because the host rules
  // below know only other apps' hosts.
  const own = mode === "own" ? appRoute(href) : null;
  const to = mode === "anchor" ? href : (own?.canonical ?? said?.canonical ?? null);
  const shown = to === null ? linkTarget(href) : null;
  return (
    <>
      <InlineRef
        mark={vendor ? <VendorMark vendor={vendor} data-vendor={vendor} /> : undefined}
        kind={vendor ? (said?.kind ?? undefined) : undefined}
        href={to ?? undefined}
        title={to === null ? href : undefined}
        wrap
      >
        {children}
      </InlineRef>
      {shown === null ? null : <span className="md-link-inert-host">{` (${shown})`}</span>}
    </>
  );
}
