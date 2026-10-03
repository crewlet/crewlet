/**
 * Editing a page, as the signed-in person (`save_page`, ADR-0024).
 *
 * # Against the version the person opened
 *
 * A save states the version it edited (`base_version`), because there is no
 * per-field merge that makes overwriting prose safe: a save that raced
 * somebody else's is refused `stale_version` rather than silently replacing
 * their paragraph. The editor keeps that base as its own state, apart from
 * the page the screen keeps polling — so a save that lands while this one is
 * open is SEEN (the page moved past the base) before the person presses Save,
 * and the refusal, if they press it anyway, is not a surprise.
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
 * the same lines, the base stays where it is and each clash is shown — their
 * text beside the draft's — for the person to settle: keep theirs, keep yours,
 * or keep both. Nothing is saved until they press Save.
 *
 * # A link to a page is the address the backlinks read
 *
 * "Link a page" inserts `[title](#/knowledge/pages/{id})` — `PAGE_ADDRESS_PREFIX`
 * plus the id, which is exactly what `pages.Links` reads back, so a link made
 * here lists this page in the other one's "Linked from" once the index has
 * read the save. A title would break on the first rename; the id does not.
 */

import { useEffect, useId, useMemo, useRef, useState } from "react";
import { Button, Callout, ConfirmModal, FormField, Input, Textarea } from "@crewlethq/ui";
import { LinkGlyph } from "@crewlethq/icons/glyphs";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { plural } from "~/lib/format.ts";
import { Segmented } from "~/ui/primitives.tsx";
import type { PageDetail } from "~/protocol/index.ts";
import { merge3, mergedText, type Merge, type Resolution } from "~/lib/merge.ts";
import { BodyDiff } from "./History.tsx";

/** The markdown link to a page, by its id. */
export function pageLink(title: string, id: string): string {
  // A `]` in a title would close the label early; escaped, it renders as itself.
  return `[${title.replace(/([[\]\\])/g, "\\$1")}](${PAGE_ADDRESS_PREFIX}${id})`;
}

