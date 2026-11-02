/**
 * What the artifact screens' suites share to read the draft a screen
 * declares and to apply a `draft.set` through it, as the drawer would.
 *
 * Not a suite itself, and never imported by anything that ships.
 */

import { act } from "@testing-library/react";
import type { AssistantDraftField, AssistantPageContext } from "~/contract/assistant.ts";
import {
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "~/app/assistant/context.ts";

/** Put the frame on a screen, so the registry builds a page context for it. */
export function openFrame(screen: string): void {
  resetPageContextForTest();
  setFramePage({ screen, title: "Artifacts › New", path: ["artifacts"], params: {}, query: {} });
}

/** The page context as the drawer would send it now. */
export function context(): AssistantPageContext {
  const snapshot = pageSnapshot();
  if (!snapshot) throw new Error("no frame page: call openFrame first");
  return snapshot.context;
}

/** One field of the open draft, by its path. */
export function draftField(path: string): AssistantDraftField | undefined {
  return context().draft?.fields.find((f) => f.path === path);
}

let applied = 0;

/** Apply a `draft.set` to the page as it is now, and answer its outcome. */
export function setDraft(values: Record<string, unknown>): ReturnType<typeof applyPageAction> {
  let result: ReturnType<typeof applyPageAction> | undefined;
  act(() => {
    result = applyPageAction({
      correlation_id: `draft-${(applied += 1)}`,
      action: "draft.set",
      payload: { values },
      source_revision: context().source_revision,
    });
  });
  return result!;
}
