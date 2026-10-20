/**
 * A new doc, written where it will be read: its own create page, in the
 * shared frame (`CreateShell`).
 *
 * # Straight into the page
 *
 * There is no form before the writing. The page opens as the doc will read,
 * the title set in the document's own title type with the cursor in it, and
 * the body in the editor every doc is edited with (`RichEditor`, ADR-0044),
 * so the doc looks the same before it exists, while it is written and once
 * it is saved. Enter in the title moves to the body; Command+Enter creates
 * from either; Escape asks to leave.
 *
 * # Templates fill the page, they never gate it
 *
 * While the body is empty, or still holds the template it took untouched, a
 * row of templates sits above it (`lib/artifacts/docTemplates.ts`); one press
 * fills the body, another press clears it. Once anybody writes, the row
 * steps aside, so a template never replaces writing.
 *
 * # Nothing exists until Create
 *
 * Create is one keyed write: the engine begins the doc from its starter with
 * the text as written (`doc`), its title as the tab's heading, and the page
 * opens it. A body left empty begins from the starter's own line. Until
 * then nothing is made, so leaving makes nothing either; with anything
 * written every move away asks first (`useLeaveGuard`).
 */

import { useRef, useState, type KeyboardEvent } from "react";
import {
  Callout,
  ConfirmModal,
  DocumentTitleField,
  FilterChip,
  FilterChipGroup,
  FormField,
  RichEditor,
  markdownWords,
  type RichEditorHandle,
} from "@crewlethq/ui";
import { CheckGlyph, LockGlyph } from "@crewlethq/icons/glyphs";
import { useLeaveGuard, useParam } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { DOC_TEMPLATES, docTemplate, type DocTemplate } from "~/lib/artifacts/docTemplates.ts";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import { titleHold, titleProblem, useCreateArtifact, useWorkItem } from "./create.ts";

export function DocCreate() {
  const create = useCreateArtifact();
  const access = useArtifactAccess({ app: true });
  const workItem = useWorkItem();
  const leaveCreate = useLeaveCreate();
  const [templateParam] = useParam("template", "");
  const [template, setTemplate] = useState<DocTemplate | undefined>(() =>
    docTemplate(templateParam),
  );
  const [title, setTitle] = useState("");
  const [body, setBody] = useState(template?.body ?? "");
  // A NEW KEY LOADS THE EDITOR AGAIN: a template replaces the whole body,
  // which the editor takes only as a fresh document.
  const [loaded, setLoaded] = useState(0);
  const editor = useRef<RichEditorHandle>(null);
  // WHERE A HELD MOVE WAS GOING, while Discard is asked about.
  const [leaving, setLeaving] = useState<(() => void) | null>(null);
  // LET GO, read by the guard as it is asked: the move Create makes is
  // asked of the guard in the same press that lets it go.
  const released = useRef(false);

  const name = title.trim();
  const written = body.trim() !== "";
  // THE TEMPLATES ARE OFFERED while nothing of the writer's own is on the
  // page: an empty body, or a template's untouched.
  const untouched = !written || (template !== undefined && body === template.body);
  const words = markdownWords(body);
  const problem = titleProblem(title);
  const hold = !access.can ? access.reason : titleHold(title);

  const dirty = name !== "" || (written && !untouched);
  useLeaveGuard(
    dirty
      ? (_to, leave) => {
          if (released.current) return false;
          setLeaving(() => leave);
          return true;
        }
      : null,
  );

  // CANCEL AND ESCAPE ASK FIRST while anything is written: the shell leaves
  // by replacing the entry, which no guard holds.
  const cancel = () => (dirty ? setLeaving(() => leaveCreate) : leaveCreate());

  const press = async () => {
    if (hold || create.busy) return;
    released.current = true;
    await create.fromStarter("doc", name, written ? { doc: { body } } : {});
    // STILL HERE: the engine refused, and its sentence is on the page.
    released.current = false;
  };

  const startFrom = (key: string | null) => {
    const next = key ? docTemplate(key) : undefined;
    setTemplate(next);
    setBody(next?.body ?? "");
    setLoaded((n) => n + 1);
  };

  const onTitleKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      void press();
    } else if (event.key === "Enter" || event.key === "ArrowDown") {
      event.preventDefault();
      editor.current?.focus();
    } else if (event.key === "Escape") {
      event.preventDefault();
      cancel();
    }
  };

  return (
    <CreateShell
      kind="doc"
      onCancel={cancel}
      status={create.busy ? "Creating…" : words > 0 ? plural(words, "word") : undefined}
      action={{
        label: "Create doc",
        icon: <CheckGlyph size="sm" />,
        keys: ["Mod", "Enter"],
        onSelect: () => void press(),
        ...(create.busy
          ? { disabledReason: "Creating the doc." }
          : hold
            ? { disabledReason: hold }
            : {}),
      }}
    >
      <div className="doc-create">
        <div className="doc-create-page">
          {/* THE ENGINE'S REFUSAL AT THE TOP OF THE PAGE, where the writer
              looks after pressing Create, not under a long body. */}
          {create.refusal && (
            <Callout variant="danger" title="The doc was not created" role="alert">
              {create.refusal}
            </Callout>
          )}
          <p className="doc-create-where">
            <LockGlyph size="sm" />
            <span>
              Private to you until you share it
              {workItem ? `, and listed on ${workItem}` : ""}
            </span>
          </p>

          <FormField htmlFor="doc-create-title" error={problem}>
            {(field) => (
              <DocumentTitleField
                id={field.id}
                aria-label="Title"
                aria-describedby={field.describedBy}
                placeholder={template?.titleExample ?? "Untitled doc"}
                autoComplete="off"
                autoFocus
                value={title}
                error={problem !== undefined}
                disabled={create.busy}
                onChange={(event) => setTitle(event.target.value)}
                onKeyDown={onTitleKey}
              />
            )}
          </FormField>

          {untouched && (
            <FilterChipGroup
              className="doc-create-starts"
              label="Start from a template"
              hideLabel
              semantics="radio"
              activate="manual"
              allowNone
              value={template?.key ?? null}
              onValueChange={startFrom}
            >
              {DOC_TEMPLATES.map((t) => (
                <FilterChip key={t.key} value={t.key} aria-description={t.description}>
                  {t.label}
                </FilterChip>
              ))}
            </FilterChipGroup>
          )}

          <RichEditor
            key={loaded}
            ref={editor}
            className="doc-create-editor"
            value={body}
            onChange={setBody}
            label={name ? `The text of “${name}”` : "The text of the new doc"}
            placeholder="Write, or type a slash for blocks"
            disabled={create.busy}
            onSubmit={() => void press()}
            onEscape={cancel}
          />
        </div>
      </div>

      <ConfirmModal
        open={leaving !== null}
        destructive
        title="Discard this doc?"
        message="What you wrote is not saved anywhere."
        cancelLabel="Keep writing"
        confirmLabel="Discard"
        onClose={() => setLeaving(null)}
        onConfirm={() => {
          const go = leaving;
          released.current = true;
          setLeaving(null);
          go?.();
        }}
      />
    </CreateShell>
  );
}
