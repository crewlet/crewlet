/**
 * The schema editor: a datastore's collections, their policy and their
 * columns, changed as a draft and saved as one schema change.
 *
 * CONFIRMED, NOT OPTIMISTIC. The schema is a structural change every reader of
 * the sheet sees, so nothing moves until the engine has applied it: the draft
 * is the editor's own until Save, and a refusal (a column that would rewrite
 * more rows than one change may, a rule an agent may not loosen) is said in
 * the engine's words beside the button.
 *
 * WHAT IS CHECKED HERE is what the reader can be told at the field: a new
 * collection's name and a new column's key against the engine's grammar, a
 * choice column with no choices, the bounds on how many there are. An option's
 * id is made from its label when it is added and never changes after, so
 * relabelling an option never rewrites a row that chose it.
 */

import { useEffect, useMemo, useState } from "react";
import {
  Button,
  Checkbox,
  EmptyState,
  FormField,
  HuePicker,
  IconButton,
  Input,
  isNodeHue,
  List,
  ListItem,
  Select,
  StatusDot,
  useToast,
  type Tone,
} from "@crewlethq/ui";
import { PlusGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { COLLECTION_PRESETS, COLUMN_TYPES, STATUS_CATEGORIES } from "~/contract/artifacts.ts";
import type {
  ArtifactDetail,
  ArtifactRole,
  CollectionPreset,
  ColumnType,
  DataCollection,
  DataColumn,
  DataOption,
} from "~/protocol/index.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { configureSchema } from "~/lib/artifacts/api.ts";
import {
  PRESET_WORDS,
  collectionNameProblem,
  fieldNameProblem,
  hasOptions,
  keyFromLabel,
  optionID,
  schemaChangeCount,
  schemaProblem,
  withPreset,
} from "~/lib/artifacts/schema.ts";
import { COLUMN_TYPE_WORDS, ROLE_WORDS, roleIncludes } from "~/lib/artifacts/words.ts";
import { plural } from "~/lib/format.ts";

/** What each status category is called. */
const CATEGORY_WORDS: Readonly<Record<(typeof STATUS_CATEGORIES)[number], string>> = {
  todo: "To do",
  in_progress: "In progress",
  done: "Done",
  blocked: "Blocked",
};

/** The colour a choice with no hue is stored as. */
const NO_HUE = "neutral";

/** The tones a status may wear, and what each is called. */
const STATUS_TONES: Readonly<
  Record<Extract<Tone, "neutral" | "info" | "success" | "warning" | "danger">, string>
> = {
  neutral: "Neutral",
  info: "Info",
  success: "Success",
  warning: "Warning",
  danger: "Danger",
};

/**
 * The fields of a schema's edit, by each collection's place: its title, who
 * changes it (one of the presets its own control offers, applied as that
 * control applies it), and its columns, read only.
 */
export function schemaFields(count: number): DraftFieldSpec<DataCollection[]>[] {
  const at = (
    all: readonly DataCollection[],
    i: number,
    change: (c: DataCollection) => DataCollection,
  ) => all.map((c, j) => (j === i ? change(c) : c));
  return Array.from({ length: count }, (_, i): DraftFieldSpec<DataCollection[]>[] => [
    {
      path: `collections.${i + 1}.title`,
      label: `Collection ${i + 1} title`,
      read: (all) => all[i]?.title ?? "",
      write: (all, title) =>
        !all[i]
          ? refuse("That collection is no longer in the schema.")
          : typeof title !== "string"
            ? refuse("A collection's title is a line of text.")
            : at(all, i, (c) => ({ ...c, title })),
    },
    {
      path: `collections.${i + 1}.preset`,
      label: `Collection ${i + 1}: who changes it`,
      hint: `One of ${COLLECTION_PRESETS.join(", ")}`,
      read: (all) => all[i]?.preset ?? "",
      write: (all, preset) =>
        !all[i]
          ? refuse("That collection is no longer in the schema.")
          : typeof preset === "string" &&
              (COLLECTION_PRESETS as readonly string[]).includes(preset.trim())
            ? at(all, i, (c) => withPreset(c, preset.trim() as CollectionPreset))
            : refuse(`Choose one of ${COLLECTION_PRESETS.join(", ")}.`),
    },
    {
      path: `collections.${i + 1}.columns`,
      label: `Collection ${i + 1} columns`,
      read: (all) =>
        (all[i]?.columns ?? []).map((c) => `${c.label ?? c.key} (${COLUMN_TYPE_WORDS[c.type]})`),
    },
  ]).flat();
}

export function SchemaEditor({
  detail,
  collections,
  role,
  onSaved,
}: {
  detail: ArtifactDetail;
  collections: DataCollection[];
  role: ArtifactRole;
  onSaved: () => void;
}) {
  const [draft, setDraft] = useState<DataCollection[]>(collections);
  const [picked, setPicked] = useState(collections[0]?.name ?? "");
  const [fresh, setFresh] = useState<Set<string>>(new Set());
  const [newName, setNewName] = useState("");
  const gesture = useKeyedGesture();
  const toast = useToast();
  useEffect(() => {
    setDraft(collections);
    setFresh(new Set());
  }, [collections]);
  const canEdit = roleIncludes(role, "editor");
  const changes = schemaChangeCount(collections, draft);
  const changed = changes > 0;
  const problem = schemaProblem(draft);
  const current = draft.find((c) => c.name === picked) ?? draft[0];

  // WHAT IS OPEN, for the assistant, to an editor of the datastore: each
  // collection's title, who changes it, and its columns, set through the
  // editor's own draft; Save, the one schema change, stays the person's.
  useAssistantDraft<DataCollection[]>(
    canEdit
      ? {
          kind: "client",
          id: detail.id,
          step: current ? `collections.${draft.indexOf(current) + 1}` : null,
          help:
            "You can set each collection's title and who changes it. Columns are read here: " +
            "adding, retyping and removing them, and adding collections, are the person's. " +
            "Saving applies the change for every reader of the sheet.",
          scope: `The schema of “${detail.title}” (not saved)`,
          value: draft,
          fields: schemaFields(draft.length),
          apply: setDraft,
          blocked: () => (gesture.busy ? "The schema is being saved." : null),
        }
      : null,
  );

  const update = (name: string, change: (c: DataCollection) => DataCollection) =>
    setDraft((was) => was.map((c) => (c.name === name ? change(c) : c)));
  const addCollection = () => {
    const name = newName.trim();
    if (
      collectionNameProblem(
        name,
        draft.map((c) => c.name),
      )
    )
      return;
    setDraft((was) => [
      ...was,
      { name, preset: "shared", read: "all", create: "editor", delete: "editor", columns: [] },
    ]);
    setFresh((was) => new Set([...was, `c:${name}`]));
    setPicked(name);
    setNewName("");
  };
  const save = async () => {
    const answer = await gesture.run(configureSchema(detail.id, draft));
    if (answer?.kind === "done") {
      toast.ok("Schema saved");
      onSaved();
    }
  };
  const nameProblem = newName.trim()
    ? collectionNameProblem(
        newName.trim(),
        draft.map((c) => c.name),
      )
    : null;

  return (
    <div className="artifact-schema">
      <div className="artifact-schema-collections">
        <List aria-label="Collections">
          {draft.map((c) => (
            <ListItem
              key={c.name}
              selected={c.name === current?.name}
              onClick={() => setPicked(c.name)}
              trailing={
                <span className="artifact-note">{plural(c.columns?.length ?? 0, "column")}</span>
              }
            >
              {c.title || c.name}
            </ListItem>
          ))}
        </List>
        {canEdit && (
          <FormField
            label="New collection"
            error={nameProblem ?? undefined}
            helper="Lowercase letters, digits and underscores."
          >
            {(field) => (
              <span className="artifact-inline">
                <Input
                  id={field.id}
                  aria-describedby={field.describedBy}
                  aria-invalid={field.invalid}
                  inputSize="sm"
                  width="sm"
                  value={newName}
                  placeholder="reviews"
                  onChange={(e) => setNewName(e.target.value.toLowerCase())}
                />
                <Button
                  size="small"
                  variant="secondary"
                  leadingIcon={<PlusGlyph size="sm" />}
                  disabledReason={newName.trim() && !nameProblem ? undefined : "Name it first."}
                  onClick={addCollection}
                >
                  Add
                </Button>
              </span>
            )}
          </FormField>
        )}
      </div>
      {!current && (
        <EmptyState
          size="compact"
          title="No collections yet"
          description="A collection is one table of rows with its own columns. Name one to start adding its columns."
        />
      )}
      {current && (
        <CollectionForm
          collection={current}
          fresh={fresh}
          canEdit={canEdit}
          onChange={(change) => update(current.name, change)}
          onFresh={(key) => setFresh((was) => new Set([...was, `f:${current.name}:${key}`]))}
          onRemove={
            fresh.has(`c:${current.name}`)
              ? () => {
                  setDraft((was) => was.filter((c) => c.name !== current.name));
                  setPicked(draft[0]?.name ?? "");
                }
              : undefined
          }
        />
      )}
      {/* THE ACTIONS STAY IN REACH. The form runs to many screens on a sheet
          with choice columns, so Save and how much it will apply stick to the
          foot of the view rather than waiting at the end of it. */}
      <div className="artifact-form-actions artifact-schema-foot">
        {changed && (
          <span className="artifact-schema-pending" role="status">
            {`${plural(changes, "unsaved change")}`}
          </span>
        )}
        {problem && <p className="artifact-refusal">{problem}</p>}
        {gesture.answer && gesture.answer.kind !== "done" && (
          <p className="artifact-refusal">{gesture.answer.text}</p>
        )}
        <Button
          size="small"
          variant="ghost"
          disabledReason={changed ? undefined : "Nothing has changed."}
          onClick={() => setDraft(collections)}
        >
          Discard changes
        </Button>
        <Button
          size="small"
          variant="primary"
          loading={gesture.busy}
          disabledReason={
            !canEdit
              ? "Changing the schema needs the “Can edit” role."
              : !changed
                ? "Nothing has changed."
                : (problem ?? undefined)
          }
          onClick={() => void save()}
        >
          Save schema
        </Button>
      </div>
    </div>
  );
}

/** One collection's policy and columns. */
function CollectionForm({
  collection,
  fresh,
  canEdit,
  onChange,
  onFresh,
  onRemove,
}: {
  collection: DataCollection;
  fresh: ReadonlySet<string>;
  canEdit: boolean;
  onChange: (change: (c: DataCollection) => DataCollection) => void;
  onFresh: (key: string) => void;
  onRemove: (() => void) | undefined;
}) {
  const [label, setLabel] = useState("");
  const columns = useMemo(() => collection.columns ?? [], [collection.columns]);
  const key = keyFromLabel(label);
  const keyProblem = label.trim()
    ? fieldNameProblem(
        key,
        columns.map((c) => c.key),
      )
    : null;
  const setColumn = (k: string, change: (c: DataColumn) => DataColumn) =>
    onChange((c) => ({
      ...c,
      columns: (c.columns ?? []).map((col) => (col.key === k ? change(col) : col)),
    }));
  return (
    <div className="artifact-schema-form">
      <div className="artifact-form">
        <FormField label="Title" optional>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              value={collection.title ?? ""}
              disabled={!canEdit}
              onChange={(e) => onChange((c) => ({ ...c, title: e.target.value }))}
            />
          )}
        </FormField>
        <FormField
          label="Who changes it"
          helper={collection.preset ? PRESET_WORDS[collection.preset].description : undefined}
        >
          {(field) => (
            <Select
              id={field.id}
              value={collection.preset ?? ""}
              disabled={!canEdit}
              onChange={(value) =>
                onChange((c) => withPreset(c, String(value) as CollectionPreset))
              }
              options={COLLECTION_PRESETS.map((p) => ({ value: p, label: PRESET_WORDS[p].label }))}
              ariaLabel="Who changes it"
            />
          )}
        </FormField>
        {onRemove && (
          <Button
            size="small"
            variant="ghost"
            leadingIcon={<TrashGlyph size="sm" />}
            onClick={onRemove}
          >
            Remove this collection
          </Button>
        )}
      </div>
      <ul className="artifact-columns" role="list">
        {columns.map((col) => (
          <ColumnRow
            key={col.key}
            column={col}
            canEdit={canEdit}
            removable={fresh.has(`f:${collection.name}:${col.key}`)}
            onChange={(change) => setColumn(col.key, change)}
            onRemove={() =>
              onChange((c) => ({
                ...c,
                columns: (c.columns ?? []).filter((x) => x.key !== col.key),
              }))
            }
          />
        ))}
      </ul>
      {canEdit && (
        <FormField
          label="New column"
          error={keyProblem ?? undefined}
          helper={label.trim() ? `Stored as ${key}.` : undefined}
        >
          {(field) => (
            <span className="artifact-inline">
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid}
                inputSize="sm"
                width="sm"
                value={label}
                placeholder="Column name"
                onChange={(e) => setLabel(e.target.value)}
              />
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<PlusGlyph size="sm" />}
                disabledReason={label.trim() && !keyProblem ? undefined : "Name it first."}
                onClick={() => {
                  onChange((c) => ({
                    ...c,
                    columns: [...(c.columns ?? []), { key, type: "text", label: label.trim() }],
                  }));
                  onFresh(key);
                  setLabel("");
                }}
              >
                Add column
              </Button>
            </span>
          )}
        </FormField>
      )}
    </div>
  );
}