export function PageEditor({ detail, onDone }: { detail: PageDetail; onDone: () => void }) {
  const page = detail.page;
  const save = useAct("save_page");
  // WHAT THIS EDIT IS AGAINST — its version and that version's body — held
  // apart from the page the screen polls. See the header.
  const [base, setBase] = useState({ version: page.version, body: page.body ?? "" });
  const [draft, setDraft] = useState(page.body ?? "");
  const [message, setMessage] = useState("");
  const [lens, setLens] = useState<"write" | "preview">("write");
  const [theirs, setTheirs] = useState(false);
  const [discard, setDiscard] = useState(false);
  const [linking, setLinking] = useState(false);
  // A MERGE WAITING ON THE PERSON: their version, and the clashes to settle.
  const [merging, setMerging] = useState<{
    version: number;
    body: string;
    merge: Merge;
  } | null>(null);
  const [merged, setMerged] = useState<number | null>(null);
  const box = useRef<HTMLTextAreaElement>(null);
  const messageID = useId();

  const moved = page.version > base.version;
  const dirty = draft !== base.body;
  // WHY SAVE CANNOT BE PRESSED, one answer for the button and the shortcut
  // alike: a ⌘-Enter that skipped the "moved" clause sent a save the engine
  // could only refuse.
  const blocked = merging
    ? "Settle where both edits changed the same lines first."
    : !dirty
      ? "Nothing has changed yet."
      : moved
        ? `Revision ${page.version} was saved since you started — keep editing on top of it first.`
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

  // ESCAPE LEAVES, asking first when there is something to lose — the same
  // key every sheet in the product closes on.
  const leave = () => (dirty ? setDiscard(true) : onDone());

  const insert = (text: string) => {
    const el = box.current;
    const at = el ? el.selectionStart : draft.length;
    const end = el ? el.selectionEnd : draft.length;
    const next = draft.slice(0, at) + text + draft.slice(end);
    setDraft(next);
    requestAnimationFrame(() => {
      el?.focus();
      el?.setSelectionRange(at + text.length, at + text.length);
    });
  };

  return (
    <section className="kpage-editor" aria-label={`Editing “${page.title}”`}>
      {moved && !merging && (
        <Callout
          variant="warning"
          title={`Somebody saved revision ${page.version} while you edited.`}
        >
          Your edit started from revision {base.version}, so saving now is refused rather than
          overwriting theirs. Bring their change into your draft first.
          <span className="kpage-callout-actions">
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
        <div className="kpage-theirs">
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
        <p className="t-caption kpage-merged" role="status">
          Revision {merged}&rsquo;s change is in your draft — read it through before you save.
        </p>
      )}

      <div className="kpage-editor-bar">
        <Segmented
          ariaLabel="Write or preview"
          value={lens}
          onChange={(v) => setLens(v === "preview" ? "preview" : "write")}
          options={[
            { value: "write", label: "Write" },
            { value: "preview", label: "Preview" },
          ]}
        />
        <span className="spacer" />
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<LinkGlyph size="sm" />}
          onClick={() => setLinking(!linking)}
          aria-expanded={linking}
          disabled={lens !== "write"}
        >
          Link a page
        </Button>
      </div>
      {linking && lens === "write" && (
        <PagePicker
          exclude={page.id}
          onPick={(p) => {
            insert(pageLink(p.title, p.id));
            setLinking(false);
          }}
          onCancel={() => setLinking(false)}
        />
      )}
      {lens === "write" ? (
        <Textarea
          ref={box}
          className="kpage-editor-body"
          aria-label={`The body of “${page.title}”, in markdown`}
          value={draft}
          readOnly={merging !== null}
          rows={22}
          autoFocus
          spellCheck
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              leave();
            }
            // ⌘/Ctrl-Enter saves, through the same gate the button uses, so a
            // second one while the first answer is out sends nothing.
            if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
              event.preventDefault();
              if (pressable(save, blocked)) void press();
            }
          }}
        />
      ) : (
        <div className="prose md kpage-prose kpage-preview">
          {draft.trim() ? (
            renderMarkdown(draft)
          ) : (
            <span className="muted">Nothing written yet.</span>
          )}
        </div>
      )}

      <FormField
        label="What changed"
        htmlFor={messageID}
        helper="One line, kept with the revision — optional."
      >
        <Input
          id={messageID}
          value={message}
          maxLength={200}
          onChange={(event) => setMessage(event.target.value)}
        />
      </FormField>

      <div className="kpage-editor-foot">
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
        <span className="t-caption">Saved as you, against revision {base.version}.</span>
      </div>
      {/* A REFUSAL FOR RACING A NEWER REVISION is the callout above, which
          says what to do about it; the sentence would say it twice. */}
      {!moved && <RefusalNote write={save} />}

      {discard && (
        <ConfirmModal
          open
          destructive
          title="Discard your edit?"
          message="What you wrote here is not saved anywhere. The page stays as it is."
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
 * the draft's, and the person keeps one or both. Nothing is pre-chosen — a
 * default would be the browser deciding whose sentence survives — so Apply
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
    <section className="kpage-resolve" aria-labelledby={heading}>
      <h2 className="kpage-resolve-title" id={heading} ref={first} tabIndex={-1}>
        {clashes.length === 1
          ? "You and revision " + version + " changed the same lines"
          : `You and revision ${version} changed the same lines in ${clashes.length} places`}
      </h2>
      <p className="t-caption">
        Everything else from their change is already merged. For each place, keep their lines,
        yours, or both — your draft is held until you apply.
      </p>
      <ol className="kpage-resolve-list">
        {clashes.map((clash, i) => (
          <li key={i} className="kpage-resolve-item">
            <div className="kpage-resolve-sides">
              <figure className="kpage-resolve-side">
                <figcaption>Theirs · revision {version}</figcaption>
                <pre>{clash.theirs.join("\n") || "(removed)"}</pre>
              </figure>
              <figure className="kpage-resolve-side">
                <figcaption>Yours</figcaption>
                <pre>{clash.mine.join("\n") || "(removed)"}</pre>
              </figure>
            </div>
            <Segmented
              ariaLabel={`Place ${i + 1}: which lines to keep`}
              value={choices[i] ?? ""}
              onChange={(v) => setChoices(choices.map((c, k) => (k === i ? (v as Resolution) : c)))}
              options={RESOLUTIONS}
            />
          </li>
        ))}
      </ol>
      <div className="row gap-2 wrap">
        <Button
          size="small"
          variant="primary"
          disabled={!settled}
          onClick={() => onApply(mergedText(merge, choices as Resolution[]))}
        >
          Apply to my draft
        </Button>
        <Button size="small" variant="ghost" onClick={onCancel}>
          Not now
        </Button>
        {!settled && (
          <span className="t-caption">
            {plural(choices.filter((c) => c === undefined).length, "place")} still to settle.
          </span>
        )}
      </div>
    </section>
  );
}

/**
 * Choosing a page to link to: words from its title, and the first hits.
 * The page being edited is left out — a page linking to itself is not a link
 * anybody follows, and the backlinks do not count it.
 */
function PagePicker({
  exclude,
  onPick,
  onCancel,
}: {
  exclude: string;
  onPick: (page: { id: string; title: string }) => void;
  onCancel: () => void;
}) {
  const [q, setQ] = useState("");
  const inputID = useId();
  const found = useQuery("pages", { title: q.trim(), limit: 8 }, { enabled: q.trim().length >= 2 });
  const hits = useMemo(
    () => (found.data?.pages ?? []).filter((p) => p.id !== exclude),
    [found.data, exclude],
  );
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => field.current?.focus(), []);
  return (
    <div className="kpage-picker">
      <FormField label="Link to the page" htmlFor={inputID} helper="Words from its title.">
        <Input
          id={inputID}
          ref={field}
          value={q}
          onChange={(event) => setQ(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              event.stopPropagation();
              onCancel();
            }
          }}
        />
      </FormField>
      {q.trim().length < 2 ? (
        <p className="t-caption">Type two characters to search.</p>
      ) : found.loading && !found.data ? (
        <p className="t-caption">Searching…</p>
      ) : hits.length === 0 ? (
        <p className="t-caption">No page has that in its title.</p>
      ) : (
        <ul className="kpage-picker-hits" role="list">
          {hits.map((p) => (
            <li key={p.id}>
              <button type="button" className="kpage-picker-hit" onClick={() => onPick(p)}>
                <span className="kpage-link-title">{p.title}</span>
                <span className="kpage-link-key">{p.container}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
