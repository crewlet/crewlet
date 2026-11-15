/**
 * A repository's protection rules, written (`code_protect_branch`): a rule
 * added for the branches or tags a pattern names, one changed, and one taken
 * away.
 *
 * # Each field sent changes that field alone
 *
 * The engine keeps the rest of a rule as it stands, so the form sends only
 * what the person moved from what the page read: a new rule's pattern and
 * whatever differs from the engine's own new rule (a person's push alone, no
 * force, no delete, no gate), and an edit's changed fields. The required
 * checks change item by item (`add`, `remove`), never as a list sent whole,
 * so a check somebody else added meanwhile is not dropped. Turning the gate
 * off is `remove_gate`.
 *
 * # What the form does not state
 *
 * A gate's path classes (`approvals_unless`, `human_paths`) and its queue's
 * depth and batch (`landing`) are shown where a rule has them, on the rule's
 * line, and are changed by the code tools or the company document: the form
 * leaves them exactly as they stand.
 *
 * REMOVING A RULE IS CONFIRMED in the page, naming what the refs lose. Every
 * control is drawn for every reader and held with its reason
 * (`lib/repoWrites.ts`): a declared repository's rules are the company
 * document's.
 */

import { useState } from "react";
import { Button, Checkbox, FormField, Input, Select, Stack } from "@crewlethq/ui";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { protectFormHeld, protectHeld, protectionPattern } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeMergeGate, CodeProtection, CodeRepoSummary } from "~/protocol/index.ts";

/** A rule as the page read it, with its gate where it has one. */
export interface HeldRule {
  rule: CodeProtection;
  gate: CodeMergeGate | undefined;
}

/** The names a list of checks holds, as a person types them: by commas or spaces. */
function checkNames(typed: string): string[] {
  return [
    ...new Set(
      typed
        .split(/[\s,]+/)
        .map((c) => c.trim())
        .filter(Boolean),
    ),
  ];
}

/**
 * What a protect call sends for a form, beyond the repository and the
 * pattern: the fields that differ from the rule as it was read, or from the
 * engine's own new rule where there was none.
 */
export function protectArgs(
  held: HeldRule | undefined,
  form: {
    directPush: string;
    allowForce: boolean;
    allowDelete: boolean;
    gated: boolean;
    mergeBy: string;
    approvals: string;
    checks: string;
    ownersApprove: boolean;
    deleteSource: string;
  },
): Record<string, unknown> {
  const was = held?.rule;
  const args: Record<string, unknown> = {};
  if (form.directPush !== (was?.direct_push ?? "persons")) args.direct_push = form.directPush;
  if (form.allowForce !== Boolean(was?.allow_force)) args.allow_force = form.allowForce;
  if (form.allowDelete !== Boolean(was?.allow_delete)) args.allow_delete = form.allowDelete;
  const gate = held?.gate;
  if (!form.gated) {
    if (gate) args.remove_gate = true;
    return args;
  }
  // A GATE NEWLY MADE NAMES WHO MERGES AND ITS APPROVALS, so the call makes
  // one even where every choice is the engine's default.
  if (!gate || form.mergeBy !== gate.merge_by) args.merge_by = form.mergeBy;
  if (!gate || form.approvals !== gate.approvals) args.approvals = form.approvals;
  const wanted = checkNames(form.checks);
  const had = gate?.checks ?? [];
  const add = wanted.filter((c) => !had.includes(c));
  const remove = had.filter((c) => !wanted.includes(c));
  if (add.length || remove.length) {
    args.checks = { ...(add.length ? { add } : {}), ...(remove.length ? { remove } : {}) };
  }
  if (form.ownersApprove !== Boolean(gate?.owners_approve)) {
    args.owners_approve = form.ownersApprove;
  }
  if (form.deleteSource !== (gate?.delete_source ?? "seats")) {
    args.delete_source = form.deleteSource;
  }
  return args;
}

/** "Protect a branch": the button that opens the form for a new rule. */
export function ProtectButton({
  repo,
  pattern = "",
  label = "Protect a branch",
}: {
  repo: CodeRepoSummary;
  /** The pattern the form opens on: a branch's row names its own. */
  pattern?: string;
  label?: string;
}) {
  const write = useAct("code_protect_branch");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={protectHeld(repo, viewer)}
        onPress={() => setOpen(true)}
      >
        {label}
      </WriteButton>
      {open && <ProtectDialog repo={repo} pattern={pattern} onClose={() => setOpen(false)} />}
    </>
  );
}

/** One rule's Edit and Remove. */
export function RuleControls({ repo, held }: { repo: CodeRepoSummary; held: HeldRule }) {
  const write = useAct("code_protect_branch");
  const viewer = useViewer();
  const [editing, setEditing] = useState(false);
  const [removing, setRemoving] = useState(false);
  const blocked = protectHeld(repo, viewer);
  const pattern = held.rule.pattern;
  return (
    <span className="row gap-1">
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={blocked}
        aria-label={`Edit the rule on ${pattern}`}
        onPress={() => setEditing(true)}
      >
        Edit
      </WriteButton>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={blocked}
        aria-label={`Remove the rule on ${pattern}`}
        onPress={() => setRemoving(true)}
      >
        Remove
      </WriteButton>
      {editing && (
        <ProtectDialog
          repo={repo}
          pattern={pattern}
          held={held}
          onClose={() => setEditing(false)}
        />
      )}
      <ConfirmModal
        open={removing}
        destructive
        title={`Remove the protection on ${pattern}?`}
        message={`Every ref it matches in ${repo.name} takes any push its writer may make again, a forced one and a delete included, and a merge into it waits on no gate.`}
        cancelLabel="Keep it"
        confirmLabel="Remove protection"
        onClose={() => setRemoving(false)}
        submitting={write.busy}
        onConfirm={async () => {
          await write.run(
            { repo: repo.name, pattern, remove: true },
            { done: `Removed the protection on ${pattern}` },
          );
          setRemoving(false);
        }}
      />
    </span>
  );
}

