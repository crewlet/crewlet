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
 * OR AN AGENT SETS IT UP. "Ask an agent" in the bar opens the hand-off panel
 * beside the grid (`AskAgent.tsx`): what the sheet should hold, and which
 * agent builds it. The grid stays as it is, and the columns and rows shaped
 * in it go with the brief (`draftBrief`) once the reader has shaped any, so
 * the agent starts from them. The send files a real task to that agent
 * (`useFileArtifactTask`) and lands on it, where the reader follows the work;
 * the agent publishes the sheet.
 *
 * NOTHING EXISTS UNTIL CREATE. The writes are the shared ones (`create.ts`):
 * the sheet, its schema, then its rows in keyed batches, and the page lands
 * on the new sheet. Cancel asks first when there is something to lose.
 */

import { useCallback, useMemo, useRef, useState } from "react";
import {
  Callout,
  Checkbox,
  ChoiceList,
  ConfirmModal,
  DocumentTitleField,
  FileDrop,
  FilePickButton,
  FormField,
  Select,
  SheetColumnEditor,
  SheetColumnTypeGlyph,
  SheetGrid,
  parseCsv,
  type SheetColumn,
  type SheetColumnType,
} from "@crewlethq/ui";
import { Columns3Glyph, FileTextGlyph, PlusGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { COLLECTION_PRESETS } from "~/contract/artifacts.ts";
import { TASK_BODY_MAX_BYTES } from "~/contract/work.ts";
import { fmtBytes, plural, utf8Bytes } from "~/lib/format.ts";
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
  draftBrief,
  draftColumns,
  draftFromTable,
  isBlankRow,
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
import { artifactTaskBody, useFileArtifactTask } from "./fileTask.ts";
import { useWhoBuildsIt } from "./WhoBuildsIt.tsx";
import { createDraft } from "./assistantDraft.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import {
  AskAgentPanel,
  askTitleHold,
  askTool,
  chosenAgent,
  taskRefusal,
  useAskPanel,
} from "./AskAgent.tsx";

/** A way in: a template, or a CSV the reader brings. */
type Start = SheetTemplate | "import";

/** The files an import reads. */
const CSV_TYPES = ".csv,.tsv,.txt,text/csv,text/tab-separated-values,text/plain";

