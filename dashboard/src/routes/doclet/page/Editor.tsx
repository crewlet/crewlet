/**
 * Editing a page, as the signed-in person (`save_page`, ADR-0024).
 *
 * # Edited in place
 *
 * The body is edited where it is read (`RichEditor` from the design system):
 * the page keeps its look while it is edited, and the page bar's Edit becomes
 * the editing state, Cancel and Save. The body stays Markdown, and a save with
 * no edit sends the bytes that were opened, so the line merge below sees only
 * the lines somebody actually changed.
 *
 * # Against the version the person opened
 *
 * A save states the version it edited (`base_version`), because there is no
 * per-field merge that makes overwriting prose safe: a save that raced
 * somebody else's is refused `stale_version` rather than silently replacing
 * their paragraph. The editor keeps that base as its own state, apart from the
 * page the screen keeps polling, so a save that lands while this one is open
 * is SEEN (the page moved past the base) before the person presses Save, and
 * the refusal, if they press it anyway, is not a surprise.
 *
 * # What to do about somebody else's save
 *
 * Two answers, and they are the person's to choose: VIEW THEIR CHANGE, the
 * diff from the version this edit started from to the one saved since; and
 * KEEP EDITING ON TOP OF IT, which MERGES their change into the draft
 * (`lib/merge.ts`) and only then moves the base to their version. Moving the
 * base alone would be the one thing the engine's refusal exists to stop: the
 * draft still holds the older text, the next save is accepted against the new
 * version, and whatever they wrote is deleted by somebody who never saw it.
 *
 * A merge with no overlap becomes the draft at once. Where both edits touched
 * the same lines, the base stays where it is and each clash is shown, their
 * text beside the draft's, for the person to settle: keep theirs, keep yours,
 * or keep both. Nothing is saved until they press Save.
 */

import { useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  ConfirmModal,
  EditingStatus,
  EmptyState,
  Input,
  MergeConflicts,
  RichEditor,
  VersionCompare,
  markdownWords,
} from "@crewlethq/ui";
import { CheckGlyph } from "@crewlethq/icons/glyphs";
import { DiffView } from "~/components/DiffView.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { PAGE_MESSAGE_MAX_BYTES } from "~/contract/pages.ts";
import { plural, textBudget } from "~/lib/format.ts";
import { shownImage } from "~/lib/markdown.ts";
import { clashesOf, merge3, mergedText, type Merge } from "~/lib/merge.ts";
import { useAct } from "~/lib/useAct.ts";
import type { PageDetail } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { linkPageCommand } from "./LinkPicker.tsx";
import { useAssistantContext } from "~/app/assistant/useAssistantContext.ts";
import { refuse, useAssistantDraft } from "~/app/assistant/draft.ts";
import { pageBodyProblem } from "./assistantAction.ts";

