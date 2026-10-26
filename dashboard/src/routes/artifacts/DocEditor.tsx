/**
 * Editing a doc, as the signed-in person: its tabs' text, titles and order,
 * tabs added and removed, all one draft that Save publishes as one new version
 * based on the version the edit started from (D54, D55, D69).
 *
 * # What a save sends
 *
 * The least that says the change (`lib/artifacts/docDraft.ts`): an exact edit
 * of each changed tab and of the index, a removal for each tab dropped, and the
 * bytes of each tab added, uploaded first and streamed through the engine. A
 * new tab never lands on a file the version already holds.
 *
 * # What to do about somebody else's publish
 *
 * A save refused because somebody published since (`version_moved`) is merged
 * onto their version tab by tab: text by line, as the Doclet editor does,
 * titles and order as values. A merge with no overlap becomes the draft at
 * once, to be saved again; where both changed the same lines the person
 * settles each clash in `MergeConflicts`, and where both changed a title, the
 * order, or one removed a tab the other changed, they keep theirs or take the
 * newer one. Nothing is saved until they press Save.
 *
 * # Edited in place
 *
 * A tab's text is edited where it is read (`RichEditor`), so the doc keeps its
 * look while it is edited; the bar's Edit becomes the editing state, Cancel
 * and Save. The text stays Markdown, and a tab nobody edited is written back
 * byte for byte, so the version's patch holds only the tabs that changed.
 */

import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  ConfirmModal,
  ConflictNote,
  DocTabsEditor,
  Input,
  MergeConflicts,
  RichEditor,
  markdownWords,
  useToast,
} from "@crewlethq/ui";
import { CheckGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import type { ArtifactDetail, ArtifactDoc } from "~/protocol/index.ts";
import { newActOpID } from "~/protocol/index.ts";
import { ARTIFACT_LABEL_MAX_BYTES } from "~/contract/artifacts.ts";
import { plural, textBudget } from "~/lib/format.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { clashesOf } from "~/lib/merge.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useKindControls } from "~/lib/artifacts/kindControls.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { publishVersion, readDoc, uploadTexts, versionText } from "~/lib/artifacts/api.ts";
import {
  DOC_INDEX,
  addTab,
  addTabProblem,
  docChanged,
  docChangesOf,
  docDraftOf,
  docPublishPlan,
  mergeDoc,
  removeTab,
  renameTab,
  reorderTabs,
  resolveDoc,
  setTabText,
  tabTitleProblem,
  withFreePaths,
  type DocConflict,
  type DocDraft,
  type DocMerge,
} from "~/lib/artifacts/docDraft.ts";

/** A doc's edit as the assistant reads and changes it: the draft, and the version's label. */
interface DocEdit {
  readonly draft: DocDraft;
  readonly label: string;
}

/**
 * The fields of a doc's edit: each tab's title and text by its place in the
 * order, held to the tab's own rules, and the label of the version a save
 * makes, held to its bound.
 */
export function docEditFields(order: readonly string[]): DraftFieldSpec<DocEdit>[] {
  const tab = (
    i: number,
    key: "title" | "text",
  ): Pick<DraftFieldSpec<DocEdit>, "read" | "write"> => ({
    read: (e) => e.draft.tabs[order[i]!]?.[key] ?? "",
    write: (e, value) => {
      const at = order[i]!;
      if (typeof value !== "string") return refuse("A tab's title and text are text.");
      if (!e.draft.tabs[at]) return refuse("That tab is no longer in the doc.");
      if (key === "title") {
        const wrong = tabTitleProblem(value);
        return wrong ? refuse(wrong) : { ...e, draft: renameTab(e.draft, at, value) };
      }
      return { ...e, draft: setTabText(e.draft, at, value) };
    },
  });
  return [
    ...order.flatMap((_, i): DraftFieldSpec<DocEdit>[] => [
      { path: `tabs.${i + 1}.title`, label: `Tab ${i + 1} title`, ...tab(i, "title") },
      {
        path: `tabs.${i + 1}.text`,
        label: `Tab ${i + 1} text`,
        hint: "Markdown",
        ...tab(i, "text"),
        summary: (text) => plural(markdownWords(typeof text === "string" ? text : ""), "word"),
      },
    ]),
    {
      path: "label",
      label: "Version label",
      hint: "Optional, a few words",
      read: (e) => e.label,
      write: (e, value) => {
        if (typeof value !== "string") return refuse("A label is a line of text.");
        const budget = textBudget(value, ARTIFACT_LABEL_MAX_BYTES, "a version's label");
        return budget.over
          ? refuse(budget.line ?? "That label is too long.")
          : { ...e, label: value };
      },
    },
  ];
}

