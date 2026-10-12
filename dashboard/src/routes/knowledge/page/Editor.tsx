/**
 * Editing a page, as the signed-in person (`save_page`, ADR-0024).
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

import { useEffect, useId, useRef, useState } from "react";
import {
  Button,
  Callout,
  ConfirmModal,
  Document,
  FormField,
  Input,
  MarkdownEditor,
  SegmentedControl,
  markdownWords,
} from "@crewlethq/ui";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { PAGE_MESSAGE_MAX_BYTES } from "~/contract/pages.ts";
import { plural, textBudget } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { merge3, mergedText, type Merge, type Resolution } from "~/lib/merge.ts";
import { useAct } from "~/lib/useAct.ts";
import type { PageDetail } from "~/protocol/index.ts";
import { BodyDiff } from "./History.tsx";
import { LinkPageButton } from "./LinkPicker.tsx";

/** A page's markdown as the reader will see it, for an editor's Preview. */
export function previewMarkdown(value: string) {
  return <Document>{renderMarkdown(value)}</Document>;
}

export function PageEditor({ detail, onDone }: { detail: PageDetail; onDone: () => void }) {
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
  const messageID = useId();

  const moved = page.version > base.version;
  const dirty = draft !== base.body;
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
  // every sheet in the product closes on.
  const leave = () => (dirty ? setDiscard(true) : onDone());

  return (
    // THE FIELD'S ESCAPE, and only the field's: a picker's own Escape closes
    // the picker and nothing else.
    <section
      className="knowledge-editor"
      aria-label={`Editing “${page.title}”`}
      onKeyDown={(event) => {
        if (event.key !== "Escape" || !(event.target instanceof HTMLTextAreaElement)) return;
        event.preventDefault();
        leave();
      }}
    >
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
          <BodyDiff before={base.body} after={page.body ?? ""} against={`rev ${base.version}`} />
        </div>
      )}
      {merging && (
        <ResolveConflicts
          version={merging.version}
          merge={merging.merge}
          onCancel={() => setMerging(null)}
          onApply={(text) => {
            setDraft(text);
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

      <MarkdownEditor
        value={draft}
        onChange={setDraft}
        label={`The body of “${page.title}”, in markdown`}
        renderPreview={previewMarkdown}
        disabled={merging !== null}
        rows={22}
        actions={(editor) => <LinkPageButton editor={editor} exclude={page.id} />}
        onSubmit={() => {
          // THE SHORTCUT GOES THROUGH THE SAME GATE THE BUTTON DOES, so a
          // second one while the first answer is out sends nothing.
          if (pressable(save, blocked)) void press();
        }}
        footer={<span>{plural(markdownWords(draft), "word")} · Markdown</span>}
      />

      <FormField
        label="What changed"
        htmlFor={messageID}
        helper={
          messageBudget.over
            ? undefined
            : (messageBudget.line ?? "One line, kept with the revision. Optional.")
        }
        error={messageBudget.over ? messageBudget.line : undefined}
      >
        <Input
          id={messageID}
          value={message}
          error={messageBudget.over}
          onChange={(event) => setMessage(event.target.value)}
        />
      </FormField>

      <div className="knowledge-editor-foot">
        <WriteButton
          write={save}
          variant="primary"
          showRefusal={false}
          blocked={blocked}
          onPress={() => void press()}
        >
          Save
        </WriteButton>
        <Button variant="ghost" onClick={leave} disabled={save.busy}>
          Cancel
        </Button>
        <span className="knowledge-note">Saved as you, against revision {base.version}.</span>
      </div>
      {/* A REFUSAL FOR RACING A NEWER REVISION is the callout above, which says
          what to do about it; the sentence would say it twice. */}
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
  );
}

/** What each resolution keeps, in the person's words. */
const RESOLUTIONS: { value: Resolution; label: string }[] = [
  { value: "theirs", label: "Theirs" },
  { value: "mine", label: "Yours" },
  { value: "both", label: "Both" },
];

/**
 * Settling the passages both edits changed: each clash shows their lines and
 * the draft's, and the person keeps one or both. Nothing is pre-chosen, since
 * a default would be the browser deciding whose sentence survives, so Apply
 * waits for every clash to have an answer.
 */
function ResolveConflicts({
  version,
  merge,
  onApply,
  onCancel,
}: {
  version: number;
  merge: Merge;
  onApply: (text: string) => void;
  onCancel: () => void;
}) {
  const [choices, setChoices] = useState<(Resolution | undefined)[]>(() =>
    Array.from({ length: merge.conflicts }, () => undefined),
  );
  const clashes = merge.chunks.filter((c) => c.kind === "conflict");
  const settled = choices.every((c) => c !== undefined);
  const heading = useId();
  const first = useRef<HTMLHeadingElement>(null);
  useEffect(() => first.current?.focus(), []);
  return (
    <section className="knowledge-resolve" aria-labelledby={heading}>
      <h2 className="knowledge-section-title" id={heading} ref={first} tabIndex={-1}>
        {clashes.length === 1
          ? `You and revision ${version} changed the same lines`
          : `You and revision ${version} changed the same lines in ${clashes.length} places`}
      </h2>
      <p className="knowledge-note">For each conflict, keep their lines, yours or both.</p>
      <ol className="knowledge-resolve-list">
        {clashes.map((clash, i) => (
          <li key={i} className="knowledge-resolve-item">
            <div className="knowledge-resolve-sides">
              <figure className="knowledge-resolve-side">
                <figcaption>Theirs · revision {version}</figcaption>
                <pre>{clash.theirs.join("\n") || "(removed)"}</pre>
              </figure>
              <figure className="knowledge-resolve-side">
                <figcaption>Yours</figcaption>
                <pre>{clash.mine.join("\n") || "(removed)"}</pre>
              </figure>
            </div>
            <SegmentedControl
              semantics="radio"
              size="sm"
              label={`Place ${i + 1}: which lines to keep`}
              value={choices[i] ?? ""}
              onValueChange={(v) =>
                setChoices(choices.map((c, k) => (k === i ? (v as Resolution) : c)))
              }
              options={RESOLUTIONS}
            />
          </li>
        ))}
      </ol>
      <div className="knowledge-resolve-foot">
        <Button
          size="small"
          variant="primary"
          disabledReason={settled ? undefined : "Settle every place first."}
          onClick={() => onApply(mergedText(merge, choices as Resolution[]))}
        >
          Apply to my draft
        </Button>
        <Button size="small" variant="ghost" onClick={onCancel}>
          Not now
        </Button>
        {!settled && (
          <span className="knowledge-note">
            {plural(choices.filter((c) => c === undefined).length, "place")} still to settle.
          </span>
        )}
      </div>
    </section>
  );
}
