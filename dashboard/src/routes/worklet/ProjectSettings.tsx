/**
 * A project's own vocabulary, edited where About lists it: its labels and its
 * fields.
 *
 * # Each through `write_project`, and each facet decided by the engine
 *
 * Declaring a label is any colleague's (`tags_add`); renaming or archiving one
 * and declaring the project's own fields are the project lead's or an
 * admin's (`tags_rename`, `tags_archive`, `fields`). The engine asks each facet
 * on its own and says which one refused, in its own words, under the control
 * that was pressed. Nothing here restates who may.
 *
 * # The fields are sent whole
 *
 * `fields` REPLACES the project's own declarations, so every press sends the
 * whole set the project holds (`own_fields`, archived ones included) with the
 * one change applied: a field added, a field edited, or a field archived. The
 * effective set a task files against (`fields`) is the workspace's with the
 * project's laid over it, and is never what is sent back: sent back it copied
 * every workspace field into the project. A declaration is whole too, so an
 * edited field starts from what the project holds and changes only what the
 * dialog shows.
 *
 * # Archiving is one-way
 *
 * An archived label takes no new work and an archived field's values leave the
 * value table; neither comes back under the same slug or id. So both ask
 * before they write, and say what they cost.
 */

import { useMemo, useState } from "react";
import {
  Button,
  Card,
  Checkbox,
  ConfirmModal,
  EmptyValue,
  FormField,
  Input,
  Modal,
  Select,
  Tag,
  TagsInput,
  type SelectOption,
} from "@crewlethq/ui";
import { HashGlyph, PlusGlyph, SlidersVerticalGlyph } from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { typeName } from "~/lib/work.ts";
import type {
  WorkFieldDef,
  WorkFieldOption,
  WorkProjectDetail,
  WorkProjectTag,
} from "~/protocol/index.ts";

type ProjectWrite = ReturnType<typeof useAct<"write_project">>;