/** Briefs a reader can start from, one press each. */
const BRIEFS = [
  {
    key: "hiring",
    label: "Hiring pipeline",
    brief: "A hiring pipeline: each candidate's role, stage, interviewer and next step.",
  },
  {
    key: "vendors",
    label: "Vendor comparison",
    brief: "A vendor comparison: price, contract length, security review and a score for each.",
  },
  {
    key: "content",
    label: "Content calendar",
    brief: "A content calendar: each post's channel, owner, publish date and status.",
  },
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

/** What a new sheet holds beyond what every create page does, as the assistant reads it. */
interface SheetSummary {
  readonly start: Start;
  readonly columns: readonly string[];
  readonly rows: number;
}

/** The template keys a new sheet may start from; a CSV is the reader's to bring. */
const TEMPLATE_KEYS: readonly string[] = SHEET_TEMPLATES.map((t) => t.key);

/**
 * A new sheet's own fields: the way in, settable to a template as a press
 * of its card would, and the columns and the filled rows, read only.
 */
export const SHEET_FIELDS: readonly DraftFieldSpec<SheetSummary>[] = [
  {
    path: "start",
    label: "Starts from",
    hint: `One of ${TEMPLATE_KEYS.join(", ")}; import is a CSV the person brings`,
    read: (d) => d.start,
    write: (d, key) =>
      typeof key === "string" && TEMPLATE_KEYS.includes(key.trim())
        ? { ...d, start: key.trim() as SheetTemplate }
        : refuse(`Choose one of the templates: ${TEMPLATE_KEYS.join(", ")}.`),
  },
  { path: "columns", label: "Columns", read: (d) => d.columns },
  { path: "rows", label: "Rows filled in", read: (d) => String(d.rows) },
];

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
  const panel = useAskPanel();
  const agentName = chosenAgent(who)?.seat.name ?? "";
  const body = useRef<HTMLDivElement>(null);

  const draft = drafts[start];
  const preset = presets[start] ?? (draft ? presetOf(draft) : "shared");
  const columns = useMemo(() => (draft ? draftColumns(draft) : []), [draft]);

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
        `${file.name} is ${fmtBytes(file.size)} and a file read here is at most ${fmtBytes(IMPORT_BYTES_MAX)}. Ask an agent to load a larger table.`,
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

  const hold = !access.can
    ? access.reason
    : create.busy
      ? "Creating the sheet…"
      : !draft
        ? "Choose a CSV file to start from."
        : (titleHold(title) ?? draftProblem(draft));

  // THE GRID GOES WITH THE BRIEF once the reader has shaped it: a template, a
  // file, or a blank sheet they changed. An untouched blank sheet says nothing
  // the brief does not.
  const shaped = draft !== undefined && (start !== "blank" || touched);
  const handed = shaped ? `${brief.trim()}\n\n${draftBrief(draft, COLUMN_TYPE_WORDS)}` : brief;
  const taskText = artifactTaskBody("sheet", handed);
  const taskBytes = utf8Bytes(taskText);
  const askHold =
    task.held ??
    (brief.trim() === ""
      ? "Say what the sheet should hold."
      : (askTitleHold(title, "sheet") ??
        (who.seat === ""
          ? "Add an agent to the chart first."
          : taskBytes > TASK_BODY_MAX_BYTES
            ? `The brief and the sheet are ${taskBytes.toLocaleString()} bytes, and a task holds at most ${TASK_BODY_MAX_BYTES.toLocaleString()}. Remove some rows.`
            : undefined)));

  const make = async () => {
    if (hold || !draft) return;
    await create.sheet(title, draftWrites(draft, preset));
  };

  const ask = async () => {
    if (askHold || task.busy) return;
    const filed = await task.fileArtifactTask({
      kind: "sheet",
      title: title.trim(),
      brief: handed,
      seat: who.seat,
      project: who.project || undefined,
    });
    if (filed?.item) nav.to(["worklet", filed.item]);
  };

  const filledRows = draft ? draft.rows.filter((row) => !isBlankRow(row)).length : 0;

  const dirty = touched || title.trim() !== "" || brief.trim() !== "";
  const cancel = () => (dirty ? setLeaving(true) : leave());

  // WHAT IS OPEN, for the assistant: the title, the way in, the columns and
  // how many rows are filled, and the hand-off beside them. The way in is set
  // as a press of its card sets it; the grid is the reader's to shape.
  useAssistantDraft(
    createDraft<SheetSummary>({
      what: "sheet",
      help:
        "You can set the title and the template it starts from. Columns and rows are " +
        "shaped by the person in the grid, and a CSV is brought by them.",
      step: panel.open ? "ask" : start,
      title,
      setTitle,
      brief,
      setBrief,
      who,
      extra: {
        value: {
          start,
          columns: (draft?.collection.columns ?? []).map(
            (c) => `${c.label ?? c.key} (${COLUMN_TYPE_WORDS[c.type]})`,
          ),
          rows: filledRows,
        },
        fields: SHEET_FIELDS,
        apply: (next) => {
          if (next.start !== start) choose(next.start);
        },
      },
      blocked: () => (create.busy ? "The sheet is being created." : null),
    }),
  );

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
  ];

  return (
    <CreateShell
      kind="sheet"
      status={status}
      onCancel={cancel}
      action={{
        label: create.busy ? "Creating…" : "Create sheet",
        icon: <PlusGlyph size="sm" />,
        onSelect: () => void make(),
        disabledReason: hold,
      }}
      handoff={{
        tool: askTool(who, panel),
        open: () => panel.setOpen(true),
        panel: panel.open ? (
          <AskAgentPanel
            panel={panel}
            who={who}
            intro={`${agentName || "The agent"} gets a task to set up the sheet from your brief, makes its columns and rows, and publishes it to Artifacts.`}
            brief={brief}
            onBrief={setBrief}
            briefLabel="What should the sheet hold?"
            briefPlaceholder="A launch checklist: each gate's owner, due date and status."
            briefHelper="Name its columns, the choices they offer, and any rows to start with."
            starters={BRIEFS}
            carried={[
              ...(title.trim()
                ? [
                    {
                      key: "title",
                      icon: <FileTextGlyph size="sm" />,
                      label: `“${title.trim()}”`,
                      detail: "The sheet's title",
                    },
                  ]
                : []),
              ...(shaped
                ? [
                    {
                      key: "columns",
                      icon: <Columns3Glyph size="sm" />,
                      label:
                        filledRows > 0
                          ? `${plural(columns.length, "column")} and ${plural(filledRows, "row")}`
                          : plural(columns.length, "column"),
                      detail: columns.map((c) => c.label).join(", "),
                    },
                  ]
                : []),
            ]}
            alone="Shape columns in the grid, start from a template or bring a CSV, and they go with the brief."
            task={{ title: title.trim() || "Untitled sheet", body: taskText }}
            steps={[
              `A task is filed to ${agentName || "the agent"}${who.project ? ` in ${who.project}` : ""}, with your brief${shaped ? " and the columns" : ""}.`,
              `${agentName || "The agent"} sets up the columns and rows and publishes the sheet to Artifacts.`,
              "You land on the task, where you follow the work and answer their questions.",
            ]}
            notice={taskRefusal(task.refusal)}
            sendLabel={(n) => `Ask ${n}`}
            onSend={() => void ask()}
            hold={askHold}
            busy={task.busy}
          />
        ) : null,
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
          <FormField error={titleProblem(title)}>
            {(field) => (
              <DocumentTitleField
                autoFocus
                id={field.id}
                aria-describedby={field.describedBy}
                aria-label="Sheet title"
                error={field.invalid}
                value={title}
                placeholder="Untitled sheet"
                autoComplete="off"
                spellCheck
                onChange={(event) => setTitle(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key !== "Enter" || event.metaKey || event.ctrlKey) return;
                  event.preventDefault();
                  body.current?.querySelector<HTMLElement>('[role="grid"] [tabindex="0"]')?.focus();
                }}
              />
            )}
          </FormField>

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
          {
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
          }
          {workItem && <p className="sheet-create-note">It will be listed on {workItem}.</p>}
        </aside>
      </FileDrop>

      <ConfirmModal
        open={leaving}
        destructive
        title="Discard this sheet?"
        message={
          brief.trim()
            ? "Its title, columns, rows and brief are not saved anywhere yet."
            : "Its title, columns and rows are not saved anywhere yet."
        }
        cancelLabel="Keep editing"
        confirmLabel="Discard"
        onClose={() => setLeaving(false)}
        onConfirm={leave}
      />
    </CreateShell>
  );
}
