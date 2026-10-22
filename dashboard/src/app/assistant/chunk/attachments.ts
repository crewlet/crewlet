/**
 * The images the person attaches to their next question: what may be
 * attached, and the drafts the composer holds while they upload.
 *
 * BOUNDED HERE AT THE ENGINE'S OWN LIMITS (`ASSISTANT_ATTACHMENTS_PER_TURN`
 * images, each at most `ASSISTANT_FILE_MAX_BYTES`, of the types
 * `AssistantAttachmentMediaType` names), so a file the engine would refuse is
 * refused before it is sent, in words that say what to do. The engine checks
 * all of it again, from the bytes rather than from the name.
 *
 * EACH IMAGE IS UPLOADED AT ONCE (`session.uploadAttachment`), so Send never
 * waits on an upload it could have started earlier, and the question carries
 * only the file ids the engine answered.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import {
  ASSISTANT_ATTACHMENTS_PER_TURN,
  ASSISTANT_FILE_MAX_BYTES,
  type AssistantAttachmentMediaType,
  type AssistantFileRef,
} from "~/contract/assistant.ts";
import { RestError, isAbort } from "~/protocol/rest.ts";
import { fmtBytes } from "~/lib/format.ts";
import { conditionWords, conversation, uploadAttachment } from "./session.ts";

/** Each type a person may attach, by its name in a sentence. */
const ATTACHABLE: Record<AssistantAttachmentMediaType, string> = {
  "image/png": "PNG",
  "image/jpeg": "JPEG",
  "image/webp": "WebP",
  "image/gif": "GIF",
};

/** What the file picker offers. */
export const ATTACH_ACCEPT = Object.keys(ATTACHABLE).join(",");

/** The types in words: "PNG, JPEG, WebP or GIF". */
const TYPE_LIST = (() => {
  const names = Object.values(ATTACHABLE);
  return `${names.slice(0, -1).join(", ")} or ${names.at(-1)!}`;
})();

/** What may be attached, as the drop target's hint says it. */
export const ATTACH_HINT = `${TYPE_LIST} images, up to ${fmtBytes(ASSISTANT_FILE_MAX_BYTES)} each`;

/** Whether a file is of a type a person may attach. */
function attachable(type: string): boolean {
  return Object.hasOwn(ATTACHABLE, type);
}

/** The files that may join the `held` already attached, and why each other one may not. */
export function admit(
  files: readonly File[],
  held: number,
): { admitted: File[]; refusals: string[] } {
  const admitted: File[] = [];
  const refusals: string[] = [];
  let full = false;
  for (const file of files) {
    if (!attachable(file.type)) {
      refusals.push(`${file.name} cannot be attached. Attach a ${TYPE_LIST} image.`);
    } else if (file.size > ASSISTANT_FILE_MAX_BYTES) {
      refusals.push(
        `${file.name} is larger than ${fmtBytes(ASSISTANT_FILE_MAX_BYTES)}. Attach a smaller image.`,
      );
    } else if (held + admitted.length >= ASSISTANT_ATTACHMENTS_PER_TURN) {
      full = true;
    } else {
      admitted.push(file);
    }
  }
  if (full) {
    refusals.push(
      `A question can carry up to ${ASSISTANT_ATTACHMENTS_PER_TURN} images. Remove one to attach another.`,
    );
  }
  return { admitted, refusals };
}

/** An image in the composer: on its way up, ready to send, or refused. */
export interface AttachmentDraft {
  key: string;
  name: string;
  /** The local picture of it, an object URL of the person's own file. */
  preview: string;
  state: "uploading" | "ready" | "failed";
  /** The engine's reference, once stored. */
  file: AssistantFileRef | null;
}

/** The images attached to the next question, and what the composer does with them. */
export interface Attachments {
  drafts: readonly AttachmentDraft[];
  /** Why the last files could not be attached, in sentences; null when nothing was refused. */
  refusal: string | null;
  /** Some image is still on its way up. */
  uploading: boolean;
  add: (files: readonly File[]) => void;
  remove: (key: string) => void;
  dismiss: () => void;
  /** The stored images, for the question being sent; the composer is emptied. */
  take: () => AssistantFileRef[];
}