/** One column: its name, type, rules and choices. */
function ColumnRow({
  column,
  canEdit,
  removable,
  onChange,
  onRemove,
}: {
  column: DataColumn;
  canEdit: boolean;
  removable: boolean;
  onChange: (change: (c: DataColumn) => DataColumn) => void;
  onRemove: () => void;
}) {
  const options = column.options ?? [];
  const [choice, setChoice] = useState("");
  const setType = (type: ColumnType) =>
    onChange((c) => {
      const { options: _o, ...rest } = c;
      return hasOptions(type) ? { ...rest, type, options: c.options ?? [] } : { ...rest, type };
    });
  const addOption = () => {
    const label = choice.trim();
    if (!label) return;
    const id = optionID(
      label,
      options.map((o) => o.id),
    );
    const option: DataOption =
      column.type === "status"
        ? { id, label, category: "todo", color: "neutral" }
        : { id, label, color: "neutral" };
    onChange((c) => ({ ...c, options: [...(c.options ?? []), option] }));
    setChoice("");
  };
  const setOption = (id: string, change: (o: DataOption) => DataOption) =>
    onChange((c) => ({
      ...c,
      options: (c.options ?? []).map((o) => (o.id === id ? change(o) : o)),
    }));
  return (
    <li className="artifact-column">
      <div className="artifact-column-head">
        <Input
          containerClassName="artifact-column-name"
          inputSize="sm"
          width="full"
          aria-label={`Name of column ${column.key}`}
          value={column.label ?? ""}
          disabled={!canEdit}
          onChange={(e) => onChange((c) => ({ ...c, label: e.target.value }))}
        />
        <span className="mono artifact-column-key" title={column.key}>
          {column.key}
        </span>
        <Select
          className="artifact-column-type"
          width="full"
          size="sm"
          value={column.type}
          disabled={!canEdit}
          onChange={(value) => setType(String(value) as ColumnType)}
          options={COLUMN_TYPES.map((t) => ({ value: t, label: COLUMN_TYPE_WORDS[t] }))}
          ariaLabel={`Type of ${column.label || column.key}`}
        />
        <Select
          className="artifact-column-write"
          width="full"
          size="sm"
          value={column.write ?? ""}
          disabled={!canEdit}
          onChange={(value) =>
            onChange((c) => {
              const { write: _w, when: _when, assigned_field: _a, ...rest } = c;
              return value ? { ...rest, write: String(value) as ArtifactRole } : rest;
            })
          }
          options={[
            { value: "", label: "As the collection says" },
            ...(["contributor", "editor", "owner"] as const).map((r) => ({
              value: r,
              label: `${ROLE_WORDS[r]} and above`,
            })),
          ]}
          ariaLabel={`Who writes ${column.label || column.key}`}
        />
        <Checkbox
          className="artifact-column-required"
          size="sm"
          label="Required"
          checked={column.required === true}
          disabled={!canEdit}
          onChange={(e) => onChange((c) => ({ ...c, required: e.target.checked || undefined }))}
        />
        <Checkbox
          className="artifact-column-hidden"
          size="sm"
          label="Hidden"
          checked={column.hidden === true}
          disabled={!canEdit}
          onChange={(e) => onChange((c) => ({ ...c, hidden: e.target.checked || undefined }))}
        />
        {removable ? (
          <IconButton
            className="artifact-column-remove"
            size="sm"
            variant="ghost"
            label={`Remove ${column.label || column.key}`}
            icon={<TrashGlyph size="sm" />}
            onClick={onRemove}
          />
        ) : (
          <span className="artifact-column-remove" aria-hidden="true" />
        )}
      </div>
      {hasOptions(column.type) && (
        <div className="artifact-options">
          <ul className="artifact-list" role="list">
            {options.map((o) => (
              <li key={o.id} className="artifact-option">
                <Input
                  inputSize="sm"
                  width="sm"
                  aria-label={`Label of choice ${o.id}`}
                  value={o.label}
                  disabled={!canEdit || o.archived === true}
                  onChange={(e) => setOption(o.id, (x) => ({ ...x, label: e.target.value }))}
                />
                {column.type === "status" ? (
                  <Select
                    width="auto"
                    size="sm"
                    value={o.color && o.color in STATUS_TONES ? o.color : NO_HUE}
                    disabled={!canEdit}
                    onChange={(value) => setOption(o.id, (x) => ({ ...x, color: String(value) }))}
                    options={Object.entries(STATUS_TONES).map(([tone, words]) => ({
                      value: tone,
                      label: words,
                      leading: <StatusDot tone={tone as Tone} />,
                    }))}
                    ariaLabel={`Tone of ${o.label}`}
                  />
                ) : (
                  <HuePicker
                    size="sm"
                    label={`Colour of ${o.label}`}
                    none="No colour"
                    value={o.color && isNodeHue(o.color) ? o.color : "none"}
                    disabled={!canEdit}
                    onValueChange={(hue) =>
                      setOption(o.id, (x) => ({ ...x, color: hue === "none" ? NO_HUE : hue }))
                    }
                  />
                )}
                {column.type === "status" && (
                  <Select
                    width="auto"
                    size="sm"
                    value={o.category ?? "todo"}
                    disabled={!canEdit}
                    onChange={(value) =>
                      setOption(o.id, (x) => ({ ...x, category: String(value) }))
                    }
                    options={STATUS_CATEGORIES.map((c) => ({ value: c, label: CATEGORY_WORDS[c] }))}
                    ariaLabel={`Category of ${o.label}`}
                  />
                )}
                <Checkbox
                  size="sm"
                  label="Archived"
                  checked={o.archived === true}
                  disabled={!canEdit}
                  onChange={(e) =>
                    setOption(o.id, (x) => ({ ...x, archived: e.target.checked || undefined }))
                  }
                />
              </li>
            ))}
          </ul>
          {canEdit && (
            <span className="artifact-inline">
              <Input
                inputSize="sm"
                width="sm"
                aria-label={`New choice for ${column.label || column.key}`}
                value={choice}
                placeholder="New choice"
                onChange={(e) => setChoice(e.target.value)}
              />
              <Button
                size="small"
                variant="ghost"
                leadingIcon={<PlusGlyph size="sm" />}
                disabledReason={choice.trim() ? undefined : "Name it first."}
                onClick={addOption}
              >
                Add choice
              </Button>
            </span>
          )}
        </div>
      )}
    </li>
  );
}
