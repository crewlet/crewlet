/**
 * The picked run's actions, in the gutter by its first line: copy its lines, copy a
 * link to them at the commit, or blame them (from a blame, show their code).
 */
import { Menu, useToast } from "@crewlethq/ui";
import {
  CopyGlyph,
  EllipsisGlyph,
  FileCodeGlyph,
  LinkGlyph,
  RotateCcwClockGlyph,
} from "@crewlethq/icons/glyphs";
import { linesValue, type LineRun } from "~/lib/code.ts";
import { copyLink, copyText } from "~/lib/pageLink.ts";
import { repoHref } from "../paths.ts";
import { WAITING_FOR_COMMIT } from "./FileActions.tsx";

/** What the run's menu offers, each held with its reason where it cannot act. */
export interface LineAction {
  key: "copy" | "link" | "lens";
  label: string;
  held?: string;
}

/** The lines on screen: the first one's number and each line's text, undefined where a blame carries none. */
export interface DrawnLines {
  from: number;
  texts: readonly (string | undefined)[];
}

/** The run's text where every line of it is drawn with its text, else why not. */
export function runText(picked: LineRun, drawn: DrawnLines): { text: string } | { held: string } {
  const { start, end } = picked;
  const one = start === end;
  if (start < drawn.from || end >= drawn.from + drawn.texts.length) {
    return {
      held: one
        ? `Line ${start} is not loaded: load it to copy it.`
        : `Lines ${start} to ${end} are not all loaded: load them to copy them.`,
    };
  }
  const texts = drawn.texts.slice(start - drawn.from, end - drawn.from + 1);
  const missing = texts.findIndex((t) => t === undefined);
  if (missing >= 0) {
    return {
      held: `Line ${start + missing} is too long for a blame to carry: copy it from Code.`,
    };
  }
  return { text: texts.join("\n") };
}

/** The run's actions, pure: Copy lines, Copy a link to these lines, and the other lens. */
export function lineActionItems(
  picked: LineRun,
  drawn: DrawnLines,
  commit: string,
  blame: boolean,
): LineAction[] {
  const one = picked.start === picked.end;
  const copied = runText(picked, drawn);
  return [
    {
      key: "copy",
      label: one ? "Copy line" : "Copy lines",
      ...("held" in copied ? { held: copied.held } : {}),
    },
    {
      key: "link",
      label: one ? "Copy a link to this line" : "Copy a link to these lines",
      ...(commit ? {} : { held: WAITING_FOR_COMMIT }),
    },
    {
      key: "lens",
      label: blame
        ? one
          ? "Show this line's code"
          : "Show these lines' code"
        : one
          ? "Blame this line"
          : "Blame these lines",
    },
  ];
}

const GLYPHS = {
  copy: <CopyGlyph size="sm" />,
  link: <LinkGlyph size="sm" />,
  blame: <RotateCcwClockGlyph size="sm" />,
  code: <FileCodeGlyph size="sm" />,
};

/** "Actions for lines 17 to 19": the kit's menu on the run's first line. */
export function LineActions({
  repo,
  path,
  commit,
  picked,
  drawn,
  blame,
  onLens,
}: {
  repo: string;
  path: string;
  /** The commit the file was read at, whole, or "" while it is read. */
  commit: string;
  picked: LineRun;
  drawn: DrawnLines;
  blame: boolean;
  onLens: (blame: boolean) => void;
}) {
  const toast = useToast();
  const { start, end } = picked;
  const which = start === end ? `line ${start}` : `lines ${start} to ${end}`;
  const said = (word: "Copied" | "Blocked", what: string) =>
    word === "Copied"
      ? toast.ok(`Copied ${what}`)
      : toast.show({ variant: "warning", message: "The browser refused the clipboard" });
  const act: Record<LineAction["key"], () => void> = {
    copy: () => {
      const copied = runText(picked, drawn);
      if ("text" in copied) void copyText(copied.text).then((word) => said(word, which));
    },
    link: () =>
      void copyLink(
        repoHref(repo, {
          ref: commit,
          path,
          lines: linesValue(picked),
          ...(blame ? { lens: "blame" as const } : {}),
        }),
      ).then((word) => said(word, `a link to ${which}`)),
    lens: () => onLens(!blame),
  };
  return (
    <Menu
      label={`Actions for ${which}`}
      icon={<EllipsisGlyph size="sm" />}
      triggerVariant="ghost"
      align="start"
      items={lineActionItems(picked, drawn, commit, blame).map((item) => ({
        key: item.key,
        label: item.label,
        icon: item.key === "lens" ? (blame ? GLYPHS.code : GLYPHS.blame) : GLYPHS[item.key],
        onSelect: act[item.key],
        ...(item.held ? { disabled: true, description: item.held } : {}),
      }))}
    />
  );
}
