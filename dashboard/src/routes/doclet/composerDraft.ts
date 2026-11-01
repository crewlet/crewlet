/**
 * A new page as the assistant reads and changes it while it is being written
 * in the composer (`Composer.tsx`, `useAssistantDraft`).
 *
 * Nothing of it is saved until the person presses Save as draft or Publish,
 * so the draft is the page's own (`client`). Each value is held to the rule
 * the composer holds a typed one to, in the composer's own words: a title
 * within its bytes, a body within what a page holds, labels within their
 * count and length, and a space the company has.
 */

import { refuse, type AssistantDraftSpec, type DraftValue } from "~/app/assistant/draft.ts";
import {
  PAGE_BODY_MAX_BYTES,
  PAGE_LABELS_MAX,
  PAGE_LABEL_MAX_BYTES,
  PAGE_TITLE_MAX_BYTES,
} from "~/contract/pages.ts";
import { fmtBytes, plural, textBudget, utf8Bytes } from "~/lib/format.ts";
import { markdownWords } from "@crewlethq/ui";

/** What the composer holds. */
export interface ComposerValues {
  readonly space: string;
  /** The page it goes under, by its id; empty for the top of the space. */
  readonly parent: string;
  readonly title: string;
  readonly body: string;
  readonly labels: readonly string[];
}

/** What the assistant is told about the composer, beside its fields. */
const HELP =
  "A new page that is not saved yet. You can propose its title, its body in Markdown, " +
  "its labels and the space it is filed in; the page it goes under is the person's to " +
  "choose. The person saves it as a draft or publishes it.";

const text = (value: DraftValue): string | null => (typeof value === "string" ? value : null);

/** The composer's draft, around the setters the composer already has. */
export function composerDraft(args: {
  value: ComposerValues;
  /** The title of the page it goes under, as the place control names it. */
  parentTitle: string;
  /** The spaces a page may be filed in, by key. */
  spaces: readonly { key: string }[];
  apply: (next: ComposerValues) => void;
  busy: boolean;
}): AssistantDraftSpec<ComposerValues> {
  const { value, parentTitle, spaces, apply, busy } = args;
  return {
    kind: "client",
    step: "write",
    scope: "The page you are writing (not saved)",
    help: HELP,
    value,
    fields: [
      {
        path: "space",
        label: "Space",
        hint: `The key of one of: ${spaces.map((s) => s.key).join(", ")}`,
        read: (v) => v.space,
        write: (v, next) => {
          const key = text(next)?.trim() ?? "";
          if (!spaces.some((s) => s.key === key)) return refuse("Choose the space to file it in.");
          // A NEW SPACE IS A NEW PLACE: the page it went under is another
          // space's, so it goes to the top, as the space control does.
          return key === v.space ? v : { ...v, space: key, parent: "" };
        },
      },
      {
        path: "parent",
        label: "Goes under",
        read: (v) => (v.parent ? parentTitle || v.parent : "The top of the space"),
      },
      {
        path: "title",
        label: "Title",
        hint: "Also the page's link address in its space",
        read: (v) => v.title,
        write: (v, next) => {
          const title = text(next);
          if (title === null || title.trim() === "") return refuse("Give the page a title.");
          const budget = textBudget(title, PAGE_TITLE_MAX_BYTES, "a page's title");
          return budget.over ? refuse(budget.line ?? "Shorten the title.") : { ...v, title };
        },
      },
      {
        path: "body",
        label: "Body",
        hint: "Markdown",
        read: (v) => v.body,
        write: (v, next) => {
          const body = text(next);
          if (body === null || body.includes("\u0000"))
            return refuse("A page's body is Markdown text.");
          const bytes = utf8Bytes(body);
          return bytes > PAGE_BODY_MAX_BYTES
            ? refuse(
                `The page is ${fmtBytes(bytes)}, past its ${fmtBytes(PAGE_BODY_MAX_BYTES)} limit.`,
              )
            : { ...v, body };
        },
        summary: (body) => plural(markdownWords(typeof body === "string" ? body : ""), "word"),
      },
      {
        path: "labels",
        label: "Labels",
        hint: `At most ${PAGE_LABELS_MAX}, one per line`,
        read: (v) => v.labels,
        write: (v, next) => {
          const items = typeof next === "string" ? [next] : Array.isArray(next) ? next : null;
          if (items === null) return refuse("Labels are a list of short words.");
          const labels = [...new Set(items as readonly string[])];
          if (labels.length > PAGE_LABELS_MAX) {
            return refuse(`A page carries at most ${PAGE_LABELS_MAX} labels.`);
          }
          const long = labels.find((l) => utf8Bytes(l) > PAGE_LABEL_MAX_BYTES);
          return long
            ? refuse(`The label “${long}” is over ${PAGE_LABEL_MAX_BYTES} bytes. Shorten it.`)
            : { ...v, labels };
        },
      },
    ],
    apply,
    blocked: () => (busy ? "The page is being saved." : null),
  };
}
