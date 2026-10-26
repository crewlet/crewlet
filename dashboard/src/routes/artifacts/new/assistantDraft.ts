/**
 * A create page's draft as the assistant reads and changes it
 * (`useAssistantDraft`): the part every create page shares, declared once.
 *
 * # What every create page holds
 *
 * A title, and the hand-off to an agent beside it: the brief, the agent that
 * builds it and the project its task is filed in (`WhoBuildsIt.tsx`,
 * `AskAgent.tsx`). Each is set through the page's own setter and refused in
 * the sentence the page's own control would say: a title past its bound, an
 * agent or a project the page does not offer. What a kind holds beyond that
 * (a doc's body, a deck's theme, a canvas's boards) is the page's own
 * `extra`, lifted into the same draft so one `draft.set` can change both.
 *
 * # Nothing is made by it
 *
 * Create and Ask stay the person's presses: the help says so, and nothing
 * here sends anything. A change only fills the page, which the person reads
 * and then makes, asks for, or leaves.
 */

import {
  DraftRefusal,
  refuse,
  type AssistantDraftSpec,
  type DraftFieldSpec,
} from "~/app/assistant/draft.ts";
import { titleProblem } from "./create.ts";
import type { WhoBuildsItState } from "./WhoBuildsIt.tsx";

/** The most choices a field's hint names: the rest are the page's to show. */
const HINT_CHOICES = 8;

/** What every create page holds, beside the kind's own state. */
export interface CreateDraftValue<E> {
  readonly title: string;
  readonly brief: string;
  readonly seat: string;
  readonly project: string;
  readonly extra: E;
}

/** A create page's state, as `createDraft` takes it. */
export interface CreateDraftArgs<E> {
  /** The kind's word, as the scope line names it ("doc", "slide deck"). */
  what: string;
  /** What the assistant can and cannot do on this kind's page, beyond what every page says. */
  help: string;
  /** The section the person is in, where the page has more than one. */
  step?: string | null;
  title: string;
  setTitle: (title: string) => void;
  brief: string;
  setBrief: (brief: string) => void;
  who: WhoBuildsItState;
  /** The kind's own state, its fields, and the page's setter for it. */
  extra?: {
    value: E;
    fields: readonly DraftFieldSpec<E>[];
    apply: (next: E) => void;
  };
  /** Why the page takes no change now (a create in flight), or null. */
  blocked?: () => string | null;
}

/** What every create page says, after the kind's own help. */
const SHARED_HELP =
  "Nothing is made until the person presses Create, or Ask to hand it to an agent.";

/** A list of choices as a hint names them, the first few and a count of the rest. */
function choices(values: readonly string[]): string {
  const shown = values.slice(0, HINT_CHOICES).join(", ");
  const rest = values.length - HINT_CHOICES;
  return rest > 0 ? `${shown} and ${rest} more` : shown;
}

/**
 * A list value's items, trimmed and without blanks: a list as it is, a text
 * one item per line; null for any other value, which the field refuses.
 */
export function linesOf(value: unknown): string[] | null {
  const items =
    typeof value === "string"
      ? value.split("\n")
      : Array.isArray(value) && value.every((v) => typeof v === "string")
        ? (value as string[])
        : null;
  return items?.map((l) => l.trim()).filter((l) => l !== "") ?? null;
}

/** A kind's own field, lifted to read and write the shared draft's `extra`. */
function lifted<E>(field: DraftFieldSpec<E>): DraftFieldSpec<CreateDraftValue<E>> {
  const { read, write, file, ...rest } = field;
  // THE KIND'S OWN WRITE, over the draft's `extra`: a refusal stays the kind's.
  const over =
    <V>(fn: (extra: E, value: V) => E | DraftRefusal) =>
    (d: CreateDraftValue<E>, value: V): CreateDraftValue<E> | DraftRefusal => {
      const extra = fn(d.extra, value);
      return extra instanceof DraftRefusal ? extra : { ...d, extra };
    };
  return {
    ...rest,
    read: (d) => read(d.extra),
    ...(write ? { write: over(write) } : {}),
    ...(file ? { file: { accepts: file.accepts, write: over(file.write) } } : {}),
  };
}

/**
 * The create page's draft: its title and hand-off, settable through the
 * page's setters, and the kind's own fields beside them.
 */
export function createDraft<E>(args: CreateDraftArgs<E>): AssistantDraftSpec<CreateDraftValue<E>> {
  const { who, extra } = args;
  const agents = who.builders.map((b) => b.seat.handle);
  const projects = who.projects.map((p) => p.key);
  const value: CreateDraftValue<E> = {
    title: args.title,
    brief: args.brief,
    seat: who.seat,
    project: who.project,
    extra: extra?.value as E,
  };
  const fields: DraftFieldSpec<CreateDraftValue<E>>[] = [
    {
      path: "title",
      label: "Title",
      read: (d) => d.title,
      write: (d, next) => {
        if (typeof next !== "string") return refuse("The title is a line of text.");
        const wrong = titleProblem(next);
        return wrong ? refuse(wrong) : { ...d, title: next };
      },
    },
    ...(extra?.fields ?? []).map(lifted),
    {
      path: "ask.brief",
      label: "Brief for an agent",
      hint: "What the agent should make, sent only when the person presses Ask",
      read: (d) => d.brief,
      write: (d, next) =>
        typeof next === "string" ? { ...d, brief: next } : refuse("The brief is text."),
    },
    {
      path: "ask.agent",
      label: "Agent who builds it",
      hint: agents.length > 0 ? `One of ${choices(agents)}` : "No agent is on the chart yet",
      read: (d) => d.seat,
      write: (d, next) =>
        typeof next === "string" && agents.includes(next.trim())
          ? { ...d, seat: next.trim() }
          : refuse(
              agents.length > 0
                ? `Choose one of the agents offered: ${choices(agents)}.`
                : "Add an agent to the chart first.",
            ),
    },
    {
      path: "ask.project",
      label: "Project the task is filed in",
      hint: projects.length > 0 ? `A project key: ${choices(projects)}` : "No project is known yet",
      read: (d) => d.project,
      write: (d, next) =>
        typeof next === "string" && projects.includes(next.trim())
          ? { ...d, project: next.trim() }
          : refuse("Choose the project to file the task in, by its key."),
    },
  ];
  return {
    kind: "client",
    step: args.step ?? null,
    scope: `The new ${args.what} on this page (not created)`,
    help: `${args.help} ${SHARED_HELP}`,
    value,
    fields,
    apply: (next) => {
      if (next.title !== args.title) args.setTitle(next.title);
      if (next.brief !== args.brief) args.setBrief(next.brief);
      if (next.seat !== who.seat) who.setSeat(next.seat);
      if (next.project !== who.project) who.setProject(next.project);
      if (extra && next.extra !== extra.value) extra.apply(next.extra);
    },
    ...(args.blocked ? { blocked: args.blocked } : {}),
  };
}
