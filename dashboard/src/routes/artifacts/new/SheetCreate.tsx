/**
 * A new sheet, shaped in the sheet: its own create page, in the shared frame
 * (`CreateShell`).
 *
 * THE PAGE IS THE SHEET. The body is the real grid (`SheetGrid`) drawing the
 * draft's columns, under the title, so what the reader sees is what Create
 * makes. A press on a column's name opens its settings in place (its name,
 * type, choices and whether it is required, `SheetColumnEditor`), Add column
 * appends one, and values typed into the grid become the sheet's first rows.
 * The rail beside it holds the ways in, each of which only changes what the
 * grid shows: Blank, a template (with example rows the reader keeps or
 * drops), or a CSV, read and typed in the browser (`sheetDraft.ts`). Each way
 * in keeps its own draft, so trying a template and coming back to Blank loses
 * nothing typed into either.
 *
 * OR AN AGENT SETS IT UP. The last way in trades the grid for a brief: what
 * the sheet should hold, and which agent builds it (`WhoBuildsIt`). Create
 * files a real task to that agent (`useFileArtifactTask`) and lands on it,
 * where the reader follows the work; the agent publishes the sheet.
 *
 * NOTHING EXISTS UNTIL CREATE. The writes are the shared ones (`create.ts`):
 * the sheet, its schema, then its rows in keyed batches, and the page lands
 * on the new sheet. Cancel asks first when there is something to lose.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArtifactKindGlyph,
  Button,
  Callout,
  Checkbox,
  ChoiceList,
  ConfirmModal,
  FileDrop,
  FilePickButton,
  FormField,
  Select,
  SheetColumnEditor,
  SheetColumnTypeGlyph,
  SheetGrid,
  Textarea,
  parseCsv,
  type SheetColumn,
  type SheetColumnType,
} from "@crewlethq/ui";
import { BotGlyph, PlusGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { COLLECTION_PRESETS } from "~/contract/artifacts.ts";
import { plural } from "~/lib/format.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import {
  PRESET_WORDS,
  SHEET_TEMPLATES,
  optionID,
  type SheetTemplate,
} from "~/lib/artifacts/schema.ts";
import {
  IMPORT_BYTES_MAX,
  IMPORT_ROWS_MAX,
  canAddColumn,
  columnNameProblem,
  draftColumns,
  draftFromTable,
  draftOf,
  draftProblem,
  draftWrites,
  emptyRows,
  titleFromFile,
  typeFromName,
  valueContext,
  withColumnMoved,
  withColumnSettings,
  withEdits,
  withNewColumn,
  withRowAdded,
  withoutColumn,
  withoutRow,
  type SheetDraft,
} from "~/lib/artifacts/sheetDraft.ts";
import { COLUMN_TYPE_WORDS } from "~/lib/artifacts/words.ts";
import type { CollectionPreset } from "~/protocol/index.ts";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import { titleHold, titleProblem, useCreateArtifact, useWorkItem } from "./create.ts";
import { useFileArtifactTask } from "./fileTask.ts";
import { WhoBuildsIt, useWhoBuildsIt } from "./WhoBuildsIt.tsx";

/** A way in: a template, a CSV the reader brings, or an agent that sets it up. */
type Start = SheetTemplate | "import" | "agent";

/** The files an import reads. */
const CSV_TYPES = ".csv,.tsv,.txt,text/csv,text/tab-separated-values,text/plain";

/** Briefs a reader can start from, one press each. */
const BRIEFS = [
  "A hiring pipeline: each candidate's role, stage, interviewer and next step.",
  "A vendor comparison: price, contract length, security review and a score for each.",
  "A content calendar: each post's channel, owner, publish date and status.",
] as const;

/** A template's draft, with its example rows or without. */
function templateDraft(key: SheetTemplate, examples: boolean): SheetDraft {
  const template = SHEET_TEMPLATES.find((t) => t.key === key) ?? SHEET_TEMPLATES[0];
  return draftOf(template.schema(), examples ? template.examples(new Date()) : []);
}

/** The policy a template starts with: its own collection's, "Editors change it" where it names none. */
function presetOf(draft: SheetDraft): CollectionPreset {
  return draft.collection.preset ?? "shared";
}

