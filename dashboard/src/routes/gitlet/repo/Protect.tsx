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
 * force, no delete, no gate), and an edit's changed fields. Every list of a
 * gate changes item by item, never sent whole, so an item somebody else
 * added meanwhile is not dropped: the required checks and the classes that
 * exempt a change from approval by what is added and removed (`add`,
 * `remove`), the paths a person approves by glob (`put`, `remove`). The
 * queue's depth and batch are sent a field at a time. Turning the gate off is
 * `remove_gate`.
 *
 * REMOVING A RULE IS CONFIRMED in the page, naming what the refs lose. Every
 * control is drawn for every reader and held with its reason
 * (`lib/repoWrites.ts`): a declared repository's rules are the company
 * document's.
 */

import { useState } from "react";
import {
  Button,
  Checkbox,
  FormField,
  Input,
  ListInput,
  Select,
  Stack,
  Text,
  withDraft,
} from "@crewlethq/ui";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import {
  CODE_DEFAULT_QUEUE_BATCH,
  CODE_DEFAULT_QUEUE_DEPTH,
  CODE_MAX_QUEUE_BATCH,
  CODE_MAX_QUEUE_DEPTH,
  CODE_PATH_CLASSES,
} from "~/contract/code.ts";
import { fieldError, fieldHeld, formHeld, holdReason, type FormHold } from "~/lib/formHold.ts";
import { PATH_CLASS_WORDS } from "~/lib/protection.ts";
import { protectFormHeld, protectHeld, protectionPattern } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeMergeGate, CodeProtection, CodeRepoSummary } from "~/protocol/index.ts";

/** A rule as the page read it, with its gate where it has one. */
export interface HeldRule {
  rule: CodeProtection;
  gate: CodeMergeGate | undefined;
}

/** One path a person approves, as the form holds it: its glob and the classes that exempt it. */
export interface HumanPathDraft {
  glob: string;
  unless: readonly string[];
}