/** Whether a press landed, so its dialog can close. */
function landed(result: { kind: string } | null): boolean {
  return !!result && (result.kind === "applied" || result.kind === "pending");
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

/** What a label dialog was opened for. */
type LabelPress =
  | { kind: "add" }
  | { kind: "rename"; tag: WorkProjectTag }
  | { kind: "archive"; tag: WorkProjectTag };

/**
 * The project's labels: the live ones with Rename and Archive, Add label, and
 * the archived ones under them, which the tasks filed under one still carry.
 */
export function ProjectLabels({ detail }: { detail: WorkProjectDetail }) {
  const write = useAct("write_project");
  const [press, setPress] = useState<LabelPress | null>(null);
  const live = (detail.tags ?? []).filter((t) => !t.archived);
  const archived = detail.archived_tags ?? [];
  const close = () => {
    write.dismiss();
    setPress(null);
  };
  return (
    <Card>
      <Card.Header
        icon={<HashGlyph size="sm" />}
        subtitle="What tasks in this project are grouped by."
        actions={
          <WriteButton
            write={write}
            size="small"
            variant="secondary"
            leadingIcon={<PlusGlyph />}
            showRefusal={false}
            onPress={() => setPress({ kind: "add" })}
          >
            Add label
          </WriteButton>
        }
      >
        <Card.Title>Labels</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        {live.length === 0 ? (
          <EmptyValue label="This project declares no labels yet" />
        ) : (
          <ul className="col gap-2" aria-label="Labels">
            {live.map((tag) => (
              <li key={tag.slug} className="row gap-2">
                <Tag appearance="outline" title={tag.description || undefined}>
                  {tag.label}
                </Tag>
                <span className="mono t-caption truncate">{tag.slug}</span>
                <span className="spacer" />
                <WriteButton
                  write={write}
                  size="small"
                  variant="ghost"
                  aria-label={`Rename the label ${tag.label}`}
                  showRefusal={false}
                  onPress={() => setPress({ kind: "rename", tag })}
                >
                  Rename
                </WriteButton>
                <WriteButton
                  write={write}
                  size="small"
                  variant="ghost"
                  aria-label={`Archive the label ${tag.label}`}
                  showRefusal={false}
                  onPress={() => setPress({ kind: "archive", tag })}
                >
                  Archive
                </WriteButton>
              </li>
            ))}
          </ul>
        )}
        {archived.length > 0 && (
          <div className="col gap-1">
            <span className="t-label">Archived</span>
            <div className="row gap-2 wrap">
              {archived.map((tag) => (
                <Tag key={tag.slug} appearance="outline" title={tag.description || undefined}>
                  {tag.label}
                </Tag>
              ))}
            </div>
            <span className="t-caption">They take no new work. Tasks filed under one keep it.</span>
          </div>
        )}
        <RefusalNote write={write} />
      </div>
      {press?.kind === "add" && <AddLabelDialog detail={detail} write={write} onClose={close} />}
      {press?.kind === "rename" && (
        <RenameLabelDialog detail={detail} tag={press.tag} write={write} onClose={close} />
      )}
      {press?.kind === "archive" && (
        <ArchiveLabel detail={detail} tag={press.tag} write={write} onClose={close} />
      )}
    </Card>
  );
}

/**
 * Declare a label. The engine makes its slug from the words typed, by the rule
 * every create and filter reads labels through, so the dialog asks for the
 * label alone.
 */
function AddLabelDialog({
  detail,
  write,
  onClose,
}: {
  detail: WorkProjectDetail;
  write: ProjectWrite;
  onClose: () => void;
}) {
  const [label, setLabel] = useState("");
  const [description, setDescription] = useState("");
  const blocked = label.trim() ? undefined : "Name the label first.";
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const said = label.trim();
    const result = await write.run(
      {
        project: detail.key,
        tags_add: [
          {
            slug: said,
            label: said,
            ...(description.trim() ? { description: description.trim() } : {}),
          },
        ],
      },
      { done: `Added the label “${said}” to ${detail.name}` },
    );
    if (landed(result)) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Add a label to ${detail.name}`}
      icon={<HashGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            blocked={blocked}
            onPress={() => void submit()}
          >
            Add label
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField
          label="Label"
          htmlFor="project-label-name"
          helper="What a person reads on a task, like Needs design."
        >
          <Input
            id="project-label-name"
            value={label}
            autoFocus
            onChange={(event) => setLabel(event.target.value)}
          />
        </FormField>
        <FormField label="Description" optional htmlFor="project-label-description">
          <Input
            id="project-label-description"
            value={description}
            onChange={(event) => setDescription(event.target.value)}
          />
        </FormField>
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/** Rename a label: its words change, its slug (what every task holds) does not. */
function RenameLabelDialog({
  detail,
  tag,
  write,
  onClose,
}: {
  detail: WorkProjectDetail;
  tag: WorkProjectTag;
  write: ProjectWrite;
  onClose: () => void;
}) {
  const [label, setLabel] = useState(tag.label);
  const said = label.trim();
  const blocked = !said
    ? "Name the label first."
    : said === tag.label
      ? "Type a different name."
      : undefined;
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { project: detail.key, tags_rename: { [tag.slug]: said } },
      { done: `Renamed the label “${tag.label}” to “${said}”` },
    );
    if (landed(result)) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Rename the label ${tag.label}`}
      icon={<HashGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            blocked={blocked}
            onPress={() => void submit()}
          >
            Rename
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField
          label="Label"
          htmlFor="project-label-rename"
          helper={`Tasks keep it under ${tag.slug}, so nothing filed under it moves.`}
        >
          <Input
            id="project-label-rename"
            value={label}
            autoFocus
            onChange={(event) => setLabel(event.target.value)}
          />
        </FormField>
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/** Archive a label, after saying it is one-way. */
function ArchiveLabel({
  detail,
  tag,
  write,
  onClose,
}: {
  detail: WorkProjectDetail;
  tag: WorkProjectTag;
  write: ProjectWrite;
  onClose: () => void;
}) {
  return (
    <ConfirmModal
      open
      onClose={onClose}
      title={`Archive the label ${tag.label}?`}
      message="No new task can take it, and it cannot be brought back. Tasks already filed under it keep it."
      confirmLabel="Archive label"
      destructive
      submitting={write.busy}
      onConfirm={async () => {
        const result = await write.run(
          { project: detail.key, tags_archive: [tag.slug] },
          { done: `Archived the label “${tag.label}” in ${detail.name}` },
        );
        if (landed(result)) onClose();
      }}
    >
      <RefusalNote write={write} />
    </ConfirmModal>
  );
}

// ---------------------------------------------------------------------------
// Fields
// ---------------------------------------------------------------------------

/**
 * The field types a person may declare here, in their words. Not `rollup`: a
 * rollup's value keeps itself up to date and this build computes none, so the
 * engine refuses every declaration that would make one useful.
 */
export const FIELD_TYPES: readonly { value: string; label: string }[] = [
  { value: "text", label: "Text" },
  { value: "textarea", label: "Long text" },
  { value: "number", label: "Number" },
  { value: "date", label: "Date" },
  { value: "dropdown", label: "Dropdown" },
  { value: "labels", label: "Labels" },
  { value: "checkbox", label: "Checkbox" },
  { value: "url", label: "Link" },
  { value: "email", label: "Email" },
  { value: "progress", label: "Progress" },
  { value: "people", label: "People" },
];

/** The types whose value is one of a list of choices the field declares. */
const CHOOSES = new Set(["dropdown", "labels"]);

/** A type in words, or the slug itself for a type this build has no word for. */
export function fieldTypeWord(type: string): string {
  return FIELD_TYPES.find((t) => t.value === type)?.label ?? type;
}

/**
 * An identifier from what a person typed, the shape the engine takes for a
 * field's and an option's slug: lower case, words joined by underscores. A
 * name that leaves nothing usable is sent as typed, and the engine's refusal
 * names the rule.
 */
export function identifierOf(name: string): string {
  const joined = name
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
  const lead = /^[a-z]/.test(joined) ? joined : joined ? `f_${joined}` : "";
  return lead.slice(0, 32).replace(/_+$/, "") || name.trim();
}

/** A declaration as a write sends it: the fields its writer stamps left out. */
function declaration(field: WorkFieldDef): Record<string, unknown> {
  const out: Record<string, unknown> = {
    id: field.id,
    slug: field.slug,
    name: field.name,
    type: field.type,
  };
  if (field.description) out.description = field.description;
  if (field.applies_to?.length) out.applies_to = field.applies_to;
  if (field.required) out.required = true;
  if (field.required_in_subtasks) out.required_in_subtasks = true;
  if (field.archived) out.archived = true;
  if (field.pinned) out.pinned = true;
  if (field.hide_from_agents) out.hide_from_agents = true;
  if (field.config && Object.keys(field.config).length > 0) out.config = field.config;
  return out;
}

/**
 * The whole `fields` argument: the project's own set with one declaration
 * replaced (by id) or, for a new one, added at the end.
 */
export function fieldsWith(own: readonly WorkFieldDef[], changed: WorkFieldDef): unknown[] {
  const replaced = own.some((f) => f.id !== "" && f.id === changed.id);
  const next = replaced ? own.map((f) => (f.id === changed.id ? changed : f)) : [...own, changed];
  return next.map(declaration);
}

/** What a field dialog was opened for. */
type FieldPress =
  | { kind: "add" }
  | { kind: "edit"; field: WorkFieldDef }
  | { kind: "archive"; field: WorkFieldDef };

/**
 * The project's own fields, with Add, Edit and Archive, and under them the
 * company's fields its tasks carry as well, which are edited in the company's
 * catalogue rather than here.
 */
export function ProjectFields({ detail }: { detail: WorkProjectDetail }) {
  const write = useAct("write_project");
  const [press, setPress] = useState<FieldPress | null>(null);
  const own = detail.own_fields ?? [];
  const live = own.filter((f) => !f.archived);
  const retired = own.filter((f) => f.archived);
  // THE COMPANY'S, as the effective set carries them: whatever a task here
  // files with that the project did not declare itself.
  const company = useMemo(() => {
    const ownIds = new Set((detail.own_fields ?? []).map((f) => f.id));
    const seen = new Set<string>();
    const out: WorkFieldDef[] = [];
    for (const group of detail.fields ?? []) {
      for (const field of group.fields) {
        if (ownIds.has(field.id) || seen.has(field.id)) continue;
        seen.add(field.id);
        out.push(field);
      }
    }
    return out;
  }, [detail.fields, detail.own_fields]);
  const close = () => {
    write.dismiss();
    setPress(null);
  };
  return (
    <Card>
      <Card.Header
        icon={<SlidersVerticalGlyph size="sm" />}
        subtitle="What a task in this project records beside its title, as this project declares it."
        actions={
          <WriteButton
            write={write}
            size="small"
            variant="secondary"
            leadingIcon={<PlusGlyph />}
            showRefusal={false}
            onPress={() => setPress({ kind: "add" })}
          >
            Add field
          </WriteButton>
        }
      >
        <Card.Title>Fields</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        {live.length === 0 ? (
          <EmptyValue label="This project declares no fields of its own" />
        ) : (
          <ul className="col gap-2" aria-label="This project's fields">
            {live.map((field) => (
              <li key={field.id} className="row gap-2">
                <Tag appearance="outline" title={field.description || undefined}>
                  {field.name}
                </Tag>
                <span className="t-caption truncate">{fieldSummary(field, detail)}</span>
                <span className="spacer" />
                <WriteButton
                  write={write}
                  size="small"
                  variant="ghost"
                  aria-label={`Edit the field ${field.name}`}
                  showRefusal={false}
                  onPress={() => setPress({ kind: "edit", field })}
                >
                  Edit
                </WriteButton>
                <WriteButton
                  write={write}
                  size="small"
                  variant="ghost"
                  aria-label={`Archive the field ${field.name}`}
                  showRefusal={false}
                  onPress={() => setPress({ kind: "archive", field })}
                >
                  Archive
                </WriteButton>
              </li>
            ))}
          </ul>
        )}
        {retired.length > 0 && (
          <div className="col gap-1">
            <span className="t-label">Archived</span>
            <div className="row gap-2 wrap">
              {retired.map((field) => (
                <Tag key={field.id} appearance="outline">
                  {field.name}
                </Tag>
              ))}
            </div>
          </div>
        )}
        {company.length > 0 && (
          <div className="col gap-1">
            <span className="t-label">From the company</span>
            <div className="row gap-2 wrap">
              {company.map((field) => (
                <Tag key={field.id} appearance="outline" title={field.description || undefined}>
                  {field.name}
                </Tag>
              ))}
            </div>
            <span className="t-caption">Every project's tasks carry these as well.</span>
          </div>
        )}
        <RefusalNote write={write} />
      </div>
      {(press?.kind === "add" || press?.kind === "edit") && (
        <FieldDialog
          detail={detail}
          field={press.kind === "edit" ? press.field : null}
          write={write}
          onClose={close}
        />
      )}
      {press?.kind === "archive" && (
        <ArchiveField detail={detail} field={press.field} write={write} onClose={close} />
      )}
    </Card>
  );
}

/** One line under a field's name: its type, whether it is required, and where. */
function fieldSummary(field: WorkFieldDef, detail: WorkProjectDetail): string {
  const parts = [fieldTypeWord(field.type)];
  if (field.required) parts.push("required");
  if (field.applies_to?.length) {
    parts.push(field.applies_to.map((slug) => typeName(slug, detail.types)).join(", "));
  }
  return parts.join(" · ");
}

/**
 * The choices a dropdown or labels field keeps, after the dialog's list: a name
 * still in the list keeps its option (and its id, which every stored value
 * holds), a new name is a new option, and a name taken out is ARCHIVED rather
 * than dropped, so a task that chose it still reads it.
 */
export function optionsAfter(
  was: readonly WorkFieldOption[],
  names: readonly string[],
): Partial<WorkFieldOption>[] {
  const out: Partial<WorkFieldOption>[] = [];
  const kept = new Set<string>();
  for (const name of names) {
    const trimmed = name.trim();
    if (!trimmed) continue;
    const held = was.find((o) => !o.archived && o.name === trimmed);
    if (held) {
      kept.add(held.id);
      out.push(held);
    } else {
      out.push({ slug: identifierOf(trimmed), name: trimmed });
    }
  }
  for (const option of was) {
    if (kept.has(option.id)) continue;
    out.push(option.archived ? option : { ...option, archived: true });
  }
  return out;
}

/**
 * Add a field, or edit one the project declares. A new field takes its slug
 * from its name; an existing field keeps its slug and its type, since saved
 * views and agents name it by the slug and its stored values are of its type.
 */
function FieldDialog({
  detail,
  field,
  write,
  onClose,
}: {
  detail: WorkProjectDetail;
  field: WorkFieldDef | null;
  write: ProjectWrite;
  onClose: () => void;
}) {
  const [name, setName] = useState(field?.name ?? "");
  const [slug, setSlug] = useState(field?.slug ?? "");
  const [slugTouched, setSlugTouched] = useState(false);
  const [type, setType] = useState(field?.type ?? "text");
  const [description, setDescription] = useState(field?.description ?? "");
  const [required, setRequired] = useState(field?.required ?? false);
  const [appliesTo, setAppliesTo] = useState<string[]>(field?.applies_to ?? []);
  const [choices, setChoices] = useState<string[]>(
    (field?.config?.options ?? []).filter((o) => !o.archived).map((o) => o.name),
  );
  const shownSlug = field ? field.slug : slugTouched ? slug : identifierOf(name);
  const chooses = CHOOSES.has(type);
  const blocked = !name.trim()
    ? "Name the field first."
    : chooses && choices.length === 0
      ? "Give it at least one choice."
      : undefined;
  const typeOptions: SelectOption[] = FIELD_TYPES.map((t) => ({ value: t.value, label: t.label }));
  const appliesOptions: SelectOption[] = (detail.types ?? [])
    .filter((t) => !t.archived)
    .map((t) => ({ value: t.slug, label: typeName(t.slug, detail.types) }));

  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const config = { ...(field?.config ?? {}) };
    if (chooses) config.options = optionsAfter(field?.config?.options ?? [], choices) as never;
    else delete config.options;
    const next: WorkFieldDef = {
      ...(field ?? { id: "" }),
      slug: shownSlug,
      name: name.trim(),
      type,
      description: description.trim() || undefined,
      required,
      applies_to: appliesTo,
      config,
    };
    const fields = fieldsWith(detail.own_fields ?? [], next).map((f) => {
      // A NEW FIELD GOES WITHOUT AN ID, and the engine mints one.
      const entry = f as Record<string, unknown>;
      if (entry.id === "") delete entry.id;
      return entry;
    });
    const result = await write.run(
      { project: detail.key, fields },
      {
        done: field
          ? `Saved the field ${next.name} in ${detail.name}`
          : `Added the field ${next.name} to ${detail.name}`,
      },
    );
    if (landed(result)) onClose();
  };

  return (
    <Modal
      open
      size="md"
      title={field ? `Edit the field ${field.name}` : `Add a field to ${detail.name}`}
      icon={<SlidersVerticalGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            blocked={blocked}
            onPress={() => void submit()}
          >
            {field ? "Save field" : "Add field"}
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField label="Name" htmlFor="project-field-name">
          <Input
            id="project-field-name"
            value={name}
            autoFocus
            onChange={(event) => setName(event.target.value)}
          />
        </FormField>
        <FormField
          label="Slug"
          htmlFor="project-field-slug"
          helper={
            field
              ? "Saved views and agents name the field by it, so it stays."
              : "What a filter and an agent type for it. Lower case, with underscores."
          }
        >
          <Input
            id="project-field-slug"
            className="mono"
            value={shownSlug}
            disabled={!!field}
            onChange={(event) => {
              setSlugTouched(true);
              setSlug(event.target.value);
            }}
          />
        </FormField>
        <FormField
          label="Type"
          htmlFor="project-field-type"
          helper={field ? "Its values are stored as this type, so it stays." : undefined}
        >
          <Select
            id="project-field-type"
            ariaLabel="Type"
            value={type}
            disabled={!!field}
            options={typeOptions}
            onChange={(next) => setType(String(next))}
          />
        </FormField>
        {chooses && (
          <FormField
            label="Choices"
            htmlFor="project-field-choices"
            helper="Press Enter after each. A choice taken out is archived, and a task that chose it keeps it."
          >
            {(f) => (
              <TagsInput
                id={f.id}
                aria-label="Choices"
                aria-describedby={f.describedBy}
                value={choices}
                onChange={setChoices}
                allowCustom
              />
            )}
          </FormField>
        )}
        <FormField
          label="Applies to"
          optional
          htmlFor="project-field-applies"
          helper="Leave empty for every type of task."
        >
          <Select
            id="project-field-applies"
            ariaLabel="Applies to"
            multiple
            value={appliesTo}
            placeholder="Every type"
            options={appliesOptions}
            onChange={(next) => setAppliesTo(Array.isArray(next) ? next.map(String) : [])}
          />
        </FormField>
        <FormField label="Description" optional htmlFor="project-field-description">
          <Input
            id="project-field-description"
            value={description}
            onChange={(event) => setDescription(event.target.value)}
          />
        </FormField>
        <Checkbox
          label="Required"
          description="A task cannot be filed here without it."
          checked={required}
          onCheckedChange={setRequired}
        />
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/** Archive a field, after saying it is one-way and what it costs. */
function ArchiveField({
  detail,
  field,
  write,
  onClose,
}: {
  detail: WorkProjectDetail;
  field: WorkFieldDef;
  write: ProjectWrite;
  onClose: () => void;
}) {
  return (
    <ConfirmModal
      open
      onClose={onClose}
      title={`Archive the field ${field.name}?`}
      message="Tasks stop carrying it, and it cannot be brought back. A field like it later is a new field, with no values."
      confirmLabel="Archive field"
      destructive
      submitting={write.busy}
      onConfirm={async () => {
        const result = await write.run(
          {
            project: detail.key,
            fields: fieldsWith(detail.own_fields ?? [], { ...field, archived: true }),
          },
          { done: `Archived the field ${field.name} in ${detail.name}` },
        );
        if (landed(result)) onClose();
      }}
    >
      <RefusalNote write={write} />
    </ConfirmModal>
  );
}