export function PageEditor({
  detail,
  reading,
  onDone,
}: {
  detail: PageDetail;
  /** The body as it reads, drawn while the editor's code loads so nothing moves. */
  reading: ReactNode;
  onDone: () => void;
}) {
  const page = detail.page;
  const save = useAct("save_page");
  // WHAT THIS EDIT IS AGAINST, its version and that version's body, held
  // apart from the page the screen polls. See the header.
  const [base, setBase] = useState({ version: page.version, body: page.body ?? "" });
  const [draft, setDraft] = useState(page.body ?? "");
  const [message, setMessage] = useState("");
  const [theirs, setTheirs] = useState(false);
  const [discard, setDiscard] = useState(false);
  // A MERGE WAITING ON THE PERSON: their version, and the clashes to settle.
  const [merging, setMerging] = useState<{
    version: number;
    body: string;
    merge: Merge;
  } | null>(null);
  const [merged, setMerged] = useState<number | null>(null);
  // ONE COMMAND LIST for the life of the edit, so the editor's menu is not
  // rebuilt on every keystroke.
  const [commands] = useState(() => [linkPageCommand(page.id)]);

  const moved = page.version > base.version;
  const dirty = draft !== base.body;

  // --- assistant page actions (WP12) ---
  // THE OPEN DRAFT, declared once (`useAssistantDraft`): its body reaches the
  // assistant, and a new body is set as typing writes it ([pageBodyProblem]);
  // the save stays the person's. The page and its base are what is in view.
  useAssistantContext({
    resources: [{ kind: "page", id: page.id, version: String(base.version) }],
  });
  useAssistantDraft<string>({
    kind: "client",
    id: page.id,
    revision: base.version,
    scope: `The draft of “${page.title}” you are editing (not saved)`,
    help:
      "The body is Markdown and a change replaces all of it. Where the body here is cut, " +
      "read the page in full before proposing a new one. Saving stays the person's.",
    value: draft,
    fields: [
      {
        path: "body",
        label: "Body",
        hint: "Markdown",
        read: (d) => d,
        write: (_, body) => {
          const problem =
            typeof body === "string" ? pageBodyProblem(body) : "A page's body is Markdown text.";
          return problem === null ? (body as string) : refuse(problem);
        },
        summary: (body) => plural(markdownWords(typeof body === "string" ? body : ""), "word"),
      },
    ],
    apply: setDraft,
    blocked: () =>
      merging
        ? "Settle where both edits changed the same lines first."
        : save.busy
          ? "The page is being saved."
          : null,
  });
  // --- end assistant page actions (WP12) ---
  // WHY SAVE CANNOT BE PRESSED, one answer for the button and the shortcut
  // alike: a shortcut that skipped the "moved" clause sent a save the engine
  // could only refuse.
  const messageBudget = textBudget(message, PAGE_MESSAGE_MAX_BYTES, "a save's note");
  const blocked = merging
    ? "Settle where both edits changed the same lines first."
    : !dirty
      ? "Nothing has changed yet."
      : moved
        ? `Revision ${page.version} was saved since you started. Keep editing on top of it first.`
        : messageBudget.over
          ? messageBudget.line
          : undefined;

  const press = async () => {
    const result = await save.run(
      {
        page: page.id,
        base_version: base.version,
        body: draft,
        ...(message.trim() ? { message: message.trim() } : {}),
      },
      { done: `Saved “${page.title}”` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onDone();
  };

  // ESCAPE LEAVES, asking first when there is something to lose: the same key
  // every sheet in the product closes on. The editor calls it only when no
  // menu or panel of its own is open, whose Escape closes that and nothing else.
  const leave = () => (dirty ? setDiscard(true) : onDone());

  return (
    <>
      {/* THE PAGE BAR WHILE EDITING: the state, Cancel and Save, where Edit
          was, so the control a writer pressed is where they finish, and
          nothing else: the search the bar holds while reading is not what an
          edit is for, and on a phone it pushed Save out of the bar. */}
      <PageActions>
        <EditingStatus dirty={dirty} saving={save.busy} />
        <Button size="small" variant="ghost" onClick={leave} disabled={save.busy}>
          Cancel
        </Button>
        <WriteButton
          write={save}
          size="small"
          variant="primary"
          showRefusal={false}
          blocked={blocked}
          onPress={() => void press()}
        >
          Save
        </WriteButton>
      </PageActions>
      <section className="knowledge-editor" aria-label={`Editing “${page.title}”`}>
        {moved && !merging && (
          <Callout
            variant="warning"
            title={`Somebody saved revision ${page.version} while you edited.`}
          >
            Someone saved revision {page.version} since you started. Bring their change in first.
            <span className="knowledge-callout-actions">
              <Button size="small" variant="secondary" onClick={() => setTheirs(!theirs)}>
                {theirs ? "Hide their change" : "View their change"}
              </Button>
              <Button
                size="small"
                variant="secondary"
                onClick={() => {
                  const theirBody = page.body ?? "";
                  const merge = merge3(base.body, draft, theirBody);
                  setTheirs(false);
                  save.dismiss();
                  if (merge.conflicts === 0) {
                    setDraft(mergedText(merge));
                    setBase({ version: page.version, body: theirBody });
                    setMerged(page.version);
                  } else {
                    setMerging({ version: page.version, body: theirBody, merge });
                  }
                }}
              >
                Keep editing on top of it
              </Button>
            </span>
          </Callout>
        )}
        {moved && theirs && !merging && (
          <div className="knowledge-theirs">
            <VersionCompare
              title="What changed since you started"
              fromLabel={`rev ${base.version}`}
              toLabel="the saved page"
              lens="changes"
              changes={
                <DiffView
                  before={base.body}
                  after={page.body ?? ""}
                  against={`rev ${base.version}`}
                  aria-label={`Changes from rev ${base.version} to the saved page`}
                  empty={
                    <EmptyState
                      size="compact"
                      icon={<CheckGlyph size="xl" />}
                      title="The body did not change"
                      description="Only the title, labels or position changed."
                    />
                  }
                />
              }
            />
          </div>
        )}
        {merging && (
          <MergeConflicts
            clashes={clashesOf(merging.merge)}
            theirs={`revision ${merging.version}`}
            onCancel={() => setMerging(null)}
            onApply={(choices) => {
              setDraft(mergedText(merging.merge, choices));
              setBase({ version: merging.version, body: merging.body });
              setMerged(merging.version);
              setMerging(null);
            }}
          />
        )}
        {merged !== null && !moved && !merging && (
          <p className="knowledge-note" role="status">
            Revision {merged} is merged into your draft.
          </p>
        )}

        <RichEditor
          value={draft}
          onChange={setDraft}
          label={`The body of “${page.title}”`}
          placeholder="Write the page, or type a slash for blocks"
          disabled={merging !== null}
          autoFocus
          commands={commands}
          resolveImage={shownImage}
          fallback={reading}
          onEscape={leave}
          onSubmit={() => {
            // THE SHORTCUT GOES THROUGH THE SAME GATE THE BUTTON DOES, so a
            // second one while the first answer is out sends nothing.
            if (pressable(save, blocked)) void press();
          }}
        />

        <div className="knowledge-editor-foot">
          <Input
            containerClassName="knowledge-editor-note"
            aria-label="What changed, kept with the revision"
            placeholder="What changed (optional)"
            value={message}
            error={messageBudget.over}
            onChange={(event) => setMessage(event.target.value)}
          />
          <span className="knowledge-note">
            {plural(markdownWords(draft), "word")} · Saved as you, against revision {base.version}
          </span>
        </div>
        {messageBudget.line && (
          <p className="knowledge-note" role={messageBudget.over ? "alert" : undefined}>
            {messageBudget.line}
          </p>
        )}
        {/* A REFUSAL FOR RACING A NEWER REVISION is the callout above, which
            says what to do about it; the sentence would say it twice. */}
        {!moved && <RefusalNote write={save} />}

        {discard && (
          <ConfirmModal
            open
            destructive
            title="Discard your edit?"
            message="The page stays as it is."
            cancelLabel="Keep editing"
            confirmLabel="Discard"
            onClose={() => setDiscard(false)}
            onConfirm={onDone}
          />
        )}
      </section>
    </>
  );
}