export function DocEditor({
  detail,
  version,
  doc,
  selected,
  onSelect,
  image,
  imageVersion,
  reading,
  onDraft,
  onDone,
  onPublished,
}: {
  detail: ArtifactDetail;
  version: number;
  doc: ArtifactDoc;
  /** The tab whose text is open, as the address names it ("" for the first). */
  selected: string;
  onSelect: (path: string) => void;
  /** Where the pictures of the tab at path are shown from. */
  image: (path: string) => (src: string) => string | null;
  /** Changes when another of the version's pictures has loaded. */
  imageVersion: unknown;
  /** The open tab as it reads, drawn while the editor's code loads. */
  reading: (path: string) => ReactNode;
  /** Whether a change is drafted, for the room. */
  onDraft: (drafting: boolean) => void;
  onDone: () => void;
  onPublished: (version: number) => void;
}) {
  const toast = useToast();
  const save = useKeyedGesture();
  const base = useMemo(() => docDraftOf(doc, version), [doc, version]);
  const [draft, setDraft] = useState<DocDraft>(base);
  const [merge, setMerge] = useState<DocMerge | null>(null);
  const [label, setLabel] = useState("");
  const [saving, setSaving] = useState(false);
  const [discard, setDiscard] = useState(false);
  // WHAT THE SAVE IS AGAINST: the version opened, or the newer one a refused
  // save was merged onto.
  const against = merge?.live ?? base;
  // EVERY FILE OF THAT VERSION, so a new tab never lands on one of them.
  const files = useQuery("artifact_version", { id: detail.id, n: against.base });
  const taken = useMemo(() => (files.data?.manifest.files ?? []).map((f) => f.path), [files.data]);

  const changes = docChangesOf(against, draft);
  const dirty = docChanged(changes);
  useEffect(() => onDraft(dirty), [dirty, onDraft]);

  const path = draft.tabs[selected] ? selected : draft.order[0]!;
  const tab = draft.tabs[path]!;
  const many = draft.order.length > 1;
  const conflicts = merge?.conflicts ?? [];
  const clash = conflicts.find((c) => c.kind === "text");
  const titleProblem = draft.order
    .map((p) => tabTitleProblem(draft.tabs[p]!.title))
    .find((problem) => problem !== null);
  const labelBudget = textBudget(label, ARTIFACT_LABEL_MAX_BYTES, "a version's label");
  const blocked =
    conflicts.length > 0
      ? "Settle where both edits differ first."
      : !dirty
        ? "Nothing has changed yet."
        : titleProblem
          ? titleProblem
          : changes.added.length > 0 && files.data === null
            ? files.error
              ? "The doc's files could not be read, so a new tab's file cannot be placed. Try again."
              : "Reading the doc's files."
            : labelBudget.over
              ? labelBudget.line
              : undefined;

  const settle = (next: DocMerge) => {
    setMerge(next);
    setDraft(next.draft);
  };

  const press = async () => {
    setSaving(true);
    try {
      const d = withFreePaths(against, draft, taken);
      if (d !== draft) setDraft(d);
      const c = docChangesOf(against, d);
      const indexChanges = c.index || c.added.length > 0 || c.removed.length > 0;
      const indexRaw =
        against.indexed && indexChanges
          ? await versionText(detail.id, against.base, DOC_INDEX)
          : null;
      const plan = docPublishPlan(against, d, indexRaw);
      const brought =
        plan.uploads.length > 0 ? await uploadTexts(detail.id, plan.uploads, newActOpID()) : null;
      const answer = await save.run(
        publishVersion(detail.id, against.base, {
          mode: "patch",
          edits: plan.edits,
          files: [
            ...(brought?.files ?? []),
            ...plan.removes.map((p) => ({ path: p, remove: true as const })),
          ],
          ...(brought ? { upload: brought.upload } : {}),
          ...(plan.entry ? { entry: plan.entry } : {}),
          ...(label.trim() ? { label: label.trim() } : {}),
        }),
      );
      if (answer?.kind === "done") {
        const n = Number(answer.body.number ?? against.base + 1);
        toast.ok(`Saved as version ${n}`);
        onPublished(n);
        return;
      }
      if (answer?.kind === "refused" && answer.error?.code === "version_moved") {
        // THE LIVE VERSION the refusal names, which the draft is merged onto
        // and the next save is based on.
        const live = answer.error.body.live as { number?: unknown } | undefined;
        const head = Number(live?.number);
        if (!Number.isInteger(head) || head < 1) {
          toast.show({ variant: "danger", title: "Could not save the doc", message: answer.text });
          return;
        }
        const newer = await readDoc(detail.id, head, new AbortController().signal);
        save.reset();
        settle(mergeDoc(against, d, docDraftOf(newer.doc, head)));
        return;
      }
      if (answer) {
        toast.show({ variant: "danger", title: "Could not save the doc", message: answer.text });
      }
    } catch (err) {
      toast.show({
        variant: "danger",
        title: "Could not save the doc",
        message: err instanceof Error ? err.message : "The engine did not answer.",
      });
    } finally {
      setSaving(false);
    }
  };

  const leave = () => (dirty ? setDiscard(true) : onDone());
  const busy = save.busy || saving;

  // WHAT IS OPEN, for the assistant: every tab's title and text and the
  // version's label, over the version the edit is based on, set through the
  // editor's own changes; Save stays the writer's. A whole new text loads
  // the tab's editor afresh, as it takes one only as a fresh document.
  const [loaded, setLoaded] = useState(0);
  useAssistantDraft<DocEdit>({
    kind: "client",
    id: detail.id,
    revision: against.base,
    step: `tabs.${draft.order.indexOf(path) + 1}`,
    help:
      "Each tab's text is Markdown and a change replaces all of it; a tab whose text here is " +
      "cut is longer than shown. Adding, removing and reordering tabs are the person's, and " +
      "saving makes a new version as them.",
    scope: `Your edit of “${detail.title}” (not saved)`,
    value: { draft, label },
    fields: docEditFields(draft.order),
    apply: (next) => {
      if (next.draft !== draft) {
        setDraft(next.draft);
        setLoaded((n) => n + 1);
      }
      if (next.label !== label) setLabel(next.label);
    },
    blocked: () =>
      busy
        ? "Saving the doc."
        : conflicts.length > 0
          ? "Settle where both edits differ first."
          : null,
  });

  // THE BAR WHILE EDITING (`lib/artifacts/kindControls.ts`): the state,
  // Cancel and Save, in Edit's place, so the control a writer pressed is
  // where they finish. Save keeps its word at every width; the reason it
  // cannot be pressed is the one the shortcut reads too.
  useKindControls({
    editing: { dirty, saving: busy },
    tools: [
      {
        key: "cancel",
        label: "Cancel",
        icon: <XGlyph size="sm" />,
        labelled: true,
        ...(busy ? { disabledReason: "Saving the doc." } : {}),
        onSelect: leave,
      },
      {
        key: "save",
        label: "Save",
        icon: <CheckGlyph size="sm" />,
        labelled: true,
        emphasis: "primary",
        ...(busy
          ? { disabledReason: "Saving the doc." }
          : blocked
            ? { disabledReason: blocked }
            : {}),
        onSelect: () => void press(),
      },
    ],
  });

  return (
    <>
      <div className="doc-read doc-read--editing">
        <div className="doc-read-side">
          <DocTabsEditor
            label={`Tabs of ${detail.title}`}
            items={draft.order.map((p) => ({ key: p, title: draft.tabs[p]!.title }))}
            selected={path}
            onSelect={onSelect}
            onRename={(p, title) => setDraft((d) => renameTab(d, p, title))}
            onReorder={(order) => setDraft((d) => reorderTabs(d, order))}
            onRemove={(p) => {
              setDraft((d) => removeTab(d, p));
              if (p === path) onSelect("");
            }}
            onAdd={(title) => {
              const added = addTab(draft, title, taken, path);
              setDraft(added.draft);
              onSelect(added.path);
            }}
            titleProblem={tabTitleProblem}
            addBlocked={addTabProblem(draft) ?? undefined}
            disabled={clash !== undefined || saving}
          />
        </div>
        <section
          className="doc-editor doc-read-page"
          aria-label={many ? `Editing “${tab.title}”` : `Editing “${detail.title}”`}
        >
          {merge && conflicts.length === 0 && (
            <Callout variant="info" title={`Version ${merge.live.base} is merged into your draft.`}>
              Somebody published while you were editing
              {merge.theirs.length > 0 ? ` (${merge.theirs.join(", ")})` : ""}. Their changes are
              kept beside yours. Save again to publish both.
            </Callout>
          )}
          {merge && clash?.kind === "text" && (
            <MergeConflicts
              key={clash.path}
              clashes={clashesOf(clash.merge)}
              theirs={`version ${merge.live.base}`}
              labels={{
                heading: (count, theirs) =>
                  count === 1
                    ? `You and ${theirs} changed the same lines of “${clash.title}”`
                    : `You and ${theirs} changed the same lines of “${clash.title}” in ${count} places`,
              }}
              onCancel={onDone}
              onApply={(choices) => settle(resolveDoc(merge, clash, choices))}
            />
          )}
          {merge && !clash && conflicts.length > 0 && (
            <div className="doc-conflicts">
              {conflicts.map((c) => (
                <ConflictNote
                  key={conflictKey(c)}
                  subject={conflictSubject(c)}
                  message={conflictSentence(c, merge.live.base)}
                  actions={
                    <>
                      <Button
                        size="small"
                        variant="secondary"
                        onClick={() => settle(resolveDoc(merge, c, "mine"))}
                      >
                        Keep mine
                      </Button>
                      <Button
                        size="small"
                        variant="ghost"
                        onClick={() => settle(resolveDoc(merge, c, "theirs"))}
                      >
                        Take theirs
                      </Button>
                    </>
                  }
                />
              ))}
            </div>
          )}

          {!clash && (
            <RichEditor
              key={`${path}:${loaded}`}
              value={tab.text}
              onChange={(text) => setDraft((d) => setTabText(d, path, text))}
              label={many ? `The text of “${tab.title}”` : `The text of “${detail.title}”`}
              placeholder="Write, or type a slash for blocks"
              disabled={saving}
              autoFocus
              resolveImage={image(path)}
              imageVersion={imageVersion}
              fallback={reading(path)}
              onEscape={leave}
              onSubmit={() => {
                if (!busy && !blocked) void press();
              }}
            />
          )}

          <div className="doc-editor-foot">
            <Input
              containerClassName="doc-editor-label"
              aria-label="Label for the version this save makes"
              placeholder="Label this version (optional)"
              value={label}
              error={labelBudget.over}
              onChange={(event) => setLabel(event.target.value)}
            />
            <span className="doc-note">
              {plural(markdownWords(tab.text), "word")} · Saved as you, as a new version based on
              version {against.base}
            </span>
          </div>
          {labelBudget.line && (
            <p className="doc-note" role={labelBudget.over ? "alert" : undefined}>
              {labelBudget.line}
            </p>
          )}
        </section>
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
    </>
  );
}

function conflictKey(c: DocConflict): string {
  return c.kind === "order" ? "order" : `${c.kind}:${c.path}`;
}

function conflictSubject(c: DocConflict): string {
  switch (c.kind) {
    case "order":
      return "The order of the tabs";
    case "title":
      return `The title of “${c.title}”`;
    case "tab":
    case "text":
      return `“${c.title}”`;
  }
}

function conflictSentence(c: DocConflict, version: number): string {
  switch (c.kind) {
    case "order":
      return `Version ${version} moved tabs too. Keep your order, or take theirs.`;
    case "title":
      return `Version ${version} renamed “${c.title}” too. Keep your title, or take theirs.`;
    case "tab":
      return `One of you removed “${c.title}” and the other changed it. Keep your side, or take version ${version}'s.`;
    case "text":
      return `Version ${version} changed the same lines of “${c.title}”.`;
  }
}