/** A CSV the reader brought: the file's name, the table read from it, and what reading it found. */
interface Imported {
  file: string;
  table: string[][];
  header: boolean;
  rows: number;
  unfit: number;
}

/** Why the first column stays: every row is named by it. */
const PRIMARY_KEPT = "Every row is named by this column, so a sheet keeps it.";

export function SheetCreate() {
  const create = useCreateArtifact();
  const workItem = useWorkItem();
  const leave = useLeaveCreate();
  const access = useArtifactAccess({ app: true });
  const people = useArtifactPeople().sheetPeople;
  const context = useMemo(() => valueContext(people), [people]);

  const [title, setTitle] = useState("");
  const [start, setStart] = useState<Start>("blank");
  const [drafts, setDrafts] = useState<Partial<Record<Start, SheetDraft>>>(() => ({
    blank: templateDraft("blank", false),
  }));
  const [examples, setExamples] = useState<Partial<Record<SheetTemplate, boolean>>>({});
  const [presets, setPresets] = useState<Partial<Record<Start, CollectionPreset>>>({});
  const [imported, setImported] = useState<Imported | null>(null);
  const [importProblem, setImportProblem] = useState<string | null>(null);
  const [settingsColumn, setSettingsColumn] = useState<string | null>(null);
  const [touched, setTouched] = useState(false);
  const [leaving, setLeaving] = useState(false);
  const [brief, setBrief] = useState("");
  const nav = useNavigator();
  const who = useWhoBuildsIt("sheet");
  const task = useFileArtifactTask();
  const agentName = who.builders.find((b) => b.seat.handle === who.seat)?.seat.name ?? "";
  const titleInput = useRef<HTMLInputElement>(null);
  const body = useRef<HTMLDivElement>(null);

  const draft = drafts[start];
  const preset = presets[start] ?? (draft ? presetOf(draft) : "shared");
  const columns = useMemo(() => (draft ? draftColumns(draft) : []), [draft]);

  useEffect(() => {
    titleInput.current?.focus({ preventScroll: true });
  }, []);

  /** Change the draft the reader is looking at. */
  const update = useCallback(
    (change: (now: SheetDraft) => SheetDraft) => {
      setTouched(true);
      setDrafts((all) => {
        const now = all[start];
        return now ? { ...all, [start]: change(now) } : all;
      });
    },
    [start],
  );

  const choose = (next: Start) => {
    setSettingsColumn(null);
    setStart(next);
    if (next !== "import" && next !== "agent" && !drafts[next]) {
      setDrafts((all) => ({ ...all, [next]: templateDraft(next, examples[next] ?? true) }));
    }
    // BELOW A LAPTOP'S WIDTH the rail stands under the grid, so what a choice
    // changes is above it, out of view: the page goes back up to show it.
    if (window.matchMedia?.("(width < 1024px)").matches) {
      const still = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      body.current?.scrollIntoView({ block: "start", behavior: still ? "auto" : "smooth" });
    }
  };

  const keepExamples = (key: SheetTemplate, keep: boolean) => {
    setExamples((all) => ({ ...all, [key]: keep }));
    update((now) => ({ ...now, rows: keep ? templateDraft(key, true).rows : emptyRows() }));
  };

  /** Read a table into the import draft, typed by its values. */
  const takeTable = (file: string, table: string[][], header: boolean) => {
    const read = draftFromTable(table, header, context);
    setImported({ file, table, header, rows: read.rows, unfit: read.unfit });
    setImportProblem(null);
    setDrafts((all) => ({ ...all, import: read.draft }));
    setSettingsColumn(null);
    setStart("import");
    setTouched(true);
    setTitle((now) => (now.trim() === "" ? titleFromFile(file) : now));
  };

  const readFile = async (file: File) => {
    setStart("import");
    if (file.size > IMPORT_BYTES_MAX) {
      setImportProblem(
        `${file.name} is ${(file.size / 1024 / 1024).toFixed(1)} MB and a file read here is at most ${IMPORT_BYTES_MAX / 1024 / 1024} MB. Ask an agent to load a larger table.`,
      );
      return;
    }
    let text: string;
    try {
      text = await file.text();
    } catch {
      setImportProblem(`${file.name} could not be read. Choose it again.`);
      return;
    }
    if (text.includes("\u0000")) {
      setImportProblem(`${file.name} is not a text file. Choose a CSV or a TSV file.`);
      return;
    }
    const parsed = parseCsv(text, { maxRows: IMPORT_ROWS_MAX + 1 });
    if (!parsed.ok) {
      setImportProblem(
        parsed.problem === "too_many_rows"
          ? `${file.name} has more than ${IMPORT_ROWS_MAX.toLocaleString()} rows. Ask an agent to load a larger table.`
          : `${file.name} has a quote that is never closed, starting on line ${parsed.line}.`,
      );
      return;
    }
    if (parsed.rows.every((row) => row.every((v) => v.trim() === ""))) {
      setImportProblem(`${file.name} is empty.`);
      return;
    }
    takeTable(file.name, parsed.rows, imported?.header ?? true);
  };

  /** The first file brought, dropped anywhere on the page or picked. */
  const takeFiles = (files: readonly { file: File }[]) => {
    const first = files[0];
    if (first) void readFile(first.file);
  };

  const asking = start === "agent";
  const hold = asking
    ? (task.held ??
      (task.busy
        ? "Filing the task…"
        : (titleHold(title) ??
          (brief.trim() === ""
            ? "Say what the sheet should hold."
            : who.seat === ""
              ? "Add an agent to the chart first, in Edit org."
              : undefined))))
    : !access.can
      ? access.reason
      : create.busy
        ? "Creating the sheet…"
        : !draft
          ? "Choose a CSV file to start from."
          : (titleHold(title) ?? draftProblem(draft));

  const make = async () => {
    if (hold) return;
    if (asking) {
      const filed = await task.fileArtifactTask({
        kind: "sheet",
        title: title.trim(),
        brief,
        seat: who.seat,
        project: who.project || undefined,
      });
      if (filed?.item) nav.to(["work", filed.item]);
      return;
    }
    if (draft) await create.sheet(title, draftWrites(draft, preset));
  };

  const dirty = touched || title.trim() !== "" || brief.trim() !== "";
  const cancel = () => (dirty ? setLeaving(true) : leave());

  // WHAT THE PAGE IS DOING, and only that: the grid already shows its columns and rows.
  const status = create.progress
    ? `Writing rows: ${create.progress.done.toLocaleString()} of ${create.progress.total.toLocaleString()}`
    : create.busy
      ? "Creating…"
      : task.busy
        ? "Filing the task…"
        : undefined;

  const columnEditor = (column: SheetColumn, close: () => void) => {
    if (!draft) return null;
    const all = draft.collection.columns ?? [];
    const at = all.findIndex((c) => c.key === column.field);
    const data = all[at];
    const primary = data?.primary === true;
    const provisional = draft.provisional.includes(column.field);
    const proposed = provisional ? typeFromName(column.label) : undefined;
    return (
      <SheetColumnEditor
        column={column}
        selectName
        onChange={(next) => update((now) => withColumnSettings(now, column.field, next, context))}
        onDone={close}
        // The column every row is named by holds words.
        types={primary ? ["text", "longtext"] : undefined}
        suggestedType={!primary && proposed ? (proposed as SheetColumnType) : undefined}
        nameError={columnNameProblem(draft, column.field, column.label)}
        optionId={optionID}
        onMove={(step) => update((now) => withColumnMoved(now, column.field, step))}
        canMoveBefore={!primary && at > 0 && !all[at - 1]?.primary}
        canMoveAfter={!primary && at < all.length - 1}
        onDelete={() => {
          close();
          update((now) => withoutColumn(now, column.field));
        }}
        deleteDisabledReason={primary ? PRIMARY_KEPT : undefined}
        labels={{ types: COLUMN_TYPE_WORDS }}
      />
    );
  };

  const startOptions = [
    ...SHEET_TEMPLATES.map((template) => {
      const preview = template.schema()[0]?.columns ?? [];
      return {
        value: template.key,
        label: (
          <span className="sheet-create-start">
            <span>{template.label}</span>
            <span className="sheet-create-start-columns" aria-hidden="true">
              {preview.slice(0, 6).map((c) => (
                <SheetColumnTypeGlyph key={c.key} type={c.type as SheetColumnType} size="xs" />
              ))}
            </span>
          </span>
        ),
        detail: template.description,
        ...(template.examples(new Date()).length > 0
          ? {
              panel: (
                <Checkbox
                  size="sm"
                  label="Start with the example rows"
                  checked={examples[template.key] ?? true}
                  onChange={(event) => keepExamples(template.key, event.target.checked)}
                />
              ),
            }
          : {}),
      };
    }),
    {
      value: "import",
      label: (
        <span className="sheet-create-start">
          <span>Import a CSV</span>
        </span>
      ),
      detail: "Columns are typed from their values. Check each one before you create.",
      separated: true,
      panel: (
        <div className="sheet-create-import">
          <FilePickButton
            size="small"
            variant="secondary"
            leadingIcon={<UploadGlyph size="sm" />}
            accept={CSV_TYPES}
            multiple={false}
            onFiles={takeFiles}
          >
            {imported ? "Choose another file" : "Choose a file"}
          </FilePickButton>
          <span className="sheet-create-import-hint">or drop it anywhere on this page</span>
          {imported && (
            <div className="sheet-create-import-header">
              <Checkbox
                size="sm"
                label="The first row names the columns"
                checked={imported.header}
                onChange={(event) => takeTable(imported.file, imported.table, event.target.checked)}
              />
            </div>
          )}
        </div>
      ),
    },
    {
      value: "agent",
      label: (
        <span className="sheet-create-start">
          <span>Ask an agent</span>
        </span>
      ),
      detail: "Describe the sheet. An agent sets up its columns and publishes it.",
    },
  ];

  return (
    <CreateShell
      kind="sheet"
      status={status}
      onCancel={cancel}
      action={{
        label: asking
          ? task.busy
            ? "Filing…"
            : agentName
              ? `Ask ${agentName}`
              : "Ask an agent"
          : create.busy
            ? "Creating…"
            : "Create sheet",
        icon: asking ? <BotGlyph size="sm" /> : <PlusGlyph size="sm" />,
        onSelect: () => void make(),
        disabledReason: hold,
      }}
    >
      <FileDrop
        ref={body}
        className="sheet-create"
        title="Drop the file to start from it"
        hint="A CSV or TSV file. Its columns are typed from their values."
        onFiles={takeFiles}
      >
        <div className="sheet-create-main">
          <div className="sheet-create-head">
            <ArtifactKindGlyph kind="sheet" size="lg" />
            <FormField className="sheet-create-title" error={titleProblem(title)}>
              {(field) => (
                <input
                  ref={titleInput}
                  id={field.id}
                  aria-describedby={field.describedBy}
                  aria-invalid={field.invalid || undefined}
                  aria-label="Sheet title"
                  className="sheet-create-title-input"
                  value={title}
                  placeholder="Untitled sheet"
                  autoComplete="off"
                  spellCheck
                  onChange={(event) => setTitle(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key !== "Enter" || event.metaKey || event.ctrlKey) return;
                    event.preventDefault();
                    body.current
                      ?.querySelector<HTMLElement>('[role="grid"] [tabindex="0"]')
                      ?.focus();
                  }}
                />
              )}
            </FormField>
          </div>

          {start === "import" && imported && draft && (
            <p className="sheet-create-source">
              From <strong>{imported.file}</strong>: {plural(imported.rows, "row")},{" "}
              {plural(columns.length, "column")}.{" "}
              {imported.unfit > 0
                ? `${plural(imported.unfit, "value")} did not fit its column's type and ${imported.unfit === 1 ? "was" : "were"} left empty. Change a column's type to keep them.`
                : "Every value fits its column."}
            </p>
          )}
          {importProblem && (
            <Callout variant="danger" title="That file was not imported">
              {importProblem}
            </Callout>
          )}
          {create.refusal && (
            <Callout variant="danger" title="The sheet was not created">
              {create.refusal}
            </Callout>
          )}
          {asking && task.refusal && (
            <Callout variant="danger" title="The task was not filed">
              {task.refusal.sentence}
              {task.refusal.hint ? ` ${task.refusal.hint}` : ""}
            </Callout>
          )}

          {asking ? (
            <div className="sheet-create-ask">
              <FormField
                label="What should the sheet hold?"
                helper="Name its columns, the choices they offer, and any rows to start with."
              >
                {(field) => (
                  <Textarea
                    id={field.id}
                    aria-describedby={field.describedBy}
                    autoResize
                    rows={5}
                    value={brief}
                    placeholder="A launch checklist: each gate's owner, due date and status, with the security review and pricing page as the first rows."
                    onChange={(event) => setBrief(event.target.value)}
                  />
                )}
              </FormField>
              <div className="sheet-create-briefs" role="group" aria-label="Start from an example">
                {BRIEFS.map((example) => (
                  <Button
                    key={example}
                    size="small"
                    variant="outline"
                    onClick={() => setBrief(example)}
                  >
                    {example.slice(0, example.indexOf(":"))}
                  </Button>
                ))}
              </div>
              <WhoBuildsIt who={who} />
              <p className="sheet-create-next">
                {agentName
                  ? `${agentName} gets a task to set up “${title.trim() || "the sheet"}”. They make its columns and rows from your brief and publish it to Artifacts. You land on the task, where you can follow the work and answer their questions.`
                  : "The agent you pick gets a task to set up the sheet from your brief and publish it to Artifacts. You land on the task, where you can follow the work."}
              </p>
            </div>
          ) : draft ? (
            <>
              <SheetGrid
                label={title.trim() || "Untitled sheet"}
                columns={columns}
                rows={draft.rows}
                people={people}
                fit="content"
                onEdit={(edits) => update((now) => withEdits(now, edits))}
                onAddRow={() => update(withRowAdded)}
                onDeleteRow={(id) => update((now) => withoutRow(now, id))}
                renderColumnEditor={columnEditor}
                settingsColumn={settingsColumn}
                onSettingsColumnChange={setSettingsColumn}
                onAddColumn={
                  canAddColumn(draft)
                    ? () => {
                        // Drawn and opened in one render, so its settings open on its header.
                        const added = withNewColumn(draft);
                        update(() => added.draft);
                        setSettingsColumn(added.key);
                      }
                    : undefined
                }
                labels={{ types: COLUMN_TYPE_WORDS }}
              />
              <p className="sheet-create-hint">
                Press a column's name to set its type and choices. What you type becomes the sheet's
                first rows; empty rows are left out.
              </p>
            </>
          ) : (
            <FileDrop
              className="sheet-create-drop"
              title="Choose a CSV file, or drop one here"
              hint={`A comma, semicolon or tab separated file of up to ${IMPORT_ROWS_MAX.toLocaleString()} rows. Nothing is sent until you create the sheet.`}
              onFiles={takeFiles}
              actions={
                <FilePickButton
                  size="small"
                  variant="secondary"
                  leadingIcon={<UploadGlyph size="sm" />}
                  accept={CSV_TYPES}
                  multiple={false}
                  onFiles={takeFiles}
                >
                  Choose a file
                </FilePickButton>
              }
            />
          )}
        </div>

        <aside className="sheet-create-rail" aria-label="Sheet setup">
          <section className="sheet-create-section">
            <FormField as="fieldset" label="Start from">
              <ChoiceList
                label="Start from"
                value={start}
                onChange={(value) => choose(value as Start)}
                options={startOptions}
              />
            </FormField>
          </section>
          {!asking && (
            <section className="sheet-create-section">
              <FormField label="Who can change rows" helper={PRESET_WORDS[preset].description}>
                {(field) => (
                  <Select
                    id={field.id}
                    aria-describedby={field.describedBy}
                    size="sm"
                    width="full"
                    value={preset}
                    onChange={(value) => {
                      setTouched(true);
                      setPresets((all) => ({ ...all, [start]: value as CollectionPreset }));
                    }}
                    options={COLLECTION_PRESETS.map((p) => ({
                      value: p,
                      label: PRESET_WORDS[p].label,
                    }))}
                  />
                )}
              </FormField>
              <p className="sheet-create-note">
                Finer rules, such as who may write each column, are in the sheet's Data panel once
                it is made.
              </p>
            </section>
          )}
          {workItem && !asking && (
            <p className="sheet-create-note">It will be listed on {workItem}.</p>
          )}
        </aside>
      </FileDrop>

      <ConfirmModal
        open={leaving}
        destructive
        title="Discard this sheet?"
        message={
          asking
            ? "Its title and brief have not been sent, and leaving discards them."
            : "Its title, columns and rows have not been saved, and leaving discards them."
        }
        cancelLabel="Keep editing"
        confirmLabel="Discard"
        onClose={() => setLeaving(false)}
        onConfirm={leave}
      />
    </CreateShell>
  );
}
