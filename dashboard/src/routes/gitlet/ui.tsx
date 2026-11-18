/**
 * The marks every Gitlet screen draws the same way: a commit's id, a branch,
 * who did something and when, what a directory entry is; a check's verdict, a tag.
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
 * exception); faces, directories and files are told apart by drawing and name,
 * never by hue (`docs/reference/dashboard-design.md`, "The one rule").
 */

import "~/styles/gitlet.css";

import { Fragment, type ComponentType, type ReactNode } from "react";
import { Copyable, Spinner, Tag, VisuallyHidden, type AvatarVerdict } from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";
import {
  BookOpenGlyph,
  CircleCheckGlyph,
  CircleDashedGlyph,
  CircleStopGlyph,
  CircleXGlyph,
  FileCodeGlyph,
  FileTextGlyph,
  FolderGlyph,
  GitBranchGlyph,
  LinkGlyph,
  PackageGlyph,
  SquareTerminalGlyph,
  TagGlyph,
  type GlyphProps,
} from "@crewlethq/icons/glyphs";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { refName, shortOID } from "~/lib/code.ts";
import { ago } from "~/lib/ago.ts";
import { rollupWord } from "~/lib/checks.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import type { CodeCheckRollup, CodeTreeEntry } from "~/protocol/index.ts";
import { repoHref } from "./paths.ts";

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

/** Gitlet itself as a writer: a merge commit it built, a branch it moved. */
export const GITLET = Object.freeze({ product: "gitlet" as const });

/** Who did something: a seat's handle, a name, or Gitlet itself. */
export type Who = string | typeof GITLET;

/** The product mark's size at each face's: the kit avatar's 20, 26 and 32 px, or the words'. */
const MARK_SIZE = { xs: 20, sm: 26, md: 32, text: "1.15em" } as const;

/**
 * Who someone is, as a face: a seat by its handle or its name, off the chart —
 * an agent as its own character, a person as initials on the neutral badge —
 * or, for a name the chart does not hold (a git author line written by
 * somebody's own git), that name's initials; Gitlet, its product mark.
 * Decorative: the name is always printed beside it.
 */
export function Face({
  who,
  size = "xs",
  verdict,
  ring,
}: {
  who: Who;
  /** A step, or `text`: a face inside a line of code or prose, sized by the words around it. */
  size?: "xs" | "sm" | "md" | "text";
  /** A reviewer's verdict on the kit's corner, in a change's row alone. */
  verdict?: AvatarVerdict | undefined;
  /** The reader's own face. */
  ring?: "brand" | undefined;
}) {
  const badge = useSeatBadgeOf();
  if (typeof who !== "string") {
    return (
      <span className="code-face" aria-hidden="true">
        <ProductMark
          product={who.product}
          size={MARK_SIZE[size]}
          drawing={size === "text" ? "small" : "auto"}
        />
      </span>
    );
  }
  const seat = badge(who);
  return (
    <SeatAvatar
      name={seat.name || who}
      size={size}
      kind={seat.kind === "human" ? "human" : "agent"}
      avatar={seat.avatar}
      verdict={verdict}
      ring={ring}
      decorative
    />
  );
}

/** A seat's name off the chart, the words it was given where the chart has none, or "Gitlet". */
export function useWhoName(): (who: Who) => string {
  const badge = useSeatBadgeOf();
  return (who) => (typeof who === "string" ? badge(who).name || who : "Gitlet");
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
  who: Who;
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

/** The names a licence goes by, in the order a root is searched for one. */
export const LICENSE_NAMES = [
  "license",
  "license.md",
  "license.txt",
  "licence",
  "licence.md",
  "copying",
];

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
 * A branch as Gitlet names it: the kit's code-ref chip with its glyph (the
 * seventh exception), a link where the host gives one, which keeps `.prose-link`.
 */
export function BranchChip({ name, href }: { name: string; href?: string | undefined }) {
  const inner = (
    <>
      <GitBranchGlyph size="xs" />
      <span>
        <SlashBreaks text={name} />
      </span>
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
 * A branch or a path that wraps after its slashes: a line break offered after
 * each `/`, so a narrow column breaks it between its parts, never mid-word.
 */
export function SlashBreaks({ text }: { text: string }) {
  const parts = text.split("/");
  return (
    <>
      {parts.map((part, i) => (
        <Fragment key={i}>
          {part}
          {i < parts.length - 1 && (
            <>
              /<wbr />
            </>
          )}
        </Fragment>
      ))}
    </>
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

/** Each rollup state's glyph and ink: a cross only for a failure, never for an absence. */
export const ROLLUP_LOOK: Readonly<
  Record<string, { glyph: ComponentType<GlyphProps> | "spinner"; tone: string }>
> = {
  passed: { glyph: CircleCheckGlyph, tone: "success" },
  failing: { glyph: CircleXGlyph, tone: "danger" },
  running: { glyph: "spinner", tone: "info" },
  pending: { glyph: CircleDashedGlyph, tone: "neutral" },
  cancelled: { glyph: CircleStopGlyph, tone: "neutral" },
};

/**
 * Where a commit's checks stand: a glyph and a word, toned by the rollup's state.
 * `of` adds the required count to a pass: "Passed 2 of 2"; `bare` draws the glyph alone.
 */
export function CheckRollupMark({
  rollup,
  title,
  of = false,
  bare = false,
}: {
  rollup: CodeCheckRollup;
  title: string;
  of?: boolean;
  bare?: boolean;
}) {
  const look = ROLLUP_LOOK[rollup.state];
  const Glyph = look?.glyph;
  const count =
    of && rollup.state === "passed" && rollup.required > 0
      ? ` ${rollup.required_passed} of ${rollup.required}`
      : "";
  return (
    <span className="code-check-mark" data-tone={look?.tone ?? "neutral"} title={title}>
      {Glyph === "spinner" ? <Spinner size="sm" /> : Glyph && <Glyph size="sm" aria-hidden />}
      {bare ? (
        <VisuallyHidden>{rollupWord(rollup)}</VisuallyHidden>
      ) : (
        <span className="code-check-mark__word">
          {rollupWord(rollup)}
          {count}
        </span>
      )}
    </span>
  );
}

/**
 * A tag naming a commit: its glyph and name on the neutral outline, opening the
 * repository's files at its whole ref, which no branch of the same name can shadow.
 */
export function TagChip({ repo, gitRef }: { repo: string; gitRef: string }) {
  const name = refName(gitRef).name;
  return (
    <a
      className="code-tag-chip"
      href={repoHref(repo, { ref: gitRef })}
      title={`Browse the files at ${name}`}
    >
      <Tag appearance="outline" size="xs" leadingIcon={<TagGlyph size="xs" />}>
        {name}
      </Tag>
    </a>
  );
}
