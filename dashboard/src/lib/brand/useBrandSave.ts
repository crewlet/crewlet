/**
 * One gesture's brand save: its keys, its answer, and whether it is out.
 *
 * THE KEYS ARE THE GESTURE'S ([saveBrand]): minted when the person presses
 * Save, and kept for the retry an unknown answer asks for, so pressing Save
 * again on the same edit is the same writes, which the engine applies once.
 * Any other answer, or a different edit, mints new keys.
 */

import { useCallback, useRef, useState } from "react";
import {
  newBrandSaveKeys,
  publishBrandDraft,
  saveBrand,
  type BrandDraftAnswer,
  type BrandSave,
  type BrandSaveAnswer,
  type BrandSaveKeys,
} from "./api.ts";
import { serializeBrandDocument } from "./brand.ts";

/** What makes two saves the same request: the target, the document and each file by what it is. */
function requestOf(save: BrandSave): string {
  return JSON.stringify([
    save.kit,
    save.head,
    save.title,
    serializeBrandDocument(save.document),
    save.files.map((f) =>
      "from" in f
        ? [f.path, "from", f.from]
        : [
            f.path,
            f.body.size,
            f.body.type,
            f.body instanceof File ? [f.body.name, f.body.lastModified] : null,
          ],
    ),
  ]);
}

/** One save gesture: its answer, whether it is out, and the way to send it. */
export interface SaveGesture<A> {
  busy: boolean;
  answer: A | null;
  run: (save: BrandSave) => Promise<A | null>;
  reset: () => void;
}

/** A save gesture over `write`, keyed once per request and again only for the retry an unknown answer asks for. */
function useSaveGesture<A extends { kind: string }>(
  write: (save: BrandSave, keys: BrandSaveKeys) => Promise<A>,
): SaveGesture<A> {
  const [busy, setBusy] = useState(false);
  const [answer, setAnswer] = useState<A | null>(null);
  const last = useRef<{ request: string; keys: BrandSaveKeys } | null>(null);
  const run = useCallback(
    async (save: BrandSave) => {
      if (busy) return null;
      const request = requestOf(save);
      const keys =
        answer?.kind === "unknown" && last.current?.request === request
          ? last.current.keys
          : newBrandSaveKeys();
      last.current = { request, keys };
      setBusy(true);
      try {
        const next = await write(save, keys);
        setAnswer(next);
        return next;
      } finally {
        setBusy(false);
      }
    },
    [answer, busy, write],
  );
  const reset = useCallback(() => {
    last.current = null;
    setAnswer(null);
  }, []);
  return { busy, answer, run, reset };
}

/** Publish a version of the company's kit and adopt it. */
export type BrandSaveGesture = SaveGesture<BrandSaveAnswer>;

export function useBrandSave(): BrandSaveGesture {
  return useSaveGesture(saveBrand);
}

/** Publish a version of the company's kit as a draft, adopting nothing: a setup step's Continue. */
export function useBrandDraftSave(): SaveGesture<BrandDraftAnswer> {
  return useSaveGesture(publishBrandDraft);
}
