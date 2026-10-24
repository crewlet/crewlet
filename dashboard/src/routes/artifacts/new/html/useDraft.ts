/**
 * A new HTML page's private draft: the artifact its first preview makes, and
 * every version a later preview publishes onto it.
 *
 * A PREVIEW IS A REAL PUBLISH. The dashboard never runs a page's script on its
 * own origin (it enforces Trusted Types with no policy, and ADR-0035 keeps
 * author code on the credential-less artifact origin), and the artifact origin
 * serves only versions the linter wrote. So the first preview creates the
 * artifact, owned by the person and shared with nobody, and each later one
 * publishes the next version on the head it read. The engine has no draft
 * state that hides an artifact from its owner's gallery: a draft is an
 * ordinary private artifact, and Discard moves it to the trash.
 *
 * EVERY STEP IS THE PIPELINE'S: the files go up through an upload
 * (`uploadFiles`), and a refused page answers the linter's findings, every one,
 * with nothing left behind.
 */

import { useCallback, useState } from "react";
import { newActOpID, RestError, type ArtifactLintFinding } from "~/protocol/index.ts";
import { keyedWrite } from "~/lib/keyedWrite.ts";
import { refusalText } from "~/lib/refusal.ts";
import {
  createPage,
  patchArtifact,
  publishVersion,
  trashArtifact,
  uploadFiles,
  type UploadFile,
} from "~/lib/artifacts/api.ts";
import type { ArtifactCapability } from "~/protocol/index.ts";
import type { PageFile } from "./source.ts";

/** The draft the previews publish onto. */
export interface Draft {
  id: string;
  /** The head the last preview landed, which the next one is based on. */
  head: number;
  title: string;
  /** What the last preview published ([fingerprint]): a run is offered only when it differs. */
  published: string;
  /** What the last landed preview warned. */
  warnings: ArtifactLintFinding[];
}

export type RunState =
  | { kind: "idle" }
  | { kind: "running" }
  /**
   * The linter refused the page: every finding, and nothing was saved.
   * `fingerprint` is what was refused, so an edit since makes it history.
   */
  | { kind: "refused"; findings: ArtifactLintFinding[]; fingerprint: string }
  /** Anything else that kept the preview from landing, in the reader's words. */
  | { kind: "failed"; text: string };

/**
 * What one run came to: the draft it landed, or why it did not. A caller
 * steers by it (a refusal shows the checks, any other failure stays where its
 * callout is), which the state alone could not say before the next render.
 */
export type RunOutcome =
  { kind: "landed"; draft: Draft } | Extract<RunState, { kind: "refused" } | { kind: "failed" }>;

export interface PageRun {
  files: readonly PageFile[];
  entry: string;
  title: string;
  capabilities: readonly ArtifactCapability[];
  /** [fingerprint] of what is run, kept on the draft once it lands. */
  fingerprint: string;
  /** The task the page is made for. */
  workItem?: string;
}

/** Files as an upload brings them: text as text, a picture as its bytes. */
function uploads(files: readonly PageFile[]): UploadFile[] {
  return files.map((f) => ({ path: f.path, body: f.kind === "text" ? f.text : f.blob }));
}

/** The linter's findings a refusal carries, or null for any other refusal. */
function findingsOf(err: RestError | undefined): ArtifactLintFinding[] | null {
  if (err?.code !== "policy_refused" || !Array.isArray(err.body.findings)) return null;
  return err.body.findings.filter(
    (f): f is ArtifactLintFinding =>
      typeof f === "object" && f !== null && typeof (f as ArtifactLintFinding).message === "string",
  );
}

/** Findings as a landed answer lists them. */
function warningsOf(body: Record<string, unknown>): ArtifactLintFinding[] {
  return Array.isArray(body.warnings) ? (body.warnings as ArtifactLintFinding[]) : [];
}

export function useDraft() {
  const [draft, setDraft] = useState<Draft | null>(null);
  const [state, setState] = useState<RunState>({ kind: "idle" });
  const [discarding, setDiscarding] = useState(false);

  const run = useCallback(
    async (page: PageRun): Promise<RunOutcome> => {
      setState({ kind: "running" });
      try {
        const up = await uploadFiles(draft?.id ?? null, uploads(page.files), newActOpID());
        const answer = await keyedWrite({
          ...(draft
            ? publishVersion(draft.id, draft.head, {
                mode: "whole",
                files: up.files,
                upload: up.upload,
                entry: page.entry,
                capabilities: page.capabilities,
              })
            : createPage({
                title: page.title,
                upload: up.upload,
                files: up.files,
                entry: page.entry,
                capabilities: page.capabilities,
                ...(page.workItem ? { workItem: page.workItem } : {}),
              })),
          key: newActOpID(),
        });
        if (answer.kind !== "done") {
          const error = answer.kind === "refused" ? answer.error : undefined;
          const findings = findingsOf(error);
          const outcome: RunOutcome = findings
            ? { kind: "refused", findings, fingerprint: page.fingerprint }
            : error?.code === "version_moved"
              ? {
                  kind: "failed",
                  text: "This draft changed in another window, so this preview was not saved. Open the draft to continue there.",
                }
              : { kind: "failed", text: answer.text };
          setState(outcome);
          return outcome;
        }
        const id =
          draft?.id ?? (typeof answer.body.artifact === "string" ? answer.body.artifact : "");
        const head =
          typeof answer.body.number === "number" ? answer.body.number : (draft?.head ?? 1);
        let title = draft?.title ?? page.title;
        if (draft && page.title !== draft.title) {
          // A RENAME IS ITS OWN WRITE, and a refused one keeps the old name
          // rather than failing the preview that landed.
          const renamed = await keyedWrite({
            ...patchArtifact(id, { title: page.title }),
            key: newActOpID(),
          });
          if (renamed.kind === "done") title = page.title;
        }
        const next: Draft = {
          id,
          head,
          title,
          published: page.fingerprint,
          warnings: warningsOf(answer.body),
        };
        setDraft(next);
        setState({ kind: "idle" });
        return { kind: "landed", draft: next };
      } catch (err) {
        const outcome: RunOutcome = { kind: "failed", text: refusalText(err) };
        setState(outcome);
        return outcome;
      }
    },
    [draft],
  );

  /** Move the draft to the trash; answers whether it went. */
  const discard = useCallback(async (): Promise<string | null> => {
    if (!draft) return null;
    setDiscarding(true);
    const answer = await keyedWrite({ ...trashArtifact(draft.id), key: newActOpID() });
    setDiscarding(false);
    if (answer.kind === "done") {
      setDraft(null);
      return null;
    }
    return answer.text;
  }, [draft]);

  return { draft, state, run, discard, discarding };
}
