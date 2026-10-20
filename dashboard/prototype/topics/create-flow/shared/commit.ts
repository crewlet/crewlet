/**
 * Turning a draft into an artifact, through the engine's own writes and in
 * the order the shipped screens make them, so every proposal lands a person
 * on a real artifact page holding what they typed.
 *
 * A DRAFT IS NOTHING UNTIL ITS CREATE: a person who opens a blank doc and
 * leaves has made nothing, so nothing is written before the create. Then a
 * doc is the engine's starter followed by one publish of what was typed (the
 * doc editor's own save path), a sheet is the artifact, its columns and its
 * first rows, and a deck is the starter with the slides' text edited in.
 */

import { useCallback, useState } from "react";
import { newActOpID } from "~/protocol/index.ts";
import type { ArtifactKind, DataCollection } from "~/protocol/index.ts";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "~/lib/keyedWrite.ts";
import {
  configureSchema,
  createFromStarter,
  createSheet,
  publishVersion,
  readDeck,
  readDoc,
  uploadTexts,
  versionText,
  writeData,
} from "~/lib/artifacts/api.ts";
import { DOC_INDEX, docDraftOf, docPublishPlan, setTabText } from "~/lib/artifacts/docDraft.ts";
import { draftOf, publishOf, sourcesNeeded } from "~/lib/artifacts/deck.ts";
import { withText } from "~/lib/slides.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useToast } from "@crewlethq/ui";
import { useNavigator } from "~/app/router.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { KIND, type BuiltKind } from "./kinds.ts";
import type { SlideTextEdit } from "./starterDeck.ts";

/** What a create came to: the artifact made, or why not. */
export type Committed = { ok: true; id: string; note?: string } | { ok: false; text: string };

async function write(req: Omit<KeyedRequest, "key">): Promise<KeyedAnswer> {
  return keyedWrite({ ...req, key: newActOpID() });
}

function made(answer: KeyedAnswer): string | { text: string } {
  if (answer.kind !== "done") return { text: answer.text };
  const id = typeof answer.body.artifact === "string" ? answer.body.artifact : "";
  return id || { text: "The engine made it and named no artifact." };
}

/** A doc: the starter under its title, then what was typed as its first tab. */
export async function commitDoc(title: string, markdown: string): Promise<Committed> {
  const id = made(await write(createFromStarter({ kind: "doc", title })));
  if (typeof id !== "string") return { ok: false, text: id.text };
  if (markdown.trim() === "") return { ok: true, id };
  try {
    const signal = new AbortController().signal;
    const { doc } = await readDoc(id, 1, signal);
    const base = docDraftOf(doc, 1);
    const first = base.order[0];
    if (!first) return { ok: true, id };
    const draft = setTabText(base, first, markdown);
    const indexRaw = base.indexed ? await versionText(id, 1, DOC_INDEX) : null;
    const plan = docPublishPlan(base, draft, indexRaw);
    const brought =
      plan.uploads.length > 0 ? await uploadTexts(id, plan.uploads, newActOpID()) : null;
    const saved = await write(
      publishVersion(id, 1, {
        mode: "patch",
        edits: plan.edits,
        files: [
          ...(brought?.files ?? []),
          ...plan.removes.map((p) => ({ path: p, remove: true as const })),
        ],
        ...(brought ? { upload: brought.upload } : {}),
        ...(plan.entry ? { entry: plan.entry } : {}),
      }),
    );
    return saved.kind === "done"
      ? { ok: true, id }
      : { ok: true, id, note: `The doc was made, and its text was not saved. ${saved.text}` };
  } catch (err) {
    return {
      ok: true,
      id,
      note: `The doc was made, and its text was not saved. ${err instanceof Error ? err.message : ""}`,
    };
  }
}

/** A sheet: the artifact, its columns, then the rows the person filled. */
export async function commitSheet(
  title: string,
  schema: DataCollection[],
  rows: readonly Record<string, unknown>[],
): Promise<Committed> {
  const id = made(await write(createSheet({ title })));
  if (typeof id !== "string") return { ok: false, text: id.text };
  const set = await write(configureSchema(id, schema));
  if (set.kind !== "done") {
    return { ok: true, id, note: `The sheet was made, and its columns were not set. ${set.text}` };
  }
  const collection = schema[0]?.name;
  const filled = rows.filter((r) => Object.values(r).some((v) => v !== "" && v !== null));
  if (!collection || filled.length === 0) return { ok: true, id };
  const ops = filled.map((fields) => ({
    op: "set" as const,
    collection,
    doc: newActOpID().slice(0, 26),
    fields,
  }));
  const wrote = await write(writeData(id, ops, newActOpID(), { via: "dashboard" }));
  return wrote.kind === "done"
    ? { ok: true, id }
    : {
        ok: true,
        id,
        note: `The sheet was made, and its first rows were not saved. ${wrote.text}`,
      };
}

