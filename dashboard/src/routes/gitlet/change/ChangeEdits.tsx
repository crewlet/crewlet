/**
 * A live change's edits (`code_update_change`, one per call): back to draft, target, work item.
 */

import { useState } from "react";
import { Button, FormField, Input, Stack } from "@crewlethq/ui";
import { PencilGlyph, RotateCcwGlyph } from "@crewlethq/icons/glyphs";
import { Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { changeLive, refName, shortOID } from "~/lib/code.ts";
import { useAct } from "~/lib/useAct.ts";
import type { CodeChangeDetail, WorkItemRef } from "~/protocol/index.ts";
import { useBranchDetail } from "../branchDetail.ts";

/** What `work_item` says to carry none (the engine's `noWorkItem`). */
export const NO_WORK_ITEM = "none";

/** A work item as the reader names it in the field: a Worklet item by its key, another tracker's as `<backend>:<id>`. */
export function workItemName(work: WorkItemRef | null | undefined): string {
  if (!work) return "";
  return work.backend === "worklet" ? work.key || work.id : `${work.backend}:${work.id}`;
}

/** "Convert to draft", on an open change: it stops asking to be merged until it is marked ready again. */
export function ConvertToDraft({ change }: { change: CodeChangeDetail }) {
  const write = useAct("code_update_change");
  if (change.state !== "open") return null;
  return (
    <p className="code-rail__note">
      Still in progress?{" "}
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        onPress={() =>
          void write.run(
            { change: change.ref, expected_version: change.version, draft: true },
            { done: `Converted ${change.ref} to a draft` },
          )
        }
      >
        Convert to draft
      </WriteButton>
    </p>
  );
}

/** One edit's Edit press and its dialog: a single field, sent as one update. */
function EditOne({
  change,
  label,
  title,
  field,
  helper,
  placeholder,
  initial,
  held,
  send,
}: {
  change: CodeChangeDetail;
  /** The press's name, for a reader of assistive tech. */
  label: string;
  title: string;
  field: string;
  helper: string;
  placeholder: string;
  initial: string;
  /** Why the value typed would change nothing, or be refused. */
  held: (value: string) => string | undefined;
  /** The update's arguments and what its press says it did. */
  send: (value: string) => { args: Record<string, unknown>; done: string };
}) {
  const write = useAct("code_update_change");
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(initial);
  if (!changeLive(change.state)) return null;
  const blocked = held(value.trim());
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const { args, done } = send(value.trim());
    const result = await write.run(
      { change: change.ref, expected_version: change.version, ...args },
      { done },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) setOpen(false);
  };
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        aria-label={label}
        leadingIcon={<PencilGlyph size="sm" />}
        showRefusal={false}
        onPress={() => {
          setValue(initial);
          setOpen(true);
        }}
      >
        Edit
      </WriteButton>
      {open && (
        <Modal
          open
          size="sm"
          title={title}
          onClose={() => setOpen(false)}
          onSubmit={() => void submit()}
          dismissable={!write.busy}
          footer={
            <>
              <Button variant="secondary" onClick={() => setOpen(false)} disabled={write.busy}>
                Cancel
              </Button>
              <WriteButton
                write={write}
                variant="primary"
                blocked={blocked}
                onPress={() => void submit()}
              >
                Save
              </WriteButton>
            </>
          }
        >
          <Stack gap={3}>
            <FormField label={field} helper={helper}>
              {(f) => (
                <Input
                  id={f.id}
                  aria-describedby={f.describedBy}
                  width="full"
                  spellCheck={false}
                  autoFocus
                  placeholder={placeholder}
                  value={value}
                  onChange={(event) => setValue(event.target.value)}
                />
              )}
            </FormField>
          </Stack>
        </Modal>
      )}
    </>
  );
}

/** Edit at the rail's Branches: the branch a live change merges into, retargeted to another live one. */
export function EditTarget({ change }: { change: CodeChangeDetail }) {
  const target = refName(change.target_ref).name;
  const source = refName(change.source_ref).name;
  return (
    <EditOne
      change={change}
      label={`Change the branch ${change.ref} merges into`}
      title={`Merge ${change.ref} into another branch`}
      field="Target branch"
      helper={`A live branch of ${change.repo} other than ${source}. Its gate and its checks judge the change from then on.`}
      placeholder={target}
      initial={target}
      held={(value) =>
        value === ""
          ? "Name the branch it merges into."
          : value === target
            ? `It merges into ${target} already.`
            : value === source
              ? `${source} is what the change proposes: it merges into another branch.`
              : undefined
      }
      send={(value) => ({ args: { target: value }, done: `Retargeted ${change.ref} to ${value}` })}
    />
  );
}

/** Edit at the rail's Work item: the task the change does, set, changed or taken off. */
export function EditWorkItem({ change }: { change: CodeChangeDetail }) {
  const current = workItemName(change.work_item);
  return (
    <EditOne
      change={change}
      label={`Change the work item of ${change.ref}`}
      title={`The work item of ${change.ref}`}
      field="Work item"
      helper="The task the change does, by its key, or another tracker's as backend:id. Left empty, it carries none."
      placeholder="ENG-42"
      initial={current}
      held={(value) =>
        value === current
          ? current
            ? `It carries ${current} already.`
            : "It carries no work item: name one."
          : undefined
      }
      send={(value) =>
        value === ""
          ? {
              args: { work_item: NO_WORK_ITEM },
              done: `Took the work item off ${change.ref}`,
            }
          : { args: { work_item: value }, done: `Set ${change.ref}'s work item to ${value}` }
      }
    />
  );
}

/** "Restore branch": a finished change's deleted source branch, made again at its head. */
export function RestoreBranch({ change }: { change: CodeChangeDetail }) {
  const write = useAct("code_create_branch");
  const source = refName(change.source_ref).name;
  const finished = !changeLive(change.state);
  const found = useBranchDetail(change.repo, source, finished);
  if (!finished || found.loading || found.error || found.row) return null;
  return (
    <p className="code-rail__row">
      <span className="code-rail__note">deleted</span>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        leadingIcon={<RotateCcwGlyph size="sm" />}
        onPress={() =>
          void write.run(
            { repo: change.repo, branch: source, at: change.head },
            { done: `Restored ${source} at ${shortOID(change.head)}` },
          )
        }
      >
        Restore branch
      </WriteButton>
    </p>
  );
}