/** What the protection form holds, every field as the person left it. */
export interface ProtectForm {
  directPush: string;
  allowForce: boolean;
  allowDelete: boolean;
  gated: boolean;
  mergeBy: string;
  approvals: string;
  /** The classes that exempt a change from approval (`approvals_unless`). */
  approvalsUnless: readonly string[];
  /** The paths a person approves (`human_paths`), by glob, in the form's order. */
  humanPaths: readonly HumanPathDraft[];
  checks: string;
  /** The queue's depth and batch as typed, each a whole number from 1. */
  depth: string;
  batch: string;
  ownersApprove: boolean;
  deleteSource: string;
  agentsEditChecks: boolean;
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

/** Classes in the engine's order, each once: how a gate carries them. */
function inClassOrder(classes: readonly string[]): string[] {
  return CODE_PATH_CLASSES.filter((c) => classes.includes(c));
}

/** A list's change, item by item: what `wanted` adds to `had` and what it removes, or null for none. */
function listChange(
  wanted: readonly string[],
  had: readonly string[],
): { add?: string[]; remove?: string[] } | null {
  const add = wanted.filter((c) => !had.includes(c));
  const remove = had.filter((c) => !wanted.includes(c));
  if (!add.length && !remove.length) return null;
  return { ...(add.length ? { add } : {}), ...(remove.length ? { remove } : {}) };
}

/**
 * The human paths' change, by glob: a glob put where it is new or its
 * exempting classes moved, and a glob removed where the form dropped it, or
 * null for none. A glob typed twice is put once, as first typed.
 */
function humanPathsChange(
  wanted: readonly HumanPathDraft[],
  had: readonly { glob: string; unless?: string[] }[],
): { put?: { glob: string; unless?: string[] }[]; remove?: string[] } | null {
  const seen = new Set<string>();
  const put: { glob: string; unless?: string[] }[] = [];
  for (const w of wanted) {
    const glob = w.glob.trim();
    if (glob === "" || seen.has(glob)) continue;
    seen.add(glob);
    const unless = inClassOrder(w.unless);
    const was = had.find((h) => h.glob === glob);
    if (was && inClassOrder(was.unless ?? []).join() === unless.join()) continue;
    put.push({ glob, ...(unless.length ? { unless } : {}) });
  }
  const remove = had.map((h) => h.glob).filter((g) => !seen.has(g));
  if (!put.length && !remove.length) return null;
  return { ...(put.length ? { put } : {}), ...(remove.length ? { remove } : {}) };
}

/**
 * What a protect call sends for a form, beyond the repository and the
 * pattern: the fields that differ from the rule as it was read, or from the
 * engine's own new rule where there was none.
 */
export function protectArgs(
  held: HeldRule | undefined,
  form: ProtectForm,
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
  const exemptions = listChange(inClassOrder(form.approvalsUnless), gate?.approvals_unless ?? []);
  if (exemptions) args.approvals_unless = exemptions;
  const paths = humanPathsChange(form.humanPaths, gate?.human_paths ?? []);
  if (paths) args.human_paths = paths;
  const checks = listChange(checkNames(form.checks), gate?.checks ?? []);
  if (checks) args.checks = checks;
  // THE LANDING A FIELD AT A TIME, against the gate's or the engine's new
  // gate's (`config.CodeLanding`), so a queue's other field moved meanwhile
  // is kept.
  const depth = Number(form.depth);
  const batch = Number(form.batch);
  const landing = {
    ...(depth !== (gate?.landing.depth ?? CODE_DEFAULT_QUEUE_DEPTH) ? { depth } : {}),
    ...(batch !== (gate?.landing.batch ?? CODE_DEFAULT_QUEUE_BATCH) ? { batch } : {}),
  };
  if (Object.keys(landing).length) args.landing = landing;
  if (form.ownersApprove !== Boolean(gate?.owners_approve)) {
    args.owners_approve = form.ownersApprove;
  }
  if (form.deleteSource !== (gate?.delete_source ?? "seats")) {
    args.delete_source = form.deleteSource;
  }
  if (form.agentsEditChecks !== Boolean(gate?.agents_edit_checks)) {
    args.agents_edit_checks = form.agentsEditChecks;
  }
  return args;
}

/**
 * Why a gate's form cannot be sent yet, or undefined: the queue's depth and
 * batch are whole numbers inside the engine's bounds (`gitlet.MaxQueueDepth`,
 * `MaxQueueBatch`), each from 1, said under the field. Judged only for a gated rule.
 */
export function landingHeld(
  form: Pick<ProtectForm, "gated" | "depth" | "batch">,
): FormHold<"depth" | "batch"> | undefined {
  if (!form.gated) return undefined;
  const bad = (typed: string, max: number) => {
    const n = Number(typed);
    return typed.trim() === "" || !Number.isInteger(n) || n < 1 || n > max;
  };
  if (bad(form.depth, CODE_MAX_QUEUE_DEPTH)) {
    return fieldHeld(
      "depth",
      `Changes tested at once is a whole number from 1 to ${CODE_MAX_QUEUE_DEPTH}.`,
    );
  }
  if (bad(form.batch, CODE_MAX_QUEUE_BATCH)) {
    return fieldHeld(
      "batch",
      `Changes per candidate is a whole number from 1 to ${CODE_MAX_QUEUE_BATCH}.`,
    );
  }
  return undefined;
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
  const [form, setForm] = useState<ProtectForm>(() => ({
    directPush: held?.rule.direct_push ?? "persons",
    allowForce: Boolean(held?.rule.allow_force),
    allowDelete: Boolean(held?.rule.allow_delete),
    gated: gate !== undefined,
    mergeBy: gate?.merge_by ?? "writers",
    approvals: gate?.approvals ?? "none",
    approvalsUnless: gate?.approvals_unless ?? [],
    humanPaths: (gate?.human_paths ?? []).map((h) => ({ glob: h.glob, unless: h.unless ?? [] })),
    checks: (gate?.checks ?? []).join(", "),
    depth: String(gate?.landing.depth ?? CODE_DEFAULT_QUEUE_DEPTH),
    batch: String(gate?.landing.batch ?? CODE_DEFAULT_QUEUE_BATCH),
    ownersApprove: Boolean(gate?.owners_approve),
    deleteSource: gate?.delete_source ?? "seats",
    agentsEditChecks: Boolean(gate?.agents_edit_checks),
  }));
  // A GLOB TYPED AND NOT YET ADDED is part of what the person means to save.
  const [pathDraft, setPathDraft] = useState("");
  const set = (patch: Partial<ProtectForm>) => setForm((f) => ({ ...f, ...patch }));
  const globs = form.humanPaths.map((h) => h.glob);
  const sent: ProtectForm = {
    ...form,
    humanPaths: withDraft(globs, pathDraft).map(
      (glob) => form.humanPaths.find((h) => h.glob === glob) ?? { glob, unless: [] },
    ),
  };
  const args = protectArgs(held, sent);
  const hold: FormHold<"pattern" | "depth" | "batch"> | undefined =
    protectFormHeld(typed) ??
    landingHeld(form) ??
    (held && Object.keys(args).length === 0 ? formHeld("Nothing is changed yet.") : undefined);
  const blocked = holdReason(hold);
  const named = protectionPattern(typed);
  const checked = checkNames(form.checks).length > 0;
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { repo: repo.name, pattern: named, ...args },
      { done: held ? `Changed the protection on ${named}` : `Protected ${named}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  const exemptBy = (classes: readonly string[], c: string, on: boolean) =>
    on ? [...classes, c] : classes.filter((x) => x !== c);
  return (
    <Modal
      open
      size="md"
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
            error={fieldError(hold, "pattern")}
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
              value={form.directPush}
              onChange={(value) => set({ directPush: String(value) })}
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
          checked={form.allowForce}
          onCheckedChange={(allowForce) => set({ allowForce })}
        />
        <Checkbox
          label="Allow deletion"
          description="A push may delete it."
          checked={form.allowDelete}
          onCheckedChange={(allowDelete) => set({ allowDelete })}
        />
        <Checkbox
          label="Gate merges into it"
          description="A change merges into it only once the gate below is met."
          checked={form.gated}
          onCheckedChange={(gated) => set({ gated })}
        />
        {form.gated && (
          <>
            <FormField label="Merged by">
              {(field) => (
                <Select
                  id={field.id}
                  ariaLabel="Merged by"
                  value={form.mergeBy}
                  onChange={(value) => set({ mergeBy: String(value) })}
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
                  value={form.approvals}
                  onChange={(value) => set({ approvals: String(value) })}
                  options={[
                    { value: "none", label: "No approval" },
                    { value: "one", label: "One approval" },
                  ]}
                />
              )}
            </FormField>
            {CODE_PATH_CLASSES.map((c) => (
              <Checkbox
                key={c}
                label={`No approval for a change of ${PATH_CLASS_WORDS[c]}`}
                description={`A change whose every path changes ${PATH_CLASS_WORDS[c]} merges without one.`}
                checked={form.approvalsUnless.includes(c)}
                onCheckedChange={(on) =>
                  set({ approvalsUnless: exemptBy(form.approvalsUnless, c, on) })
                }
              />
            ))}
            <ListInput
              label="Paths a person approves"
              itemName="path"
              optional
              helper="A change touching one waits for a person's approval, whatever else approved it. A glob such as migrations/** or *.sql."
              placeholder="deploy/**"
              value={globs}
              draft={pathDraft}
              onDraftChange={setPathDraft}
              onChange={(next) =>
                set({
                  humanPaths: next.map(
                    (glob) => form.humanPaths.find((h) => h.glob === glob) ?? { glob, unless: [] },
                  ),
                })
              }
            />
            {form.humanPaths.map((h) => (
              <div key={h.glob} role="group" aria-label={`When ${h.glob} needs no person`}>
                <Stack gap={1}>
                  <Text as="div" variant="caption">
                    A person approves <span className="mono">{h.glob}</span>, unless every change to
                    it is of
                  </Text>
                  {CODE_PATH_CLASSES.map((c) => (
                    <Checkbox
                      key={c}
                      size="sm"
                      label={PATH_CLASS_WORDS[c]}
                      checked={h.unless.includes(c)}
                      onCheckedChange={(on) =>
                        set({
                          humanPaths: form.humanPaths.map((x) =>
                            x.glob === h.glob ? { ...x, unless: exemptBy(x.unless, c, on) } : x,
                          ),
                        })
                      }
                    />
                  ))}
                </Stack>
              </div>
            ))}
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
                  value={form.checks}
                  onChange={(event) => set({ checks: event.target.value })}
                />
              )}
            </FormField>
            {(checked || form.agentsEditChecks) && (
              <>
                <Checkbox
                  label="A seat may approve a change to its checks"
                  description="A change to .crewlet/**, where the checks are defined, merges on a seat's approval rather than waiting for a person's."
                  checked={form.agentsEditChecks}
                  onCheckedChange={(agentsEditChecks) => set({ agentsEditChecks })}
                />
                <FormField
                  label="Changes tested at once"
                  helper={`How many candidates its merge queue tests together, 1 to ${CODE_MAX_QUEUE_DEPTH}.`}
                  error={fieldError(hold, "depth")}
                >
                  {(field) => (
                    <Input
                      id={field.id}
                      aria-describedby={field.describedBy}
                      type="number"
                      inputMode="numeric"
                      min={1}
                      max={CODE_MAX_QUEUE_DEPTH}
                      step={1}
                      width="xs"
                      value={form.depth}
                      onChange={(event) => set({ depth: event.target.value })}
                    />
                  )}
                </FormField>
                <FormField
                  label="Changes per candidate"
                  helper={`How many waiting changes one candidate takes, 1 to ${CODE_MAX_QUEUE_BATCH}.`}
                  error={fieldError(hold, "batch")}
                >
                  {(field) => (
                    <Input
                      id={field.id}
                      aria-describedby={field.describedBy}
                      type="number"
                      inputMode="numeric"
                      min={1}
                      max={CODE_MAX_QUEUE_BATCH}
                      step={1}
                      width="xs"
                      value={form.batch}
                      onChange={(event) => set({ batch: event.target.value })}
                    />
                  )}
                </FormField>
              </>
            )}
            <Checkbox
              label="Its path owners approve"
              description="A merge waits for an approval from an owner of every owned path it touches."
              checked={form.ownersApprove}
              onCheckedChange={(ownersApprove) => set({ ownersApprove })}
            />
            <FormField label="A merged change's branch">
              {(field) => (
                <Select
                  id={field.id}
                  ariaLabel="A merged change's branch"
                  value={form.deleteSource}
                  onChange={(value) => set({ deleteSource: String(value) })}
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