/** A deck: the starter under its title, then each slide's text the person changed. */
export async function commitDeck(
  title: string,
  edits: readonly SlideTextEdit[],
): Promise<Committed> {
  const id = made(await write(createFromStarter({ kind: "slides", title })));
  if (typeof id !== "string") return { ok: false, text: id.text };
  if (edits.length === 0) return { ok: true, id };
  try {
    const signal = new AbortController().signal;
    const { deck } = await readDeck(id, 1, signal);
    const base = draftOf(deck, 1);
    let shown = base;
    for (const e of edits) {
      const slideId = base.order[e.slide];
      const slide = slideId ? shown.slides[slideId] : undefined;
      if (!slideId || !slide) continue;
      shown = {
        ...shown,
        slides: {
          ...shown.slides,
          [slideId]: { ...slide, root: withText(slide.root, e.path, e.text) },
        },
      };
    }
    const sources: Record<string, string> = {};
    await Promise.all(
      sourcesNeeded(base, shown).map(async (path) => {
        sources[path] = await versionText(id, 1, path);
      }),
    );
    const saved = await write(publishVersion(id, 1, publishOf(base, shown, sources, "")));
    return saved.kind === "done"
      ? { ok: true, id }
      : { ok: true, id, note: `The deck was made, and your edits were not saved. ${saved.text}` };
  } catch (err) {
    return {
      ok: true,
      id,
      note: `The deck was made, and your edits were not saved. ${err instanceof Error ? err.message : ""}`,
    };
  }
}

/** A task's title from a brief: the kind, then the brief's opening words. */
export function briefTitle(kind: BuiltKind, prompt: string): string {
  const said = prompt
    .trim()
    .replace(/\s+/g, " ")
    .replace(/^(build|design|make|create)\s+/i, "");
  const cut = said.length > 72 ? `${said.slice(0, 72).replace(/\s+\S*$/, "")}…` : said;
  const noun = KIND[kind].one.replace(/^a /, "");
  return `Make ${KIND[kind].one}: ${cut || noun}`;
}

/** What an ask came to: the task filed, or why not. */
export type Asked = { ok: true; item: string } | { ok: false; text: string } | null;

/**
 * Asking an agent to build one: a task filed to the agent with the brief as
 * its description, which is how an agent is given work everywhere else.
 */
export function useAskAgent() {
  const viewer = useViewer();
  const create = useAct("create_work_item");
  // WHERE THE TASK IS FILED: the builder's own team's project, else the
  // asker's own, else the company's first, so an ask is never refused for a
  // project the person never chose.
  const projects = useQuery("work_projects", { limit: 50 }, { enabled: viewer.tracker });
  const [asked, setAsked] = useState<Asked>(null);
  const ask = useCallback(
    async (kind: BuiltKind, prompt: string, handle: string, name: string, unit = "") => {
      const rows = projects.data?.projects ?? [];
      const project =
        rows.find((p) => unit !== "" && p.unit?.key === unit)?.key ||
        viewer.project ||
        rows[0]?.key ||
        "";
      const result = await create.run(
        {
          title: briefTitle(kind, prompt),
          body: prompt.trim(),
          assignee: handle,
          ...(project ? { project } : {}),
        },
        { done: `Asked ${name} for ${KIND[kind].one}`, quiet: true },
      );
      if (!result) return null;
      // A REFUSAL STAYS ON THE HOOK (`refusal`), where the brief shows it.
      if (result.kind === "refused") return null;
      const answer: Asked =
        result.kind === "unknown"
          ? { ok: false, text: "Could not confirm the task was filed." }
          : { ok: true, item: (result.receipt as { item?: string } | null)?.item ?? "" };
      setAsked(answer);
      return answer;
    },
    [create, viewer.project, projects.data],
  );
  return { ask, asked, busy: create.busy, refusal: create.refusal, reset: () => setAsked(null) };
}

/** The address an artifact opens at, with the deck's editor open where it is a deck. */
export function landing(
  kind: ArtifactKind,
  id: string,
): { path: string[]; query?: Record<string, string> } {
  return kind === "slides"
    ? { path: ["artifacts", id], query: { mode: "edit" } }
    : { path: ["artifacts", id] };
}

/**
 * One create, from a press: the draft committed, the person taken to what
 * they made (a deck with its editor open), and a toast saying it was made,
 * or what did not land.
 */
export function useCreate() {
  const nav = useNavigator();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");
  const run = useCallback(
    async (kind: ArtifactKind, title: string, make: () => Promise<Committed>) => {
      if (busy) return;
      setBusy(true);
      setRefusal("");
      try {
        const done = await make();
        if (!done.ok) {
          setRefusal(done.text);
          return;
        }
        if (done.note)
          toast.show({ variant: "warning", title: `Made “${title}”`, message: done.note });
        else toast.ok(`Created “${title}”`);
        const to = landing(kind, done.id);
        nav.to(to.path, to.query);
      } finally {
        setBusy(false);
      }
    },
    [busy, nav, toast],
  );
  return { run, busy, refusal };
}
