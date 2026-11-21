/**
 * A change's description, the first card of its conversation: who opened it,
 * what it says, and its menu of Edit, Copy link and Quote. Edit changes the
 * title and the description in ONE `code_update_change` against the version on screen.
 */

import { useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import {
  Button,
  FormField,
  Input,
  MarkdownEditor,
  Menu,
  Stack,
  Tag,
  useToast,
} from "@crewlethq/ui";
import { EllipsisGlyph, LinkGlyph, PencilGlyph, TextQuoteGlyph } from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import { RefusalsTo, WriteButton, pressable, useRefusalPlace } from "~/components/WriteButton.tsx";
import { changeLive } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { copyLink } from "~/lib/pageLink.ts";
import { useAct } from "~/lib/useAct.ts";
import { menuHold } from "~/lib/useWriteAccess.ts";
import type { CodeChangeDetail } from "~/protocol/index.ts";
import { changeHref } from "../paths.ts";
import { Said } from "./Timeline.tsx";
import "~/styles/gitlet.css";

/** Why Save is held while the words on screen moved under the reader. */
export const EDIT_MOVED =
  "Its title or description changed since you began editing. Start again from the latest to keep both.";

/** The description as a quote: each line after `> `, then a blank line. */
export function quoteOf(body: string): string {
  return `${body
    .split("\n")
    .map((line) => (line ? `> ${line}` : ">"))
    .join("\n")}\n\n`;
}

export function Description({
  change,
  onQuote,
}: {
  change: CodeChangeDetail;
  /** Puts the description, quoted, into the comment box. */
  onQuote: (text: string) => void;
}) {
  const now = useNow();
  const toast = useToast();
  const update = useAct("code_update_change");
  const [editing, setEditing] = useState(false);
  const head = useRef<HTMLDivElement>(null);
  // FOCUS GOES BACK TO THE MENU'S TRIGGER once the form closes.
  const backToMenu = () =>
    queueMicrotask(() => head.current?.querySelector<HTMLElement>("[aria-haspopup]")?.focus());
  const body = change.body ?? "";
  const live = changeLive(change.state);
  const items = [
    ...(live
      ? [
          {
            key: "edit",
            label: "Edit",
            icon: <PencilGlyph size="sm" />,
            onSelect: () => setEditing(true),
            ...menuHold(update.access),
          },
        ]
      : []),
    {
      key: "link",
      label: "Copy link",
      icon: <LinkGlyph size="sm" />,
      onSelect: () =>
        void copyLink(changeHref(change.repo, change.number)).then((word) =>
          word === "Copied"
            ? toast.ok(`Copied the link to ${change.ref}`)
            : toast.show({ variant: "warning", message: "The browser refused the clipboard" }),
        ),
    },
    ...(body.trim()
      ? [
          {
            key: "quote",
            label: "Quote",
            icon: <TextQuoteGlyph size="sm" />,
            onSelect: () => onQuote(quoteOf(body)),
          },
        ]
      : []),
  ];
  return (
    <section aria-label="Description" className="code-description">
      <div className="code-description__head" ref={head}>
        <Said who={change.owner} verb="opened this change" at={change.opened_at} now={now} />
        <span className="code-description__marks">
          <Tag size="sm" appearance="outline">
            Author
          </Tag>
          <Menu
            label="More actions on the description"
            icon={<EllipsisGlyph size="sm" />}
            triggerVariant="ghost"
            align="end"
            items={items}
          />
        </span>
      </div>
      <div className="code-description__body">
        {editing ? (
          <EditForm
            change={change}
            onDone={() => {
              setEditing(false);
              backToMenu();
            }}
          />
        ) : body ? (
          <div className="prose md">{renderMarkdown(body)}</div>
        ) : (
          <p className="muted">No description.</p>
        )}
      </div>
    </section>
  );
}

/** The title and the description, edited in place and saved as one update. */
function EditForm({ change, onDone }: { change: CodeChangeDetail; onDone: () => void }) {
  const update = useAct("code_update_change");
  // SAVE'S REFUSAL ON A LINE OF ITS OWN, under the row, so the row keeps one height.
  const refusals = useRefusalPlace("code-description__refusal");
  const [opened, setOpened] = useState(() => ({ title: change.title, body: change.body ?? "" }));
  const [title, setTitle] = useDraftState(opened.title);
  const [body, setBody] = useDraftState(opened.body);
  useDraftBox({
    path: "edit.title",
    label: `The title of ${change.ref}`,
    hint: "One line",
    rank: 1,
    value: title,
    set: setTitle,
    refuses: () => null,
  });
  useDraftBox({
    path: "edit.body",
    label: `The description of ${change.ref}`,
    hint: "Markdown",
    rank: 1,
    value: body,
    set: setBody,
    refuses: () => null,
  });
  // WHAT IS ON SCREEN MOVED UNDER THE FORM (a seat's edit), never a push's version alone.
  const stale = change.title !== opened.title || (change.body ?? "") !== opened.body;
  const titleMoved = title !== opened.title;
  const bodyMoved = body !== opened.body;
  const blocked = stale
    ? EDIT_MOVED
    : !title.trim()
      ? "A change needs a title."
      : !titleMoved && !bodyMoved
        ? "Change the title or the description first."
        : undefined;
  const which =
    titleMoved && bodyMoved ? "title and description" : titleMoved ? "title" : "description";
  const save = async () => {
    if (!pressable(update, blocked)) return;
    const result = await update.run(
      {
        change: change.ref,
        expected_version: change.version,
        ...(titleMoved ? { title } : {}),
        ...(bodyMoved ? { body } : {}),
      },
      { done: `Saved ${change.ref}'s ${which}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onDone();
  };
  const again = () => {
    const latest = { title: change.title, body: change.body ?? "" };
    setOpened(latest);
    setTitle(latest.title);
    setBody(latest.body);
  };
  const keys = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === "Escape" && !titleMoved && !bodyMoved) {
      event.preventDefault();
      onDone();
    }
  };
  return (
    <form
      aria-label={`Edit ${change.ref}`}
      onKeyDown={keys}
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        void save();
      }}
    >
      <Stack gap={3}>
        <FormField label="Title">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              value={title}
              autoFocus
              onFocus={(event) => event.currentTarget.select()}
              onChange={(event) => setTitle(event.target.value)}
            />
          )}
        </FormField>
        <FormField label="Description">
          {(field) => (
            <MarkdownEditor
              id={field.id}
              aria-describedby={field.describedBy}
              label="Description"
              placeholder="What it changes and why. Markdown works."
              value={body}
              rows={8}
              onChange={setBody}
              onSubmit={() => void save()}
              renderPreview={(value) => <div className="prose md">{renderMarkdown(value)}</div>}
            />
          )}
        </FormField>
        <RefusalsTo place={refusals.element}>
          <div className="code-description__actions">
            {stale && (
              <Button size="small" variant="ghost" onClick={again}>
                Start again from the latest
              </Button>
            )}
            <Button size="small" variant="secondary" onClick={onDone} disabled={update.busy}>
              Cancel
            </Button>
            <WriteButton
              write={update}
              size="small"
              variant="primary"
              type="submit"
              blocked={blocked}
              onPress={() => void save()}
            >
              Save
            </WriteButton>
          </div>
        </RefusalsTo>
        {refusals.place}
        {blocked && <p className="muted t-caption">{blocked}</p>}
      </Stack>
    </form>
  );
}