/**
 * Why an upload did not land, in words: for a condition of the node the
 * engine's words for it (`conditionWords`), which say who clears it and ask
 * for a wait only where one helps, and the code's own sentence for anything
 * else.
 */
function uploadRefusal(name: string, err: unknown): string {
  const why =
    err instanceof RestError && !err.unanswered
      ? conditionWords(err) || err.sentence || "Check your connection and try again."
      : "Check your connection and try again.";
  return `${name} could not be attached. ${why}`;
}

/** What the hook keeps of one draft beside what it draws. */
interface LiveDraft {
  controller: AbortController;
  preview: string;
  conversation: string | null;
  failed: boolean;
}

let nextKey = 0;

/**
 * The composer's attachments. `conversationId` is the conversation the drawer
 * shows: images uploaded to another one (the person started a new one, or
 * opened another, meanwhile) are dropped, since a question may only carry its
 * own conversation's files.
 */
export function useAttachments(conversationId: string | null): Attachments {
  const [drafts, setDrafts] = useState<AttachmentDraft[]>([]);
  const [refusal, setRefusal] = useState<string | null>(null);
  /**
   * Each draft's upload, the conversation it went to and whether it was
   * refused, by key: read at once by a second drop or paste, before the
   * drafts the composer draws have caught up.
   */
  const live = useRef(new Map<string, LiveDraft>());

  const drop = useCallback((keys: readonly string[]) => {
    for (const key of keys) {
      const held = live.current.get(key);
      if (!held) continue;
      held.controller.abort();
      URL.revokeObjectURL(held.preview);
      live.current.delete(key);
    }
    setDrafts((now) => now.filter((d) => !keys.includes(d.key)));
  }, []);

  const add = useCallback(
    (files: readonly File[]) => {
      const held = [...live.current.values()].filter((entry) => !entry.failed).length;
      const { admitted, refusals } = admit(files, held);
      setRefusal(refusals.length > 0 ? refusals.join(" ") : null);
      for (const file of admitted) {
        const key = `attachment-${(nextKey += 1)}`;
        const preview = URL.createObjectURL(file);
        const controller = new AbortController();
        live.current.set(key, { controller, preview, conversation: null, failed: false });
        setDrafts((now) => [
          ...now,
          { key, name: file.name, preview, state: "uploading", file: null },
        ]);
        uploadAttachment(file, controller.signal).then(
          (stored) => {
            const entry = live.current.get(key);
            if (!entry) return;
            entry.conversation = stored.conversationId;
            if (conversation().id !== stored.conversationId) {
              drop([key]);
              return;
            }
            setDrafts((now) =>
              now.map((d) => (d.key === key ? { ...d, state: "ready", file: stored.file } : d)),
            );
          },
          (err: unknown) => {
            const entry = live.current.get(key);
            if (isAbort(err) || !entry) return;
            entry.failed = true;
            setDrafts((now) => now.map((d) => (d.key === key ? { ...d, state: "failed" } : d)));
            setRefusal(uploadRefusal(file.name, err));
          },
        );
      }
    },
    [drop],
  );

  // ANOTHER CONVERSATION'S IMAGES never ride along with this one's question.
  useEffect(() => {
    const stale = [...live.current.entries()]
      .filter(([, entry]) => entry.conversation !== null && entry.conversation !== conversationId)
      .map(([key]) => key);
    if (stale.length > 0) drop(stale);
  }, [conversationId, drop]);

  // THE LOCAL PICTURES AND UPLOADS END WITH THE COMPOSER.
  useEffect(() => {
    const held = live.current;
    return () => {
      for (const entry of held.values()) {
        entry.controller.abort();
        URL.revokeObjectURL(entry.preview);
      }
      held.clear();
    };
  }, []);

  const take = useCallback((): AssistantFileRef[] => {
    const files = drafts.flatMap((d) => (d.state === "ready" && d.file ? [d.file] : []));
    for (const entry of live.current.values()) URL.revokeObjectURL(entry.preview);
    live.current.clear();
    setDrafts([]);
    setRefusal(null);
    return files;
  }, [drafts]);

  return {
    drafts,
    refusal,
    uploading: drafts.some((d) => d.state === "uploading"),
    add,
    remove: (key) => drop([key]),
    dismiss: () => setRefusal(null),
    take,
  };
}
