/**
 * The marks every Gitlet screen draws the same way: a commit's id, a branch,
 * who did something and when, what a directory entry is, and how a repository
 * is cloned.
 *
 * # Why they are one module
 *
 * A code host is read by its small marks — a commit's seven characters, the
 * face beside a name, the folder beside a directory — and a reader learns
 * them once. Drawn per screen, the history drew a commit's id one way and
 * the blame another, and a seat was a face on one screen and a bare name on
 * the next. So each mark is written here, once, and every screen draws it.
 *
 * ONLY A BRANCH IS COLOURED, in the kit's code-ref chip (the seventh
 * exception). A seat's face is its own profile icon (the one rule's third
 * exception), a person's their initials on the neutral badge; a directory and
 * a file are glyphs, told apart by their drawing and their name, never by a
 * hue (`docs/reference/dashboard-design.md`, "The one rule").
 */

import "~/styles/gitlet.css";

import type { ReactNode } from "react";
import { Button, Copyable, InlineCode, Popover } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  FileCodeGlyph,
  FileTextGlyph,
  FolderGlyph,
  GitBranchGlyph,
  LinkGlyph,
  PackageGlyph,
  SquareTerminalGlyph,
} from "@crewlethq/icons/glyphs";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { refName, shortOID } from "~/lib/code.ts";
import { ago } from "~/lib/ago.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import type { CodeTreeEntry } from "~/protocol/index.ts";

/**
 * A commit's id: its first characters, and a press that copies the whole of
 * it. The id is never the link to the commit — the subject beside it is —
 * so the chip is one control, the copy.
 */
export function ShaChip({ oid }: { oid: string }) {
  return (
    <Copyable
      className="code-sha"
      value={oid}
      display={shortOID(oid)}
      monospace
      ariaLabel={`Copy the commit id ${shortOID(oid)}`}
      title={`Copy ${oid}`}
    />
  );
}

/**
 * Who someone is, as a face: a seat by its handle or its name, off the chart —
 * an agent as its own character, a person as initials on the neutral badge —
 * or, for a name the chart does not hold (a git author line written by
 * somebody's own git), that name's initials. Decorative: the name is always
 * printed beside it.
 */
export function Face({
  who,
  size = "xs",
}: {
  who: string;
  /** A step, or `text`: a face inside a line of code or prose, sized by the words around it. */
  size?: "xs" | "sm" | "md" | "text";
}) {
  const badge = useSeatBadgeOf()(who);
  return (
    <SeatAvatar
      name={badge.name || who}
      size={size}
      kind={badge.kind === "human" ? "human" : "agent"}
      avatar={badge.avatar}
      decorative
    />
  );
}

/** A seat's name off the chart, or the words it was given where the chart has none. */
export function useWhoName(): (who: string) => string {
  const badge = useSeatBadgeOf();
  return (who) => badge(who).name || who;
}

/**
 * Who did something, and when: a face, the name, what they did, and how long
 * ago — its whole instant the time's title. `who` is a seat's handle or a
 * name; `verb` is the rest of the sentence ("pushed revision 2").
 */
export function Byline({
  who,
  verb,
  at,
  now,
  children,
}: {
  who: string;
  verb?: ReactNode;
  at?: string | undefined;
  now: number;
  /** Anything said after the time: a commit's id, a link. */
  children?: ReactNode;
}) {
  const name = useWhoName()(who);
  return (
    <span className="code-byline">
      <Face who={who} />
      <span className="code-byline__who">{name}</span>
      {verb && <span className="code-byline__verb">{verb}</span>}
      {at && (
        <time className="code-byline__when" dateTime={at} title={fmtDateTime(at)}>
          {ago(at, now, { clock: true })}
        </time>
      )}
      {children}
    </span>
  );
}

/** What a directory entry is, in a word, by git's mode. */
export function entryKind(e: Pick<CodeTreeEntry, "mode">): string {
  switch (e.mode) {
    case "040000":
      return "Directory";
    case "100755":
      return "Executable";
    case "120000":
      return "Link";
    case "160000":
      return "Submodule";
  }
  return "File";
}

