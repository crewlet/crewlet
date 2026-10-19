/**
 * Editing one tab of a doc, as the signed-in person: Save publishes the change
 * as a new version based on the version the edit started from (D54).
 *
 * # Against the version the person opened
 *
 * The save names its base (`If-Match`), and the engine refuses one that raced
 * somebody else's publish (`version_moved`) rather than letting it replace
 * their change. The change goes as the one exact edit of the tab's file
 * ([editOf]), so a save sends the passage a person changed.
 *
 * # What to do about somebody else's publish
 *
 * The same answer the Knowledge editor gives (`lib/merge.ts`): their version
 * of the tab is merged into the draft by line against the text this edit
 * started from, and only then does the base move to their version. A merge
 * with no overlap becomes the draft at once, to be saved again; where both
 * changed the same lines, each clash is shown for the person to settle with
 * `MergeConflicts`. Nothing is saved until they press Save.
 */

import { useEffect, useId, useState } from "react";
import {
  Button,
  Callout,
  ConfirmModal,
  Document,
  FormField,
  Input,
  MarkdownEditor,
  MergeConflicts,
  markdownWords,
  useToast,
} from "@crewlethq/ui";
import type { ArtifactDetail, DocTab } from "~/protocol/index.ts";
import { plural, textBudget } from "~/lib/format.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { clashesOf, merge3, mergedText, type Merge } from "~/lib/merge.ts";
import { publishVersion, readDoc } from "~/lib/artifacts/api.ts";
import { editOf } from "~/lib/artifacts/doc.ts";
import { ARTIFACT_LABEL_MAX_BYTES } from "~/contract/artifacts.ts";