function ProtectDialog({
  repo,
  pattern,
  held,
  onClose,
}: {
  repo: CodeRepoSummary;
  pattern: string;
  held?: HeldRule;
  onClose: () => void;
}) {
  const write = useAct("code_protect_branch");
  const gate = held?.gate;
  const [typed, setTyped] = useState(pattern);
  const [directPush, setDirectPush] = useState(held?.rule.direct_push ?? "persons");
  const [allowForce, setAllowForce] = useState(Boolean(held?.rule.allow_force));
  const [allowDelete, setAllowDelete] = useState(Boolean(held?.rule.allow_delete));
  const [gated, setGated] = useState(gate !== undefined);
  const [mergeBy, setMergeBy] = useState(gate?.merge_by ?? "writers");
  const [approvals, setApprovals] = useState(gate?.approvals ?? "none");
  const [checks, setChecks] = useState((gate?.checks ?? []).join(", "));
  const [ownersApprove, setOwnersApprove] = useState(Boolean(gate?.owners_approve));
  const [deleteSource, setDeleteSource] = useState(gate?.delete_source ?? "seats");
  const args = protectArgs(held, {
    directPush,
    allowForce,
    allowDelete,
    gated,
    mergeBy,
    approvals,
    checks,
    ownersApprove,
    deleteSource,
  });
  const blocked =
    protectFormHeld(typed) ??
    (held && Object.keys(args).length === 0 ? "Nothing is changed yet." : undefined);
  const named = protectionPattern(typed);
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { repo: repo.name, pattern: named, ...args },
      { done: held ? `Changed the protection on ${named}` : `Protected ${named}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={held ? `Protection on ${held.rule.pattern}` : `Protect a branch of ${repo.name}`}
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
            blocked={blocked}
            onPress={() => void submit()}
          >
            {held ? "Save" : "Protect"}
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        {!held && (
          <FormField
            label="Branches or tags"
            helper="A branch's name, or a pattern such as refs/heads/release/* or refs/tags/v*."
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                width="full"
                spellCheck={false}
                placeholder="main"
                value={typed}
                onChange={(event) => setTyped(event.target.value)}
              />
            )}
          </FormField>
        )}
        <FormField label="Direct pushes">
          {(field) => (
            <Select
              id={field.id}
              ariaLabel="Direct pushes"
              value={directPush}
              onChange={(value) => setDirectPush(String(value))}
              options={[
                { value: "persons", label: "Only a person pushes to it directly" },
                { value: "none", label: "Nobody pushes to it directly" },
              ]}
            />
          )}
        </FormField>
        <Checkbox
          label="Allow forced pushes"
          description="A push may rewrite its history."
          checked={allowForce}
          onCheckedChange={setAllowForce}
        />
        <Checkbox
          label="Allow deletion"
          description="A push may delete it."
          checked={allowDelete}
          onCheckedChange={setAllowDelete}
        />
        <Checkbox
          label="Gate merges into it"
          description="A change merges into it only once the gate below is met."
          checked={gated}
          onCheckedChange={setGated}
        />
        {gated && (
          <>
            <FormField label="Merged by">
              {(field) => (
                <Select
                  id={field.id}
                  ariaLabel="Merged by"
                  value={mergeBy}
                  onChange={(value) => setMergeBy(String(value))}
                  options={[
                    { value: "writers", label: "Anybody who may write it" },
                    { value: "maintainers", label: "Its maintainers" },
                    { value: "persons", label: "A person" },
                  ]}
                />
              )}
            </FormField>
            <FormField label="Approvals">
              {(field) => (
                <Select
                  id={field.id}
                  ariaLabel="Approvals"
                  value={approvals}
                  onChange={(value) => setApprovals(String(value))}
                  options={[
                    { value: "none", label: "No approval" },
                    { value: "one", label: "One approval" },
                  ]}
                />
              )}
            </FormField>
            <FormField
              label="Required checks"
              optional
              helper="Check names, separated by commas. With any, changes land through its merge queue."
            >
              {(field) => (
                <Input
                  id={field.id}
                  aria-describedby={field.describedBy}
                  width="full"
                  spellCheck={false}
                  placeholder="unit, lint"
                  value={checks}
                  onChange={(event) => setChecks(event.target.value)}
                />
              )}
            </FormField>
            <Checkbox
              label="Its path owners approve"
              description="A merge waits for an approval from an owner of every owned path it touches."
              checked={ownersApprove}
              onCheckedChange={setOwnersApprove}
            />
            <FormField label="A merged change's branch">
              {(field) => (
                <Select
                  id={field.id}
                  ariaLabel="A merged change's branch"
                  value={deleteSource}
                  onChange={(value) => setDeleteSource(String(value))}
                  options={[
                    { value: "seats", label: "A seat's branch is deleted as it merges" },
                    { value: "none", label: "Every branch is kept" },
                  ]}
                />
              )}
            </FormField>
          </>
        )}
      </Stack>
    </Modal>
  );
}