/** The names a README goes by, the one a reader writes first first. */
export const README_NAMES = ["readme.md", "readme.markdown", "readme", "readme.txt"];

/** Prose a reader reads rather than runs: a README, a licence, a change log, a note. */
const PROSE_FILE =
  /(^|\/)(readme|licen[cs]e|copying|changelog|notice|authors)(\.[^/]*)?$|\.(md|markdown|txt|rst|adoc)$/i;

/**
 * An entry's glyph: a folder, a submodule's package, a link, a script the
 * mode makes executable, prose, or code — told apart by drawing, never by
 * hue, and decorative, since the entry's name and its kind are said beside
 * it.
 */
export function EntryGlyph({ entry }: { entry: Pick<CodeTreeEntry, "mode" | "path"> }) {
  switch (entry.mode) {
    case "040000":
      return <FolderGlyph size="sm" className="code-glyph code-glyph--dir" />;
    case "160000":
      return <PackageGlyph size="sm" className="code-glyph" />;
    case "120000":
      return <LinkGlyph size="sm" className="code-glyph" />;
    case "100755":
      return <SquareTerminalGlyph size="sm" className="code-glyph" />;
  }
  if (/(^|\/)readme(\.[^/]*)?$/i.test(entry.path)) {
    return <BookOpenGlyph size="sm" className="code-glyph" />;
  }
  return PROSE_FILE.test(entry.path) ? (
    <FileTextGlyph size="sm" className="code-glyph" />
  ) : (
    <FileCodeGlyph size="sm" className="code-glyph" />
  );
}

/**
 * The address a person clones a repository from: the API's own origin — the
 * one this dashboard is served from, since the engine serves `/git/` beside
 * it and never on the public listener — and `/git/{name}.git`.
 */
export function cloneURL(repo: string, origin: string = location.origin): string {
  return `${origin}/git/${repo}.git`;
}

/**
 * Clone: the repository's HTTPS address with its copy, and what git asks for
 * — the person's own token as the password, minted by `crewlet iam token
 * -login`, which `crewlet gitlet credential` hands git from the environment.
 * The clone is git's, from the reader's own machine: nothing here writes.
 */
export function CloneButton({ repo }: { repo: string }) {
  const url = cloneURL(repo);
  return (
    <Popover
      label={`Clone ${repo}`}
      align="end"
      trigger={(open, toggle) => (
        <Button size="small" variant="primary" aria-expanded={open} onClick={toggle}>
          Clone
        </Button>
      )}
    >
      <div className="code-clone">
        <p className="code-clone__head">Clone with HTTPS</p>
        <Copyable
          className="code-clone__url"
          value={url}
          variant="block"
          monospace
          ariaLabel={`Copy ${url}`}
          title="Copy the address"
        />
        <p className="code-clone__note">
          git asks for a password: give it your own token, which{" "}
          <InlineCode>crewlet iam token -login</InlineCode> mints. To have git never ask, make{" "}
          <InlineCode>crewlet gitlet credential</InlineCode> its credential helper.
        </p>
      </div>
    </Popover>
  );
}

/**
 * A branch as Gitlet names it: the kit's code-ref chip with its glyph (the
 * seventh exception), a link where the host gives one, which keeps `.prose-link`.
 */
export function BranchChip({ name, href }: { name: string; href?: string | undefined }) {
  const inner = (
    <>
      <GitBranchGlyph size="xs" />
      {name}
    </>
  );
  return href ? (
    <a className="code-branch-chip prose-link" href={href}>
      {inner}
    </a>
  ) : (
    <span className="code-branch-chip">{inner}</span>
  );
}

/**
 * A ref by its short name: a branch as its chip, a tag or any other ref in
 * the code face.
 */
export function RefName({ gitRef }: { gitRef: string }) {
  const ref = refName(gitRef);
  return ref.kind === "branch" ? (
    <BranchChip name={ref.name} />
  ) : (
    <span className="mono">{ref.name}</span>
  );
}
