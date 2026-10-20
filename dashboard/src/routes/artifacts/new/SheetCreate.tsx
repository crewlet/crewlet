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
 * NOTHING EXISTS UNTIL CREATE. The writes are the shared ones (`create.ts`):
 * the sheet, its schema, then its rows in keyed batches, and the page lands
 * on the new sheet. Cancel asks first when there is something to lose.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type KeyboardEvent } from "react";
import {
  ArtifactKindGlyph,
  Button,
  Callout,
  Checkbox,
  ChoiceList,
  FormField,
  Modal,
  Select,
  SheetColumnEditor,
  SheetColumnTypeGlyph,
  SheetGrid,
  parseCsv,
  type SheetColumn,
  type SheetColumnType,
} from "@crewlethq/ui";
import { PlusGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { COLLECTION_PRESETS } from "~/contract/artifacts.ts";
import { plural } from "~/lib/format.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { PRESET_WORDS, SHEET_TEMPLATES, optionID, type SheetTemplate } from "~/lib/artifacts/schema.ts";
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

/** A way in: a template, or a CSV the reader brings. */
type Start = SheetTemplate | "import";

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
  const [dropping, setDropping] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
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
    if (next !== "import" && !drafts[next]) {
      setDrafts((all) => ({ ...all, [next]: templateDraft(next, examples[next] ?? true) }));
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

  const onDrop = (event: DragEvent) => {
    if (!event.dataTransfer.types.includes("Files")) return;
    event.preventDefault();
    setDropping(false);
    const file = event.dataTransfer.files[0];
    if (file) void readFile(file);
  };

  const hold = !access.can
    ? access.reason
    : create.busy
      ? "Creating the sheet…"
      : !draft
        ? "Choose a CSV file to start from."
        : (titleHold(title) ?? draftProblem(draft));

  const make = () => {
    if (hold || !draft) return;
    void create.sheet(title, draftWrites(draft, preset));
  };

  // MOD+ENTER CREATES from anywhere in the page's body, as a composer's send
  // does from its field: read here, on the body, rather than bound at the
  // window, where the reply composer's row already answers the same press.
  const onBodyKey = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.defaultPrevented || event.key !== "Enter" || event.altKey || event.shiftKey) return;
    if (!(event.metaKey || event.ctrlKey)) return;
    event.preventDefault();
    make();
  };

  const dirty = touched || title.trim() !== "";
  const cancel = () => (dirty ? setLeaving(true) : leave());

  // WHAT THE PAGE IS DOING, and only that: the grid already shows its columns and rows.
  const status = create.progress
    ? `Writing rows: ${create.progress.done.toLocaleString()} of ${create.progress.total.toLocaleString()}`
    : create.busy
      ? "Creating…"
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
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<UploadGlyph size="sm" />}
            onClick={() => fileInput.current?.click()}
          >
            {imported ? "Choose another file" : "Choose a file"}
          </Button>
          <span className="sheet-create-import-hint">or drop it anywhere on this page</span>
          {imported && (
            <Checkbox
              size="sm"
              label="The first row names the columns"
              checked={imported.header}
              onChange={(event) => takeTable(imported.file, imported.table, event.target.checked)}
            />
          )}
        </div>
      ),
    },
  ];

  return (
    <CreateShell
      kind="sheet"
      status={status}
      onCancel={cancel}
      action={{
        label: create.busy ? "Creating…" : "Create sheet",
        icon: <PlusGlyph size="sm" />,
        onSelect: make,
        keys: ["Mod", "Enter"],
        disabledReason: hold,
      }}
    >
      <div
        ref={body}
        className="sheet-create"
        data-dropping={dropping || undefined}
        onDragOver={(event) => {
          if (!event.dataTransfer.types.includes("Files")) return;
          event.preventDefault();
          setDropping(true);
        }}
        onDragLeave={(event) => {
          if (!body.current?.contains(event.relatedTarget as Node | null)) setDropping(false);
        }}
        onDrop={onDrop}
        onKeyDown={onBodyKey}
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

          {draft ? (
            <>
              <SheetGrid
                className="sheet-create-grid"
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
                Press a column's name to set its type and choices. What you type becomes the
                sheet's first rows; empty rows are left out.
              </p>
            </>
          ) : (
            <button
              type="button"
              className="sheet-create-drop"
              onClick={() => fileInput.current?.click()}
            >
              <UploadGlyph size="md" />
              <span className="sheet-create-drop-title">Choose a CSV file, or drop one here</span>
              <span className="sheet-create-drop-detail">
                A comma, semicolon or tab separated file of up to{" "}
                {IMPORT_ROWS_MAX.toLocaleString()} rows. Nothing is sent until you create the
                sheet.
              </span>
            </button>
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
              Finer rules, such as who may write each column, are in the sheet's Data panel once it
              is made.
            </p>
          </section>
          {workItem && (
            <p className="sheet-create-note">It will be listed on {workItem}.</p>
          )}
        </aside>

        <input
          ref={fileInput}
          type="file"
          hidden
          accept=".csv,.tsv,.txt,text/csv,text/tab-separated-values,text/plain"
          onChange={(event) => {
            const file = event.target.files?.[0];
            event.target.value = "";
            if (file) void readFile(file);
          }}
        />
      </div>

      <Modal
        open={leaving}
        onClose={() => setLeaving(false)}
        title="Discard this sheet?"
        size="sm"
        placement="top"
        footer={
          <>
            <Button size="small" variant="ghost" onClick={() => setLeaving(false)}>
              Keep editing
            </Button>
            <Button size="small" variant="danger" onClick={leave}>
              Discard
            </Button>
          </>
        }
      >
        <p>Its title, columns and rows have not been saved, and leaving discards them.</p>
      </Modal>
    </CreateShell>
  );
}
