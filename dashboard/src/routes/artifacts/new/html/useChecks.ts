/**
 * The publish checks, asked while the person writes: the engine's own linter
 * over the page's text files (`POST /artifacts/checks`), with nothing written.
 *
 * ASKED AFTER A PAUSE, NEVER PER KEYSTROKE: a check is sent once the text has
 * rested for [CHECK_PAUSE_MS], and an edit while one is out aborts it, so the
 * answer drawn is always about the text on screen. A budget the engine says is
 * spent is waited out (its `Retry-After`), not retried at once.
 *
 * WHAT IS DRAWN IS THE LAST ANSWER until the next arrives, so a list of
 * findings does not blink away and back on every edit.
 */

import { useEffect, useRef, useState } from "react";
import { isAbort, RestError, type ArtifactCheckAnswer } from "~/protocol/index.ts";
import { ARTIFACT_CHECK_MAX_BYTES, ARTIFACT_CHECK_MAX_FILES } from "~/contract/artifacts.ts";
import { checkPage } from "~/lib/artifacts/api.ts";
import { refusalText } from "~/lib/refusal.ts";

/** How long the text rests before it is checked. */
export const CHECK_PAUSE_MS = 600;

export type CheckState =
  /** Nothing to check: no text file yet. */
  | { kind: "idle" }
  /** A check is out; `last` is the answer drawn meanwhile. */
  | { kind: "checking"; last: ArtifactCheckAnswer | null }
  | { kind: "checked"; answer: ArtifactCheckAnswer }
  /**
   * No answer to give while writing: the page is past what a check reads, or
   * the engine could not check it. Said in `text`; the preview still checks.
   */
  | { kind: "unavailable"; text: string; last: ArtifactCheckAnswer | null };

/**
 * An engine older than the check route: the path is answered as an
 * artifact's own (`/artifacts/{id}`), which takes no POST, or not at all.
 */
function notServed(err: unknown): boolean {
  return err instanceof RestError && (err.status === 404 || err.status === 405);
}

/** Why a check could not be answered, in the reader's words. */
function unavailableText(err: unknown): string {
  if (notServed(err)) {
    return "This engine does not check pages as you write. Previewing checks it.";
  }
  if (err instanceof RestError && err.status === 0) {
    return "The engine could not be reached to check the page. It is checked again when you edit or preview.";
  }
  return refusalText(err);
}

/**
 * Check `files` (the page's text files, by path) with `entry` as the page,
 * whenever they change and have rested; `enabled` false asks nothing.
 */
export function useChecks(
  files: readonly { path: string; text: string }[],
  entry: string,
  enabled: boolean,
): CheckState {
  const [state, setState] = useState<CheckState>({ kind: "idle" });
  const last = useRef<ArtifactCheckAnswer | null>(null);
  // The engine said it does not check (an older engine): asked no more.
  const unsupported = useRef(false);
  const key = JSON.stringify([entry, files]);

  useEffect(() => {
    if (!enabled || files.length === 0) {
      setState({ kind: "idle" });
      return;
    }
    if (unsupported.current) return;
    const bytes = files.reduce((n, f) => n + new TextEncoder().encode(f.text).length, 0);
    if (files.length > ARTIFACT_CHECK_MAX_FILES || bytes > ARTIFACT_CHECK_MAX_BYTES) {
      setState({
        kind: "unavailable",
        text: "The page is too large to check as you write. Previewing checks it.",
        last: null,
      });
      return;
    }
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const ask = async () => {
      setState({ kind: "checking", last: last.current });
      try {
        const answer = await checkPage(files, entry, controller.signal);
        last.current = answer;
        setState({ kind: "checked", answer });
      } catch (err) {
        if (isAbort(err)) return;
        if (err instanceof RestError && err.status === 429) {
          // THE BUDGET IS WAITED OUT, and the same text checked then.
          timer = setTimeout(() => void ask(), Math.max(1, err.retryAfterSeconds ?? 1) * 1000);
          return;
        }
        if (notServed(err)) unsupported.current = true;
        setState({ kind: "unavailable", text: unavailableText(err), last: last.current });
      }
    };
    timer = setTimeout(() => void ask(), CHECK_PAUSE_MS);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
    // The key is what is checked; `files` is a new array on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, enabled]);

  return state;
}
