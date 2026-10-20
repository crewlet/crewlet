/**
 * What every kind's create page shares: the task it was opened for, the bound
 * on a title, and the engine writes that make a new artifact. Each kind's page
 * (`DocCreate`, `SlidesCreate`, ...) owns its whole body and draws these in its
 * own way; the writes and their words are said once, here.
 *
 * A DOC OR A DECK IS ONE WRITE: the engine publishes its own starter, held to
 * the kind's format, and the person edits it in place from there. A SHEET IS
 * TWO: the artifact, then its schema. Each is keyed, so a retry is the same
 * operation; a schema the engine refused after the sheet was made leaves the
 * sheet in place, and its page says so and offers the schema editor. Rows
 * the page typed or imported follow as keyed batches of at most a hundred,
 * and a batch the engine refuses stops the rest: the sheet opens with the
 * rows written, and says how many and why the rest were not. A
 * design, a dashboard or an HTML page is an agent's to publish, so its page
 * asks an agent in a new task ([useAskAgent]).
 */

import { useCallback, useState } from "react";
import { useToast } from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import { useOpenNewTask, type OpenNewTask } from "~/app/newTask.ts";
import { ITEM_KEY_SHAPE, artifactPath } from "~/app/routes.ts";
import { ARTIFACT_TITLE_MAX_BYTES } from "~/contract/artifacts.ts";
import { utf8Bytes } from "~/lib/format.ts";
import { keyedWrite, useKeyedGesture, type KeyedAnswer } from "~/lib/keyedWrite.ts";
import { configureSchema, createFromStarter, createSheet, writeData } from "~/lib/artifacts/api.ts";
import { resultOf, rowID } from "~/lib/artifacts/datastore.ts";
import { insertBatches, type DraftWrites } from "~/lib/artifacts/sheetDraft.ts";
import { plural } from "~/lib/format.ts";
import { newActOpID } from "~/protocol/index.ts";

/**
 * The task a new artifact is made for (`item=`), which lists it on that
 * task's Artifacts card; "" where the page was not opened from a task, or the
 * address names something that is not a task's key.
 */
export function useWorkItem(): string {
  const [item] = useParam("item", "");
  return ITEM_KEY_SHAPE.test(item) ? item : "";
}

/**
 * What is wrong with a title, in the sentence the field says under it, or
 * undefined when the engine will take it. An empty title is not wrong while
 * it is being typed: the create action says "Give it a title." instead.
 */
export function titleProblem(title: string): string | undefined {
  const bytes = utf8Bytes(title.trim());
  return bytes > ARTIFACT_TITLE_MAX_BYTES
    ? `The title is ${bytes} bytes and the most is ${ARTIFACT_TITLE_MAX_BYTES}.`
    : undefined;
}

/** Why the create action is held for a title, or undefined when it may go. */
export function titleHold(title: string): string | undefined {
  return title.trim() === "" ? "Give it a title." : titleProblem(title);
}

/** A create page's writes, and what the last one answered. */
export interface CreateArtifact {
  /** Whether a write is out. */
  busy: boolean;
  /** What the engine said when it refused the last write, or null. */
  refusal: string | null;
  /** Make a doc or a deck from the engine's starter and open it. */
  fromStarter: (kind: "doc" | "slides", title: string) => Promise<void>;
  /** Make a sheet with these columns and rows, and open it. */
  sheet: (title: string, writes: DraftWrites) => Promise<void>;
  /** The rows written of those being written, while a sheet's rows go in; null otherwise. */
  progress: { done: number; total: number } | null;
}

/** How many times a row batch whose answer was lost, or that was rate limited, is sent again under its key. */
const BATCH_RETRIES = 2;

/**
 * Make a new artifact and open it, for the task the page was opened for
 * ([useWorkItem]). The page that calls it draws `busy` on its action and
 * `refusal` where the reader is looking.
 */
export function useCreateArtifact(): CreateArtifact {
  const nav = useNavigator();
  const toast = useToast();
  const gesture = useKeyedGesture();
  const workItem = useWorkItem();
  const { run } = gesture;

  /** The new artifact's id from a done answer, or "" for anything else. */
  const made = (answer: KeyedAnswer | null): string =>
    answer?.kind === "done" && typeof answer.body.artifact === "string" ? answer.body.artifact : "";

  const fromStarter = useCallback(
    async (kind: "doc" | "slides", title: string) => {
      const name = title.trim();
      const id = made(
        await run(createFromStarter({ kind, title: name, ...(workItem ? { workItem } : {}) })),
      );
      if (!id) return;
      toast.ok(`Started “${name}”`);
      nav.to(artifactPath(id));
    },
    [run, workItem, toast, nav],
  );

  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);

  const sheet = useCallback(
    async (title: string, writes: DraftWrites) => {
      const name = title.trim();
      const id = made(await run(createSheet({ title: name, ...(workItem ? { workItem } : {}) })));
      if (!id) return;
      const schema = await keyedWrite({
        ...configureSchema(id, writes.collections),
        key: newActOpID(),
      });
      if (schema.kind !== "done") {
        toast.show({
          variant: "warning",
          title: "The sheet was made, and its columns were not set",
          message: schema.text,
        });
        nav.to(artifactPath(id), { tab: "data", data: "schema" });
        return;
      }
      const total = writes.rows.length;
      const client = `create-${rowID()}`;
      let done = 0;
      let stopped: string | null = null;
      if (total > 0) setProgress({ done, total });
      for (const batch of insertBatches(writes.collection, writes.rows, rowID)) {
        const key = newActOpID();
        let result = resultOf(await keyedWrite({ ...writeData(id, batch, client, { via: "dashboard" }), key }));
        for (let retry = 0; retry < BATCH_RETRIES; retry++) {
          if (result.verdict !== "unknown" && result.verdict !== "rate_limited") break;
          const wait = result.verdict === "rate_limited" ? (result.retryAfterMs ?? 1000) : 500;
          await new Promise((settle) => setTimeout(settle, wait));
          result = resultOf(await keyedWrite({ ...writeData(id, batch, client, { via: "dashboard" }), key }));
        }
        if (result.verdict !== "applied" && result.verdict !== "pending") {
          stopped = result.reason;
          break;
        }
        done += batch.length;
        setProgress({ done, total });
      }
      setProgress(null);
      if (stopped === null) toast.ok(`Started “${name}”`);
      else {
        toast.show({
          variant: "warning",
          title:
            done === 0
              ? "The sheet was made, and its rows were not written"
              : `The sheet was made with ${done} of ${plural(total, "row")}`,
          message: stopped,
        });
      }
      nav.to(artifactPath(id));
    },
    [run, workItem, toast, nav],
  );

  return {
    busy: gesture.busy,
    refusal: gesture.answer && gesture.answer.kind !== "done" ? gesture.answer.text : null,
    fromStarter,
    sheet,
    progress,
  };
}

/**
 * Ask an agent for an artifact a person does not type here (a design, a
 * dashboard, an HTML page): the New task sheet, where the reader says what
 * they need and whom to ask.
 */
export function useAskAgent(): OpenNewTask {
  return useOpenNewTask();
}