export function DocEditor({
  detail,
  version,
  tab,
  titled,
  image,
  onDraft,
  onDone,
  onPublished,
}: {
  detail: ArtifactDetail;
  version: number;
  tab: DocTab;
  /** Whether the doc has several tabs, so the editor names the one it edits. */
  titled: boolean;
  /** How the preview shows the tab's pictures. */
  image: (src: string) => string | null;
  /** Whether a change is drafted, for the room. */
  onDraft: (drafting: boolean) => void;
  onDone: () => void;
  onPublished: (version: number) => void;
}) {
  const toast = useToast();
  const save = useKeyedGesture();
  // WHAT THIS EDIT IS AGAINST, its version and that version's text of the tab.
  const [base, setBase] = useState({ version, text: tab.text });
  const [draft, setDraft] = useState(tab.text);
  const [label, setLabel] = useState("");
  const [merging, setMerging] = useState<{ version: number; text: string; merge: Merge } | null>(
    null,
  );
  const [merged, setMerged] = useState<number | null>(null);
  const [discard, setDiscard] = useState(false);
  const labelID = useId();
  const name = titled ? `“${tab.title}”` : `“${detail.title}”`;

  const dirty = draft !== base.text;
  useEffect(() => onDraft(dirty), [dirty, onDraft]);

  const labelBudget = textBudget(label, ARTIFACT_LABEL_MAX_BYTES, "a version's label");
  const blocked = merging
    ? "Settle where both edits changed the same lines first."
    : !dirty
      ? "Nothing has changed yet."
      : base.text === ""
        ? "This tab is empty, so it can only be republished with its files, not edited here."
        : draft.trim() === ""
          ? "A tab holds some text."
          : labelBudget.over
            ? labelBudget.line
            : undefined;

  const press = async () => {
    const edit = editOf(tab.path, base.text, draft);
    if (!edit) return;
    const answer = await save.run(
      publishVersion(detail.id, base.version, {
        mode: "patch",
        edits: [edit],
        files: [],
        ...(label.trim() ? { label: label.trim() } : {}),
      }),
    );
    if (answer?.kind === "done") {
      const n = Number(answer.body.number ?? base.version + 1);
      toast.ok(`Saved as version ${n}`);
      onPublished(n);
      return;
    }
    if (answer?.kind === "refused" && answer.error?.code === "version_moved") {
      await bringIn(answer.error.body.live as { number?: unknown } | undefined, answer.text);
      return;
    }
    if (answer)
      toast.show({ variant: "danger", title: "Could not save the doc", message: answer.text });
  };

  // THE LIVE VERSION the refusal names, whose text of this tab is merged into
  // the draft against the text this edit started from.
  const bringIn = async (live: { number?: unknown } | undefined, said: string) => {
    const head = Number(live?.number);
    if (!Number.isInteger(head) || head < 1) {
      toast.show({ variant: "danger", title: "Could not save the doc", message: said });
      return;
    }
    let theirs: string | undefined;
    try {
      const newer = await readDoc(detail.id, head, new AbortController().signal);
      theirs = newer.doc.tabs.find((t) => t.path === tab.path)?.text;
    } catch (err) {
      toast.show({
        variant: "danger",
        title: `Could not read version ${head}`,
        message: err instanceof Error ? err.message : "The engine did not answer.",
      });
      return;
    }
    if (theirs === undefined) {
      toast.show({
        variant: "warning",
        title: `Version ${head} removed this tab`,
        message:
          "Copy what you wrote before you leave, then add the tab back if it is still needed.",
      });
      return;
    }
    const merge = merge3(base.text, draft, theirs);
    save.reset();
    if (merge.conflicts === 0) {
      setDraft(mergedText(merge));
      setBase({ version: head, text: theirs });
      setMerged(head);
    } else {
      setMerging({ version: head, text: theirs, merge });
    }
  };

  const leave = () => (dirty ? setDiscard(true) : onDone());

  return (
    <section
      className="doc-editor"
      aria-label={`Editing ${name}`}
      onKeyDown={(event) => {
        if (event.key !== "Escape" || !(event.target instanceof HTMLTextAreaElement)) return;
        event.preventDefault();
        leave();
      }}
    >
      {merging && (
        <MergeConflicts
          clashes={clashesOf(merging.merge)}
          theirs={`version ${merging.version}`}
          onCancel={() => setMerging(null)}
          onApply={(choices) => {
            setDraft(mergedText(merging.merge, choices));
            setBase({ version: merging.version, text: merging.text });
            setMerged(merging.version);
            setMerging(null);
          }}
        />
      )}
      {merged !== null && !merging && (
        <Callout variant="info" title={`Version ${merged} is merged into your draft.`}>
          Somebody published while you were editing. Their change is kept beside yours. Save again
          to publish both.
        </Callout>
      )}

      <MarkdownEditor
        value={draft}
        onChange={setDraft}
        label={`The text of ${name}, in Markdown`}
        renderPreview={(value) => <Document>{renderMarkdown(value, { image })}</Document>}
        disabled={merging !== null}
        rows={22}
        onSubmit={() => {
          if (!save.busy && !blocked) void press();
        }}
        footer={<span>{plural(markdownWords(draft), "word")} · Markdown</span>}
      />

      <FormField
        label="Label"
        htmlFor={labelID}
        helper={
          labelBudget.over
            ? undefined
            : (labelBudget.line ?? "Names the version this save makes, which keeps it. Optional.")
        }
        error={labelBudget.over ? labelBudget.line : undefined}
      >
        <Input
          id={labelID}
          value={label}
          error={labelBudget.over}
          onChange={(event) => setLabel(event.target.value)}
        />
      </FormField>

      <div className="doc-editor-foot">
        <Button
          variant="primary"
          loading={save.busy}
          disabledReason={blocked}
          onClick={() => void press()}
        >
          Save
        </Button>
        <Button variant="ghost" onClick={leave} disabled={save.busy}>
          Cancel
        </Button>
        <span className="doc-note">
          Saved as you, as a new version based on version {base.version}.
        </span>
      </div>

      {discard && (
        <ConfirmModal
          open
          destructive
          title="Discard your edit?"
          message="The doc stays as it is."
          cancelLabel="Keep editing"
          confirmLabel="Discard"
          onClose={() => setDiscard(false)}
          onConfirm={onDone}
        />
      )}
    </section>
  );
}
