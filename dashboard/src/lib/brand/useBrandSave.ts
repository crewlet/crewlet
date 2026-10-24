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
  saveBrand,
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
    save.files.map((f) => [
      f.path,
      f.body.size,
      f.body.type,
      f.body instanceof File ? [f.body.name, f.body.lastModified] : null,
    ]),
  ]);
}

export interface BrandSaveGesture {
  busy: boolean;
  answer: BrandSaveAnswer | null;
  run: (save: BrandSave) => Promise<BrandSaveAnswer | null>;
  reset: () => void;
}

export function useBrandSave(): BrandSaveGesture {
  const [busy, setBusy] = useState(false);
  const [answer, setAnswer] = useState<BrandSaveAnswer | null>(null);
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
        const next = await saveBrand(save, keys);
        setAnswer(next);
        return next;
      } finally {
        setBusy(false);
      }
    },
    [answer, busy],
  );
  const reset = useCallback(() => {
    last.current = null;
    setAnswer(null);
  }, []);
  return { busy, answer, run, reset };
}
