/** An artifact's comments on its page: threads, filter, selection, Comment mode and the draft. */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useScreenBelow, useToast } from "@crewlethq/ui";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { buildHash, parseHash, useNavigator, useParam } from "~/app/router.tsx";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import { useKeyedGesture, type KeyedRequest } from "~/lib/keyedWrite.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { ROLE_WORDS } from "~/lib/artifacts/words.ts";
import {
  PLACEABLE_KINDS,
  filterOf,
  filterParam,
  type CommentFilter,
} from "~/lib/artifacts/comments.ts";
import type {
  ArtifactCommentAnchor,
  ArtifactCommentThread,
  ArtifactCommentsAnswer,
  ArtifactDetail,
} from "~/protocol/index.ts";
import { withPanel } from "../panels.ts";
import type { PageHost } from "../host.ts";
import "~/styles/comments.css";

/** A comment being written: where it points, and what the drawing showed there. */
export interface CommentDraft {
  /** Which draft this is: a picture answers the draft it was asked for, never a later one. */
  seq: number;
  anchor: ArtifactCommentAnchor;
  /** Where it points, in the engine's words. */
  label: string;
  /** The part's words as the reader saw them. */
  quote?: string | undefined;
  /** A picture of the part as a data URL: undefined while it is drawn, null where none was. */
  crop?: string | null | undefined;
  /** Written in the Comments panel rather than beside its pin, which left the view. */
  docked?: boolean | undefined;
}

/** The comments of the artifact on show, and what the reader is doing with them. */
export interface ArtifactComments {
  detail: ArtifactDetail;
  /** The version on show, which a new thread is made on. */
  version: number;
  answer: ArtifactCommentsAnswer | null;
  threads: ArtifactCommentThread[];
  /** The read, where it failed with nothing to show: what `QueryState` says. */
  failed: QueryResult<ArtifactCommentsAnswer> | null;
  /** A thread's number, as its pin and its card show it. */
  numberOf: (id: string) => number;
  refetch: () => void;
  filter: CommentFilter;
  setFilter: (f: CommentFilter) => void;
  selected: string | null;
  /** Select a thread in the list (or none), in the address. */
  select: (id: string | null) => void;
  /** Open the panel on a thread, from its pin. */
  openThread: (id: string) => void;
  /** Open the panel, keeping the thread selected: a draft on a phone is written there. */
  showPanel: () => void;
  /** A panel drawn over the artifact makes way for it, keeping the thread selected. */
  stepAside: () => void;
  placing: boolean;
  setPlacing: (on: boolean) => void;
  draft: CommentDraft | null;
  /** Start a draft; its seq names it to the picture drawn for it. */
  startDraft: (d: Omit<CommentDraft, "seq">) => number;
  /** The picture of draft `seq` arrived (or did not). */
  setDraftCrop: (seq: number, crop: string | null) => void;
  /** The draft's pin left the view: it is written on in the panel. */
  dockDraft: () => void;
  clearDraft: () => void;
  /** The draft's words, kept across pin and panel outside React state so typing redraws only the
   *  composer. */
  draftWords: { current: string };
  /** The reader may comment, and why not where they may not. */
  can: boolean;
  reason: string | undefined;
  /** The kind takes comments on its parts, not only on its whole version. */
  placeable: boolean;
  /** On a phone: the panel covers the artifact, and a draft is written in it. */
  narrow: boolean;
  /** Whoever owns the artifact, in words: told of every comment. */
  ownerName: string;
  /** Bring a thread's part into view, as the layer on show does it. */
  reveal: (t: ArtifactCommentThread) => void;
  /** The layer on show says how it brings a thread into view; null when it goes. */
  setRevealer: (fn: ((t: ArtifactCommentThread) => void) | null) => void;
  /** A framed page lends its host, by the document it shows; the returned call takes it back. */
  lendPage: (document: string, host: PageHost) => () => void;
  /** The host of the frame showing a document, while it is drawn. */
  pageHost: (document: string) => PageHost | null;
}

const Ctx = createContext<ArtifactComments | null>(null);

/** The artifact's comments, inside its page; null anywhere else (a compare, a preview). */
export function useComments(): ArtifactComments | null {
  return useContext(Ctx);
}

