/**
 * A reader's Home board, kept on the engine under their seat so every browser
 * draws the same one; this browser keeps the last answer for a cold load.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useToast } from "@crewlethq/ui";
import { readerStore } from "~/lib/readerStore.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { HomeBoardAnswer } from "~/protocol/index.ts";
import { validBoard, type Board } from "./model.ts";

/** The most a saved board may be as JSON: the engine's `coord.MaxHomeBoardBytes`. */
export const MAX_BOARD_BYTES = 524_288;

/** What this browser holds for a reader, stored as it is written here. */
export type Local =
  | { kind: "none" }
  /** The engine's last answer, drawn on a cold load until it answers again. */
  | { kind: "cache"; v: 1; seat: string; revision: number; board: Board }
  /** A board kept here alone: a seatless sign-in's, or a bare one an earlier build kept. */
  | { kind: "browser"; board: Board };

const NONE: Local = { kind: "none" };

/** What storage holds, read as untrusted input. */
export function localOf(raw: unknown, fallback: Board): Local {
  if (typeof raw !== "object" || raw === null) return NONE;
  const r = raw as Record<string, unknown>;
  if (Array.isArray(r.widgets)) return { kind: "browser", board: validBoard(r, fallback) };
  if (r.kind === "browser") return { kind: "browser", board: validBoard(r.board, fallback) };
  if (
    r.v === 1 &&
    typeof r.seat === "string" &&
    r.seat !== "" &&
    typeof r.revision === "number" &&
    Number.isSafeInteger(r.revision) &&
    r.revision > 0
  ) {
    return {
      kind: "cache",
      v: 1,
      seat: r.seat,
      revision: r.revision,
      board: validBoard(r.board, fallback),
    };
  }
  return NONE;
}

/** The engine's answer, or null where it is not one this build can read. */
export function answerOf(data: HomeBoardAnswer | null): HomeBoardAnswer | null {
  if (data === null || typeof data !== "object") return null;
  const { seat, revision } = data;
  if (typeof seat !== "string" || typeof revision !== "number" || !Number.isSafeInteger(revision)) {
    return null;
  }
  return { seat, board: data.board ?? null, revision };
}

/** Where the board drawn now comes from, and the revision a save names. */
export interface Drawn {
  board: Board;
  /** The revision a save from here names: 0 where none is stored. */
  revision: number;
  /** "engine" under a seat, "browser" for a seatless sign-in, "unknown" before the engine answers. */
  home: "engine" | "browser" | "unknown";
  /** A board kept here alone that the engine has room for: move it there. */
  migrate: Board | null;
}

/** A save this tab made that landed, held until an answer catches up with it. */
export interface Landed {
  seat: string;
  revision: number;
  board: Board;
}

/**
 * The board to draw. The browser's copy fills only the wait for the first answer;
 * after it the answer wins, unless this tab's own landed save is newer.
 */
export function drawnOf(
  answer: HomeBoardAnswer | null,
  local: Local,
  fallback: Board,
  landed: Landed | null = null,
): Drawn {
  if (answer === null) {
    const board = local.kind === "none" ? fallback : local.board;
    const revision = local.kind === "cache" ? local.revision : 0;
    return { board, revision, home: "unknown", migrate: null };
  }
  if (answer.seat === "") {
    return {
      board: local.kind === "browser" ? local.board : fallback,
      revision: 0,
      home: "browser",
      migrate: null,
    };
  }
  if (landed !== null && landed.seat === answer.seat && landed.revision > answer.revision) {
    return { board: landed.board, revision: landed.revision, home: "engine", migrate: null };
  }
  if (answer.board !== null) {
    return {
      board: validBoard(answer.board, fallback),
      revision: answer.revision,
      home: "engine",
      migrate: null,
    };
  }
  if (local.kind === "browser") {
    return { board: local.board, revision: 0, home: "engine", migrate: local.board };
  }
  return { board: fallback, revision: 0, home: "engine", migrate: null };
}

/** What a save came to: kept, refused for a newer save elsewhere, or not kept. */
export type SaveOutcome = "saved" | "conflict" | "failed";

/** The JSON a board is saved as fits the engine's bound. */
export function fits(board: Board): boolean {
  return new TextEncoder().encode(JSON.stringify(board)).length <= MAX_BOARD_BYTES;
}

/** The seat and revision a landed save answered, or null. */
function landedOf(receipt: unknown): { seat: string; revision: number } | null {
  const r = (receipt ?? {}) as { seat?: unknown; revision?: unknown };
  return typeof r.seat === "string" &&
    r.seat !== "" &&
    typeof r.revision === "number" &&
    Number.isSafeInteger(r.revision) &&
    r.revision > 0
    ? { seat: r.seat, revision: r.revision }
    : null;
}

