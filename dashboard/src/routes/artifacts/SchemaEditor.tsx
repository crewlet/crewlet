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
import { Button, Checkbox, FormField, IconButton, Input, Select, useToast } from "@crewlethq/ui";
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
import { configureSchema } from "~/lib/artifacts/api.ts";
import {
  PRESET_WORDS,
  collectionNameProblem,
  fieldNameProblem,
  hasOptions,
  keyFromLabel,
  optionID,
  schemaProblem,
  withPreset,
} from "~/lib/artifacts/schema.ts";
import { ROLE_WORDS, roleIncludes } from "~/lib/artifacts/words.ts";

/** What each column type is called. */
const TYPE_WORDS: Readonly<Record<ColumnType, string>> = {
  text: "Text",
  longtext: "Long text",
  number: "Number",
  checkbox: "Checkbox",
  select: "Choice",
  multiselect: "Choices",
  status: "Status",
  person: "Person",
  date: "Date",
  url: "Link",
  rating: "Rating",
};

/** What each status category is called. */
const CATEGORY_WORDS: Readonly<Record<(typeof STATUS_CATEGORIES)[number], string>> = {
  todo: "To do",
  in_progress: "In progress",
  done: "Done",
  blocked: "Blocked",
};

/** The colours a choice may wear: the node hues, or none. */
const CHOICE_COLORS = ["neutral", "purple", "cyan", "green", "amber", "rose", "blue"] as const;

/** The tones a status may wear: the state tones. */
const STATUS_TONES = ["neutral", "info", "success", "warning", "danger"] as const;

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
  const changed = JSON.stringify(draft) !== JSON.stringify(collections);
  const problem = schemaProblem(draft);
  const current = draft.find((c) => c.name === picked) ?? draft[0];

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
        <ul className="artifact-list" role="list">
          {draft.map((c) => (
            <li key={c.name}>
              <Button
                size="small"
                variant={c.name === current?.name ? "secondary" : "ghost"}
                aria-pressed={c.name === current?.name}
                onClick={() => setPicked(c.name)}
              >
                {c.title || c.name}
              </Button>
            </li>
          ))}
        </ul>
        {canEdit && (
          <FormField label="New collection" error={nameProblem ?? undefined}>
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
      <div className="artifact-form-actions">
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
              width="md"
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
                placeholder="Owner"
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
          inputSize="sm"
          width="sm"
          aria-label={`Name of column ${column.key}`}
          value={column.label ?? ""}
          disabled={!canEdit}
          onChange={(e) => onChange((c) => ({ ...c, label: e.target.value }))}
        />
        <span className="mono artifact-column-key">{column.key}</span>
        <Select
          width="auto"
          size="sm"
          value={column.type}
          disabled={!canEdit}
          onChange={(value) => setType(String(value) as ColumnType)}
          options={COLUMN_TYPES.map((t) => ({ value: t, label: TYPE_WORDS[t] }))}
          ariaLabel={`Type of ${column.label || column.key}`}
        />
        <Select
          width="auto"
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
          label="Required"
          checked={column.required === true}
          disabled={!canEdit}
          onChange={(e) => onChange((c) => ({ ...c, required: e.target.checked || undefined }))}
        />
        <Checkbox
          label="Hidden"
          checked={column.hidden === true}
          disabled={!canEdit}
          onChange={(e) => onChange((c) => ({ ...c, hidden: e.target.checked || undefined }))}
        />
        {removable && (
          <IconButton
            size="sm"
            variant="ghost"
            label={`Remove ${column.label || column.key}`}
            icon={<TrashGlyph size="sm" />}
            onClick={onRemove}
          />
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
                <Select
                  width="auto"
                  size="sm"
                  value={o.color ?? "neutral"}
                  disabled={!canEdit}
                  onChange={(value) => setOption(o.id, (x) => ({ ...x, color: String(value) }))}
                  options={(column.type === "status" ? STATUS_TONES : CHOICE_COLORS).map((c) => ({
                    value: c,
                    label: c,
                  }))}
                  ariaLabel={`Colour of ${o.label}`}
                />
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
