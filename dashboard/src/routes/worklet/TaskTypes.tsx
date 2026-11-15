/**
 * The company's task types, edited from a project's About: what a task may be
 * filed as, which every project shares.
 *
 * # Through `PUT /worklet/catalogue`, and only the company's own
 *
 * The catalogue's `types` REPLACES the company's declarations, so every press
 * sends `declared_types` whole (archived ones included) with its one change:
 * a type added, renamed, archived or restored. The effective set the dialog
 * lists is the builtins with the company's words laid over them, and is never
 * what is sent: sent back it would copy every builtin into the company's
 * words. A builtin the company has not reworded is declared the first time it
 * is renamed or archived, starting from its own words.
 *
 * # An administrator's
 *
 * The route is admin's, so the dialog is drawn for every reader and each
 * control is held with the reason for anybody else. A slug never changes:
 * tasks hold it, and a rename moves only the words.
 */

import { useState } from "react";
import { Button, Callout, FormField, Input, Tag, useToast } from "@crewlethq/ui";
import { PlusGlyph, SlidersVerticalGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { Modal } from "~/components/Modal.tsx";
import { needsWords } from "~/lib/access.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { WRITE_REASONS, linkOf } from "~/lib/useWriteAccess.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import type { WorkTypeDef } from "~/protocol/index.ts";
import { identifierOf } from "./ProjectSettings.tsx";

/** One change to the company's types. */
export type TypeChange =
  | { kind: "add"; slug: string; name: string; description?: string }
  | { kind: "rename"; slug: string; name: string }
  | { kind: "archive"; slug: string }
  | { kind: "restore"; slug: string };

/** A declaration as the write sends it: the effective set's `builtin` mark left out. */
function declaration({ builtin: _builtin, ...rest }: WorkTypeDef): WorkTypeDef {
  return rest;
}

/**
 * The company's declarations with one change applied, as the write sends
 * them: the declared list whole, a changed type in place, and a builtin the
 * company never reworded declared from its own words (`effective`).
 */
export function typesAfter(
  declared: readonly WorkTypeDef[],
  effective: readonly WorkTypeDef[],
  change: TypeChange,
): WorkTypeDef[] {
  const out = declared.map(declaration);
  if (change.kind === "add") {
    const description = change.description?.trim();
    return [
      ...out,
      { slug: change.slug, name: change.name.trim(), ...(description ? { description } : {}) },
    ];
  }
  const at = out.findIndex((t) => t.slug === change.slug);
  const known = effective.find((t) => t.slug === change.slug);
  const base = at >= 0 ? out[at]! : known ? declaration(known) : null;
  if (base === null) return out;
  let next: WorkTypeDef;
  if (change.kind === "rename") next = { ...base, name: change.name.trim() };
  else if (change.kind === "archive") next = { ...base, archived: true };
  else {
    const { archived: _archived, ...live } = base;
    next = live;
  }
  if (at >= 0) out[at] = next;
  else out.push(next);
  return out;
}

/** Why this reader may not change the company's types, or undefined where they may. */
export function typesHold(
  viewer: ViewerState,
  link: ReturnType<typeof linkOf>,
): string | undefined {
  if (link !== "open") return WRITE_REASONS[link];
  if (viewer.loading) return WRITE_REASONS.loading;
  if (viewer.anonymous) return WRITE_REASONS.anonymous;
  if (!viewer.admin) return needsWords("Changing the company’s task types", ["admin"]);
  return undefined;
}

/** What the inline editor is open on. */
type Editing =
  | { kind: "add" }
  | { kind: "rename"; type: WorkTypeDef }
  | { kind: "archive"; type: WorkTypeDef }
  | null;

/** The company's task types, with Add, Rename, Archive and Restore. */
export function TaskTypesDialog({ onClose }: { onClose: () => void }) {
  const catalogue = useQuery("work_catalogue", { archived: true });
  const write = useKeyedGesture();
  const toast = useToast();
  const held = typesHold(useViewer(), linkOf(useConnection()));
  const [editing, setEditing] = useState<Editing>(null);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const effective = catalogue.data?.types ?? [];
  const declared = catalogue.data?.declared_types;
  const live = effective.filter((t) => !t.archived);
  const retired = effective.filter((t) => t.archived);
  // AN ANSWER WITHOUT THE COMPANY'S OWN DECLARATIONS cannot be written back:
  // the effective set in their place would copy every builtin in.
  const blocked = held ?? (declared ? undefined : "Reading the company’s types.");

  const open = (next: Editing) => {
    write.reset();
    setEditing(next);
    setName(next?.kind === "rename" ? next.type.name : "");
    setDescription("");
  };

  async function send(change: TypeChange, done: string) {
    if (blocked || write.busy || !declared) return;
    const answer = await write.run({
      method: "PUT",
      path: "/worklet/catalogue",
      body: { types: typesAfter(declared, effective, change) },
    });
    if (answer?.kind !== "done") return;
    toast.show({ variant: answer.pending ? "info" : "success", message: done });
    setEditing(null);
    catalogue.refetch();
  }

  const slug = identifierOf(name);
  const taken = effective.some((t) => t.slug === slug);
  const addHeld = !name.trim()
    ? "Name the type first."
    : taken
      ? `A type already files tasks as ${slug}.`
      : undefined;
  const renameHeld =
    editing?.kind === "rename" && name.trim() === editing.type.name
      ? "Type a different name."
      : !name.trim()
        ? "Name the type first."
        : undefined;

  return (
    <Modal
      open
      size="md"
      title="The company’s task types"
      icon={<SlidersVerticalGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      footer={
        <Button variant="secondary" onClick={onClose} disabled={write.busy}>
          Done
        </Button>
      }
    >
      <div className="col gap-3">
        <p className="t-caption">
          Every project files tasks under these. A type keeps its slug for good: renaming it changes
          only what people read.
        </p>
        <QueryState
          error={catalogue.error}
          refusal={catalogue.refusal}
          loading={catalogue.loading && !catalogue.data}
        >
          <ul className="col gap-2 task-types" aria-label="Task types">
            {live.map((type) => (
              <li key={type.slug} className="col gap-2">
                <div className="row gap-2">
                  <Tag appearance="outline" title={type.description || undefined}>
                    {type.name}
                  </Tag>
                  <span className="mono t-caption truncate">{type.slug}</span>
                  {type.builtin && <span className="t-caption muted">built in</span>}
                  <span className="spacer" />
                  <Button
                    size="small"
                    variant="ghost"
                    aria-label={`Rename the type ${type.name}`}
                    disabledReason={blocked}
                    onClick={() => open({ kind: "rename", type })}
                  >
                    Rename
                  </Button>
                  <Button
                    size="small"
                    variant="ghost"
                    aria-label={`Archive the type ${type.name}`}
                    disabledReason={blocked}
                    onClick={() => open({ kind: "archive", type })}
                  >
                    Archive
                  </Button>
                </div>
                {editing?.kind === "rename" && editing.type.slug === type.slug && (
                  <form
                    className="row gap-2 wrap"
                    onSubmit={(event) => {
                      event.preventDefault();
                      if (!renameHeld) {
                        void send(
                          { kind: "rename", slug: type.slug, name },
                          `Renamed the type “${type.name}” to “${name.trim()}”`,
                        );
                      }
                    }}
                  >
                    <FormField
                      label="Name"
                      htmlFor={`type-rename-${type.slug}`}
                      helper={`Tasks keep it as ${type.slug}, so nothing filed under it moves.`}
                    >
                      <Input
                        id={`type-rename-${type.slug}`}
                        value={name}
                        autoFocus
                        onChange={(event) => setName(event.target.value)}
                      />
                    </FormField>
                    <Button
                      type="submit"
                      size="small"
                      variant="primary"
                      loading={write.busy}
                      disabledReason={blocked ?? renameHeld}
                    >
                      Rename
                    </Button>
                    <Button size="small" variant="ghost" onClick={() => open(null)}>
                      Cancel
                    </Button>
                  </form>
                )}
                {editing?.kind === "archive" && editing.type.slug === type.slug && (
                  <Callout variant="warning" title={`Archive the type ${type.name}?`}>
                    <p>
                      No new task can be filed as it. Tasks already filed as it keep it, and you can
                      restore it here.
                    </p>
                    <div className="row gap-2">
                      <Button
                        size="small"
                        variant="danger"
                        loading={write.busy}
                        disabledReason={blocked}
                        onClick={() =>
                          void send(
                            { kind: "archive", slug: type.slug },
                            `Archived the type “${type.name}”`,
                          )
                        }
                      >
                        Archive type
                      </Button>
                      <Button size="small" variant="ghost" onClick={() => open(null)}>
                        Cancel
                      </Button>
                    </div>
                  </Callout>
                )}
              </li>
            ))}
          </ul>
          {editing?.kind === "add" ? (
            <form
              className="col gap-2"
              aria-label="Add a type"
              onSubmit={(event) => {
                event.preventDefault();
                if (!addHeld) {
                  void send(
                    { kind: "add", slug, name, description },
                    `Added the type “${name.trim()}”`,
                  );
                }
              }}
            >
              <FormField
                label="Name"
                htmlFor="type-add-name"
                helper={
                  name.trim()
                    ? `Tasks hold it as ${slug}, for good.`
                    : "What a person reads on a task, like Incident."
                }
              >
                <Input
                  id="type-add-name"
                  value={name}
                  autoFocus
                  onChange={(event) => setName(event.target.value)}
                />
              </FormField>
              <FormField label="Description" optional htmlFor="type-add-description">
                <Input
                  id="type-add-description"
                  value={description}
                  onChange={(event) => setDescription(event.target.value)}
                />
              </FormField>
              <div className="row gap-2">
                <Button
                  type="submit"
                  size="small"
                  variant="primary"
                  loading={write.busy}
                  disabledReason={blocked ?? addHeld}
                >
                  Add type
                </Button>
                <Button size="small" variant="ghost" onClick={() => open(null)}>
                  Cancel
                </Button>
              </div>
            </form>
          ) : (
            <div>
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<PlusGlyph />}
                disabledReason={blocked}
                onClick={() => open({ kind: "add" })}
              >
                Add type
              </Button>
            </div>
          )}
          {retired.length > 0 && (
            <div className="col gap-2">
              <span className="t-label">Archived</span>
              <ul className="col gap-2 task-types" aria-label="Archived task types">
                {retired.map((type) => (
                  <li key={type.slug} className="row gap-2">
                    <Tag appearance="outline">{type.name}</Tag>
                    <span className="mono t-caption truncate">{type.slug}</span>
                    <span className="spacer" />
                    <Button
                      size="small"
                      variant="ghost"
                      aria-label={`Restore the type ${type.name}`}
                      loading={write.busy}
                      disabledReason={blocked}
                      onClick={() =>
                        void send(
                          { kind: "restore", slug: type.slug },
                          `Restored the type “${type.name}”`,
                        )
                      }
                    >
                      Restore
                    </Button>
                  </li>
                ))}
              </ul>
              <span className="t-caption">They take no new work. Tasks filed as one keep it.</span>
            </div>
          )}
          {write.answer && write.answer.kind !== "done" && (
            <Callout variant={write.answer.kind === "unknown" ? "warning" : "danger"}>
              {write.answer.text}
            </Callout>
          )}
        </QueryState>
      </div>
    </Modal>
  );
}