/** Said where a board is past [MAX_BOARD_BYTES]. */
export const TOO_LARGE =
  "This Home is too large to keep. Remove some notes or links, then press Done again.";

/** The reader's board, `fallback` where nothing is kept; one per module, `use` per render. */
export function keptBoard(fallback: Board) {
  const browser = readerStore<Local>(STORAGE_KEYS.homeBoard, NONE, (raw) => localOf(raw, fallback));
  const keep = (next: Local) => browser.write(next);
  return { use: () => useKept(browser.use(), keep, fallback) };
}

function useKept(local: Local, keep: (next: Local) => void, fallback: Board) {
  const toast = useToast();
  // ANOTHER BROWSER MAY HAVE SAVED while this tab was hidden: coming back asks again.
  const query = useQuery("home_board", undefined, { refetchOnFocus: true });
  const answer = useMemo(() => answerOf(query.data), [query.data]);
  const write = useAct("save_home_board");
  const [landed, setLanded] = useState<Landed | null>(null);
  const asked = useRef(0);
  const drawn = useMemo(
    () => drawnOf(answer, local, fallback, landed),
    [answer, local, fallback, landed],
  );

  // WHAT IS DRAWN UNDER A SEAT IS KEPT HERE for the next cold load, and a
  // board this browser kept alone is dropped once the engine holds one.
  useEffect(() => {
    if (drawn.home !== "engine" || drawn.migrate !== null || answer === null) return;
    if (drawn.revision === 0) {
      if (local.kind !== "none") keep(NONE);
      return;
    }
    // NEVER OLDER THAN WHAT IS KEPT: another tab's newer save is read again, not overwritten.
    if (local.kind === "cache" && local.seat === answer.seat && local.revision >= drawn.revision) {
      if (local.revision > drawn.revision && asked.current !== local.revision) {
        asked.current = local.revision;
        query.refetch();
      }
      return;
    }
    keep({ kind: "cache", v: 1, seat: answer.seat, revision: drawn.revision, board: drawn.board });
  }, [drawn.home, drawn.migrate, drawn.revision, drawn.board, answer, local, keep, query]);

  const save = useCallback(
    async (
      next: Board,
      options: { quiet?: boolean; ifMatch?: number } = {},
    ): Promise<SaveOutcome> => {
      if (drawn.home === "browser") {
        keep({ kind: "browser", board: next });
        if (!options.quiet) toast.ok("Home saved");
        return "saved";
      }
      if (!fits(next)) {
        toast.failed(TOO_LARGE);
        return "failed";
      }
      const result = await write.run(
        { board: next, if_match: options.ifMatch ?? drawn.revision },
        { done: "Home saved", quiet: options.quiet },
      );
      if (result === null) {
        if (!write.access.can) toast.failed(write.access.reason);
        return "failed";
      }
      if (result.kind === "applied" || result.kind === "pending") {
        const at = landedOf(result.receipt);
        if (at !== null) setLanded({ ...at, board: next });
        return "saved";
      }
      if (result.kind === "refused") {
        write.dismiss();
        if (result.code === "stale_version") {
          query.refetch();
          return "conflict";
        }
        // HOME DRAWS NO REFUSAL BESIDE A CONTROL, so it is said where it is seen.
        toast.failed(result.sentence);
      }
      return "failed";
    },
    [drawn.home, drawn.revision, write, keep, query, toast],
  );

  // A BOARD KEPT HERE ALONE MOVES ONTO THE ENGINE, once per answer that has room for it.
  const moving = useRef<number | null>(null);
  useEffect(() => {
    if (!drawn.migrate || answer === null || moving.current === answer.revision) return;
    moving.current = answer.revision;
    void save(drawn.migrate, { quiet: true });
  }, [drawn.migrate, answer, save]);

  // A FOLD IS DRAWN AT ONCE and kept behind it; one a newer save beat is laid
  // again on the board that save left, once the re-read brings it.
  const [folding, setFolding] = useState<Board | null>(null);
  const retry = useRef<{ edit: (b: Board) => Board; after: number } | null>(null);
  const update = useCallback(
    async (edit: (b: Board) => Board) => {
      const next = edit(folding ?? drawn.board);
      setFolding(next);
      const outcome = await save(next, { quiet: true });
      if (outcome === "conflict") retry.current = { edit, after: drawn.revision };
      else setFolding(null);
    },
    [folding, drawn.board, drawn.revision, save],
  );
  useEffect(() => {
    const again = retry.current;
    if (!again || drawn.revision === again.after || drawn.home !== "engine") return;
    retry.current = null;
    void save(again.edit(drawn.board), { quiet: true }).then(() => setFolding(null));
  }, [drawn.revision, drawn.board, drawn.home, save]);

  return {
    board: folding ?? drawn.board,
    revision: drawn.revision,
    home: drawn.home,
    saving: write.busy,
    save,
    update,
  };
}