/** Hands the page's comments to everything drawn inside it. */
export function CommentsScope({
  value,
  children,
}: {
  value: ArtifactComments;
  children: ReactNode;
}) {
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

/** Why a reader below commenter cannot comment, in words. */
export function commentReason(held: ArtifactDetail["role"], fallback: string | undefined) {
  return held === "viewer"
    ? `Commenting needs the “${ROLE_WORDS.commenter}” role, and you can only view this artifact. Its owner can share it with you at that role.`
    : fallback;
}

export function useArtifactComments({
  detail,
  version,
  moved,
  ownerName,
  covering,
}: {
  detail: ArtifactDetail;
  version: number;
  /** Where the page's room says a comment moved: each listener asks again. */
  moved: Set<() => void>;
  ownerName: string;
  /** Whether the open panel is drawn over the artifact rather than beside it. */
  covering: () => boolean;
}): ArtifactComments {
  const nav = useNavigator();
  const read = useQuery(
    "artifact_comments",
    version > 0 ? { id: detail.id, version } : { id: detail.id },
  );
  const { refetch } = read;
  // THE ROOM'S HINT, never content: the threads are asked for again.
  useEffect(() => {
    moved.add(refetch);
    return () => void moved.delete(refetch);
  }, [moved, refetch]);

  const [filterRaw, setFilterRaw] = useParam("filter", "");
  const [thread] = useParam("thread", "");
  const [placing, setPlacingState] = useState(false);
  const [draft, setDraft] = useState<CommentDraft | null>(null);
  const seq = useRef(0);
  const draftWords = useRef("");
  const revealer = useRef<((t: ArtifactCommentThread) => void) | null>(null);
  const pages = useRef(new Map<string, PageHost>());
  const covers = useRef(covering);
  covers.current = covering;
  const narrow = useScreenBelow(PHONE_BREAKPOINT);
  const access = useArtifactAccess({ role: "commenter", held: detail.role });
  const answer = read.data;
  const can = access.can && (answer?.can_comment ?? true);
  const reason = can
    ? undefined
    : commentReason(
        detail.role,
        access.can ? "This artifact takes no comments now." : access.reason,
      );
  const placeable = PLACEABLE_KINDS.has(detail.kind);

  // A DRAFT IS ABOUT THE VERSION IT WAS PLACED ON: another version on show
  // takes it away, and Comment mode with it.
  useEffect(() => {
    setDraft(null);
    setPlacingState(false);
  }, [version]);

  /** The address as it stands now, so two moves in one gesture compose. */
  const here = () => parseHash(location.hash);

  // THE LAYERS' OWN, held in refs: the same calls for the page's life, so a
  // layer that registers through them does not register again on every change.
  const reveal = useCallback((t: ArtifactCommentThread) => revealer.current?.(t), []);
  const setRevealer = useCallback((fn: ((t: ArtifactCommentThread) => void) | null) => {
    revealer.current = fn;
  }, []);
  const lendPage = useCallback((document: string, host: PageHost) => {
    pages.current.set(document, host);
    return () => {
      if (pages.current.get(document) === host) pages.current.delete(document);
    };
  }, []);
  const pageHost = useCallback((document: string) => pages.current.get(document) ?? null, []);

  return useMemo<ArtifactComments>(() => {
    const threads = answer?.threads ?? [];
    const numbers = new Map(threads.map((t, i) => [t.id, i + 1]));
    const go = (query: URLSearchParams) => {
      const now = here();
      if (buildHash(now.path, query) !== now.hash) nav.to(now.path, query);
    };
    // THE PANEL, KEEPING THE THREAD SELECTED, whichever panel was open.
    const showPanel = () => {
      const now = here().query;
      const q = withPanel(now, "comments");
      for (const key of ["thread", "filter"]) {
        const v = now.get(key);
        if (v) q.set(key, v);
      }
      go(q);
    };
    return {
      detail,
      version,
      answer,
      threads,
      failed: answer === null && read.error ? read : null,
      numberOf: (id) => numbers.get(id) ?? 0,
      refetch,
      filter: filterParam(filterRaw),
      setFilter: (f) => setFilterRaw(f === "open" ? "" : f),
      selected: thread || null,
      select: (id) => nav.filter({ thread: id }),
      openThread: (id) => {
        const q = withPanel(here().query, "comments");
        q.set("thread", id);
        const t = threads.find((x) => x.id === id);
        const f = t ? filterOf(t) : "open";
        if (f === "open") q.delete("filter");
        else q.set("filter", f);
        go(q);
      },
      showPanel,
      stepAside: () => {
        if (!covers.current()) return;
        const now = here().query;
        const q = withPanel(now, null);
        const v = now.get("thread");
        if (v) q.set("thread", v);
        go(q);
      },
      placing,
      setPlacing: (on) => {
        setPlacingState(on);
        if (!on) {
          setDraft(null);
          draftWords.current = "";
        }
      },
      draft,
      startDraft: (d) => {
        seq.current += 1;
        setDraft({ ...d, seq: seq.current });
        return seq.current;
      },
      setDraftCrop: (n, crop) => setDraft((was) => (was && was.seq === n ? { ...was, crop } : was)),
      dockDraft: () => {
        setDraft((was) => (was && !was.docked ? { ...was, docked: true } : was));
        showPanel();
      },
      clearDraft: () => {
        setDraft(null);
        draftWords.current = "";
      },
      draftWords,
      can,
      reason,
      placeable,
      narrow,
      ownerName,
      reveal,
      setRevealer,
      lendPage,
      pageHost,
    };
    // The refs and `read` are read when called, never captured stale.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    detail,
    version,
    answer,
    read.error,
    refetch,
    filterRaw,
    setFilterRaw,
    thread,
    nav,
    placing,
    draft,
    can,
    reason,
    placeable,
    narrow,
    ownerName,
    reveal,
    setRevealer,
    lendPage,
    pageHost,
  ]);
}

/** One gesture's comment write: its key kept for a retry, the threads re-read once it lands, and
 *  a refusal toasted in the engine's words with the typed words kept. */
export function useCommentWrite(): {
  run: (req: Omit<KeyedRequest, "key">, done: string) => Promise<boolean>;
  busy: boolean;
} {
  const comments = useComments();
  const gesture = useKeyedGesture();
  const toast = useToast();
  const refetch = comments?.refetch;
  const { run: send, busy } = gesture;
  const run = useCallback(
    async (req: Omit<KeyedRequest, "key">, done: string) => {
      const answer = await send(req);
      if (answer?.kind === "done") {
        toast.ok(done);
        refetch?.();
        return true;
      }
      if (answer) {
        toast.show({
          variant: answer.kind === "unknown" ? "warning" : "danger",
          title: answer.kind === "unknown" ? "Not confirmed yet" : "Not saved",
          message: answer.text,
          duration: 0,
        });
      }
      return false;
    },
    [send, toast, refetch],
  );
  return { run, busy };
}
