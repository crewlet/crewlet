/**
 * A person's viewed files on a change (`code_mark_viewed`): the Viewed toggle on each file's
 * head, and the marks read beside the listing. A mark names a file at the state shown, so a
 * file a later revision changes reads unviewed again; nothing moves until the engine answers.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import { CheckGlyph, SquareGlyph } from "@crewlethq/icons/glyphs";
import { WriteButton } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import { changeLive } from "~/lib/code.ts";
import type { CodeChangeDetail, CodeDiffFile } from "~/protocol/index.ts";
import { partRefusal, refusedWords, usePagedViewed } from "./reads.ts";
import { markable, viewedFiles, viewedKeyValid } from "./viewed.ts";

/** Where the reader's marks stand: still read, read to their last page, or refused with why. */
export type MarksRead = "reading" | "read" | { refused: string };

/** The reader's marks on the listed files, and what a toggle needs to press one. */
export interface ViewedMarks {
  /** The change's ref, which a mark names. */
  change: string;
  /** The listed files marked at the state shown, by path. */
  viewed: ReadonlySet<string>;
  /** The files pressed, by path, with the state asked for, until the read says it. */
  pending: ReadonlyMap<string, boolean>;
  read: MarksRead;
  /** Why a file's toggle is held, before any press; undefined where it may be pressed. */
  held: (f: CodeDiffFile) => string | undefined;
  /** A press the engine took: wait for the marks read to say `viewed`. */
  marked: (path: string, viewed: boolean) => void;
  /** Gitlet's position of the files listed, which a mark is made at. */
  seen: string;
}

/** The read's sentence where an older fleet does not answer the marks. */
export const VIEWED_UNAVAILABLE = "Viewed files are unavailable on this fleet yet.";

/** The reader's marks on `change`, followed to their last page, against the files listed. */
export function useViewedMarks({
  change,
  listed,
  seen,
}: {
  change: CodeChangeDetail;
  listed: readonly CodeDiffFile[];
  seen: string;
}): ViewedMarks {
  const viewer = useViewer();
  const paged = usePagedViewed(change);
  const key = paged.answers.find((a) => a.viewed_key)?.viewed_key;
  const refused = partRefusal(paged);
  const read: MarksRead = useMemo(() => {
    if (refused !== null) {
      return {
        refused:
          paged.error === "bad_params" || paged.error === "unavailable"
            ? VIEWED_UNAVAILABLE
            : refusedWords("Viewed files could not be read", refused),
      };
    }
    if (paged.pageError !== null) return { refused: "Viewed files could not be read." };
    if (paged.answers.length === 0 || paged.more) return "reading";
    if (paged.items.length > 0 && !(key && viewedKeyValid(key))) {
      return {
        refused: "Viewed files could not be read: the repository's key is not one Gitlet writes.",
      };
    }
    return "read";
  }, [refused, paged.error, paged.pageError, paged.answers.length, paged.more, paged.items, key]);
  const viewed = useMemo(
    () => (read === "read" ? viewedFiles(key, paged.items, listed) : new Set<string>()),
    [read, key, paged.items, listed],
  );
  const [pending, setPending] = useState<ReadonlyMap<string, boolean>>(() => new Map());
  // A PRESS IS PENDING until the read shows the state asked for, or the read is refused.
  useEffect(() => {
    setPending((was) => {
      if (was.size === 0 || read === "reading") return was;
      if (typeof read === "object") return new Map();
      const now = new Map([...was].filter(([path, want]) => viewed.has(path) !== want));
      return now.size === was.size ? was : now;
    });
  }, [viewed, read]);
  const { refresh } = paged;
  const marked = useCallback(
    (path: string, want: boolean) => {
      setPending((was) => new Map(was).set(path, want));
      refresh();
    },
    [refresh],
  );
  const live = changeLive(change.state);
  const held = useCallback(
    (f: CodeDiffFile): string | undefined => {
      if (!live) {
        return `${change.ref} is ${change.state}: a change keeps viewed files only while it is a draft or open.`;
      }
      if (typeof read === "object") return read.refused;
      if (read === "reading") return "Still reading your viewed files.";
      if (f.path_b64) return "Only a file whose path is text can be marked viewed.";
      if (!markable(f)) return "Gitlet names no state of this file to mark.";
      if (!viewer.owner) return "Viewed files are a person's: this sign-in is bound to no seat.";
      return undefined;
    },
    [live, change.ref, change.state, read, viewer.owner],
  );
  const ref = change.ref;
  return useMemo(
    () => ({ change: ref, viewed, pending, read, held, marked, seen }),
    [ref, viewed, pending, read, held, marked, seen],
  );
}

/** The marks each toggle reads, so a new read redraws the toggles and not their sections. */
export const ViewedMarksContext = createContext<ViewedMarks | null>(null);

/** A file's Viewed toggle: pressed while the reader's mark names the state shown. */
export function ViewedToggle({ file }: { file: CodeDiffFile }) {
  const marks = useContext(ViewedMarksContext);
  const write = useAct("code_mark_viewed");
  if (!marks) return null;
  const { path } = file;
  const viewed = marks.viewed.has(path);
  const press = async () => {
    const mark =
      file.status === "D"
        ? { path }
        : { path, blob_oid: file.new_oid ?? "", mode: file.new_mode ?? "" };
    const result = await write.run(
      {
        change: marks.change,
        ...(viewed ? { unviewed: [path] } : { viewed: [mark] }),
        ...(marks.seen ? { seen: marks.seen } : {}),
      },
      { done: viewed ? `${path} is not viewed` : `${path} viewed` },
    );
    if (result?.kind === "applied" || result?.kind === "pending") marks.marked(path, !viewed);
  };
  return (
    <WriteButton
      write={write}
      size="small"
      variant="secondary"
      pressed={viewed}
      leadingIcon={viewed ? <CheckGlyph size="sm" /> : <SquareGlyph size="sm" />}
      aria-label={`Viewed: ${path}`}
      pressing={write.busy || marks.pending.has(path)}
      blocked={marks.held(file)}
      onPress={() => void press()}
    >
      Viewed
    </WriteButton>
  );
}
