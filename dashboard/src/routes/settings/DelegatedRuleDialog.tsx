/**
 * Adding or changing one delegated-decision rule: a side sheet over Settings
 * › Delegated decisions, filled from the company document as it is when the
 * sheet opens (`lib/delegatedRules.ts` has the rules a save turns on).
 *
 * FILLED FROM THE DOCUMENT, NOT FROM THE PAGE. The page draws the rules this
 * node applied; the document may already hold a newer revision of them, and
 * a form filled from the page would write the older rule back over it. The
 * sheet reads the document first, and the save is refused as a conflict if
 * the rule changed again before it.
 *
 * THE ENGINE JUDGES THE RULE. Which seats may delegate, who an escalation may
 * be put to, a kind in two rules: each is refused by the dry run every save
 * makes first, in the engine's words, under the fields
 * (`components/SettingWriteStatus.tsx`).
 */

import { useEffect, useId, useMemo, useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  ConfirmModal,
  FormField,
  IconButton,
  Input,
  Modal,
  Skeleton,
  TagsInput,
  type TagsInputOption,
} from "@crewlethq/ui";
import { PlusGlyph, ShieldCheckGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import { configRefusalWords } from "~/lib/configRefusal.ts";
import { kindWords } from "~/lib/delegation.ts";
import {
  addRulePlan,
  DELEGABLE_KINDS,
  documentRules,
  draftOf,
  editRulePlan,
  readDraft,
  releasesIn,
  type DocumentRule,
  type RuleDraft,
  type RuleField,
} from "~/lib/delegatedRules.ts";
import { useOrgIndex } from "~/lib/seats.ts";
import type { SettingWrite } from "~/lib/useSettingWrite.ts";
import { getConfig } from "~/protocol/configWrite.ts";
import { isAbort } from "~/protocol/rest.ts";

/** Which rule the sheet is for. */
export type RuleTarget =
  { readonly kind: "add" } | { readonly kind: "edit"; readonly number: number };

/** What the sheet was filled from. */
type Filled =
  | { readonly kind: "reading" }
  /** `was` is the rule as the document writes it; null for a new rule. */
  | { readonly kind: "ready"; readonly was: DocumentRule | null }
  | { readonly kind: "refused"; readonly message: string; readonly reload: boolean };

export const RULES_REREAD = "Reload to see the rules as they are now.";

export function DelegatedRuleDialog({
  target,
  write,
  onClose,
  onReload,
}: {
  target: RuleTarget;
  /** The page's one write of the rules, which the sheet's Save drives. */
  write: SettingWrite;
  onClose: () => void;
  /** Re-read the page, offered where a fresh read is the remedy. */
  onReload: () => void;
}) {
  const [filled, setFilled] = useState<Filled>({ kind: "reading" });
  const [draft, setDraft] = useState<RuleDraft>(() => draftOf());
  const [errors, setErrors] = useState<Partial<Record<RuleField, string>>>({});
  const [discarding, setDiscarding] = useState(false);
  const number = target.kind === "edit" ? target.number : 0;

  useEffect(() => {
    const controller = new AbortController();
    void (async () => {
      try {
        const read = await getConfig(controller.signal);
        if (read.kind !== "document") {
          setFilled({ kind: "refused", ...configRefusalWords(read, RULES_REREAD) });
          return;
        }
        if (number === 0) {
          setFilled({ kind: "ready", was: null });
          return;
        }
        const was = documentRules(read.document)[number - 1];
        if (was === undefined) {
          setFilled({
            kind: "refused",
            message: `The company document no longer has a rule ${number}. ${RULES_REREAD}`,
            reload: true,
          });
          return;
        }
        setDraft(draftOf(was));
        setFilled({ kind: "ready", was });
      } catch (err) {
        if (isAbort(err)) return;
        setFilled({
          kind: "refused",
          message: `The rules could not be read: ${err instanceof Error ? err.message : String(err)}.`,
          reload: true,
        });
      }
    })();
    return () => controller.abort();
  }, [number]);

  const index = useOrgIndex();
  const agents = useMemo(() => seatOptions(index.seats, "agent"), [index.seats]);
  const people = useMemo(() => seatOptions(index.seats, "human"), [index.seats]);

  const was = filled.kind === "ready" ? filled.was : null;
  const dirty =
    filled.kind === "ready" && JSON.stringify(draft) !== JSON.stringify(draftOf(was ?? undefined));
  const set = (patch: Partial<RuleDraft>) => {
    setDraft((d) => ({ ...d, ...patch }));
    setErrors({});
    if (write.state.kind !== "idle" && !write.busy) write.reset();
  };

  function save() {
    if (write.busy || filled.kind !== "ready") return;
    // ENTER ON A WARNING IS THE SECOND PRESS the warning asked for.
    if (write.state.kind === "warned") {
      write.confirm();
      return;
    }
    const read = readDraft(draft);
    if (!read.ok) {
      setErrors(read.errors);
      return;
    }
    write.submit(
      filled.was === null ? addRulePlan(read.rule) : editRulePlan(number, filled.was, read.rule),
    );
  }

  function close() {
    if (write.busy) return;
    if (dirty) {
      setDiscarding(true);
      return;
    }
    write.reset();
    onClose();
  }

  const releases = releasesIn(draft.kinds);
  const kindsId = useId();
  const title = target.kind === "add" ? "Add a rule" : `Change rule ${number}`;
  return (
    <>
      <Modal
        open
        variant="sheet"
        size="md"
        title={title}
        subtitle="Requests of the rule's kinds go to its delegates first, then to people."
        icon={<ShieldCheckGlyph size="sm" />}
        onClose={close}
        onSubmit={save}
        dismissable={!write.busy}
        stackBody
        footer={
          <>
            <Button type="button" variant="ghost" onClick={close} disabled={write.busy}>
              Cancel
            </Button>
            <Button
              type="submit"
              variant="primary"
              loading={write.busy}
              disabled={filled.kind !== "ready"}
            >
              {write.state.kind === "warned" ? "Save anyway" : "Save"}
            </Button>
          </>
        }
      >
        {filled.kind === "reading" && <Skeleton variant="text" rows={6} label="Loading" />}
        {filled.kind === "refused" && (
          <Callout variant="danger" title="The rule could not be opened">
            <span>{filled.message}</span>
            {filled.reload && (
              <div className="row gap-2">
                <Button
                  size="small"
                  variant="secondary"
                  onClick={() => {
                    onReload();
                    onClose();
                  }}
                >
                  Reload
                </Button>
              </div>
            )}
          </Callout>
        )}
        {filled.kind === "ready" && (
          <>
            <fieldset className="col gap-1" aria-labelledby={kindsId} disabled={write.busy}>
              <legend id={kindsId} className="t-label">
                Decides
              </legend>
              {DELEGABLE_KINDS.map((kind) => (
                <Checkbox
                  key={kind}
                  label={kindWords(kind)}
                  checked={draft.kinds.includes(kind)}
                  onCheckedChange={(on) =>
                    set({
                      kinds: on
                        ? DELEGABLE_KINDS.filter((k) => k === kind || draft.kinds.includes(k))
                        : draft.kinds.filter((k) => k !== kind),
                    })
                  }
                />
              ))}
            </fieldset>
            <SeatsField
              label="Delegates"
              helper="The agent seat that decides, or two for a panel that must both allow."
              value={draft.delegates}
              options={agents}
              disabled={write.busy}
              onChange={(delegates) => set({ delegates })}
            />
            <SeatsField
              label="Escalates to"
              helper="The people an escalation is put to, beside administrators: at least one."
              value={draft.escalateTo}
              options={people}
              disabled={write.busy}
              onChange={(escalateTo) => set({ escalateTo })}
            />
            <SampleField
              value={draft.sample}
              error={errors.sample}
              disabled={write.busy}
              onChange={(sample) => set({ sample })}
            />
            {releases && (
              <PathsField
                value={draft.paths}
                disabled={write.busy}
                onChange={(paths) => set({ paths })}
              />
            )}
            {releases && (
              <ChecksField
                value={draft.checks}
                error={errors.checks}
                disabled={write.busy}
                onChange={(checks) => set({ checks })}
              />
            )}
            <SettingWriteStatus
              write={write}
              unchanged="The rule already says that."
              onReload={onReload}
            />
          </>
        )}
      </Modal>
      <ConfirmModal
        open={discarding}
        destructive
        title="Discard your changes?"
        message={`Your changes to ${target.kind === "add" ? "the new rule" : `rule ${number}`} aren’t saved.`}
        cancelLabel="Keep editing"
        confirmLabel="Discard changes"
        onClose={() => setDiscarding(false)}
        onConfirm={() => {
          setDiscarding(false);
          write.reset();
          onClose();
        }}
      />
    </>
  );
}

/** The seats of one kind as a picker's options: the name, the handle under it. */
function seatOptions(
  seats: readonly { handle: string; name: string; kind: "agent" | "human" }[],
  kind: "agent" | "human",
): TagsInputOption[] {
  return seats
    .filter((s) => s.kind === kind)
    .map((s) => ({ value: s.handle, label: s.name || s.handle, description: s.handle }));
}

/**
 * Seats picked by handle. A handle the chart does not hold stays a chip of
 * its own (`allowCustom`), so a rule naming a seat this page cannot see is
 * written back as it was rather than dropped.
 */
function SeatsField({
  label,
  helper,
  value,
  options,
  disabled,
  onChange,
}: {
  label: string;
  helper: string;
  value: string[];
  options: TagsInputOption[];
  disabled: boolean;
  onChange: (next: string[]) => void;
}) {
  const id = useId();
  return (
    <FormField htmlFor={id} label={label} helper={helper}>
      <TagsInput
        id={id}
        value={value}
        options={options}
        onChange={onChange}
        disabled={disabled}
        label={label}
        placeholder="Pick a seat"
      />
    </FormField>
  );
}

function SampleField({
  value,
  error,
  disabled,
  onChange,
}: {
  value: string;
  error: string | undefined;
  disabled: boolean;
  onChange: (next: string) => void;
}) {
  const id = useId();
  return (
    <FormField
      htmlFor={id}
      label="Spot-check one in"
      optional
      error={error}
      helper="One decision in this many is shown to the people it escalates to, after the fact. Empty is the engine's default."
    >
      <Input
        id={id}
        value={value}
        inputMode="numeric"
        autoComplete="off"
        placeholder="Default"
        error={Boolean(error)}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        width="sm"
      />
    </FormField>
  );
}

function PathsField({
  value,
  disabled,
  onChange,
}: {
  value: string[];
  disabled: boolean;
  onChange: (next: string[]) => void;
}) {
  const id = useId();
  return (
    <FormField
      htmlFor={id}
      label="Always escalates"
      optional
      helper="Repository path globs (docs/**, services/*/billing/**): a merge or deploy touching one goes to people, beside the built-in list."
    >
      <TagsInput
        id={id}
        value={value}
        onChange={onChange}
        disabled={disabled}
        monospace
        submitKeys={["Enter"]}
        label="Always escalates"
        placeholder="A path glob, then Enter"
      />
    </FormField>
  );
}

/**
 * The checks a rule requires, one entry per repository and branch. A check's
 * name may hold a comma (a matrix job's), so only Enter adds one.
 */
function ChecksField({
  value,
  error,
  disabled,
  onChange,
}: {
  value: RuleDraft["checks"];
  error: string | undefined;
  disabled: boolean;
  onChange: (next: RuleDraft["checks"]) => void;
}) {
  const put = (i: number, patch: Partial<RuleDraft["checks"][number]>) =>
    onChange(value.map((entry, at) => (at === i ? { ...entry, ...patch } : entry)));
  return (
    <FormField
      label="Requires checks"
      optional
      error={error}
      helper="Checks a GitHub repository's branch requires before a delegate's merge into it or deploy from it stands, beside what its branch protection requires."
    >
      <div className="col gap-2">
        {value.map((entry, i) => (
          <div key={i} className="row gap-2 delegated-checks-entry">
            <Input
              aria-label={`Repository ${i + 1}`}
              value={entry.repo}
              placeholder="owner/name"
              autoComplete="off"
              spellCheck={false}
              disabled={disabled}
              onChange={(e) => put(i, { repo: e.target.value })}
            />
            <Input
              aria-label={`Branch ${i + 1}`}
              value={entry.branch}
              placeholder="main"
              autoComplete="off"
              spellCheck={false}
              disabled={disabled}
              onChange={(e) => put(i, { branch: e.target.value })}
            />
            <TagsInput
              aria-label={`Checks ${i + 1}`}
              value={entry.checks}
              onChange={(checks) => put(i, { checks })}
              disabled={disabled}
              submitKeys={["Enter"]}
              label={`Checks ${i + 1}`}
              placeholder="A check's name, then Enter"
            />
            <IconButton
              size="sm"
              variant="ghost"
              icon={<TrashGlyph size="sm" />}
              label={`Remove entry ${i + 1}`}
              disabled={disabled}
              onClick={() => onChange(value.filter((_, at) => at !== i))}
            />
          </div>
        ))}
        <div className="row">
          <Button
            type="button"
            size="small"
            variant="secondary"
            leadingIcon={<PlusGlyph size="sm" />}
            disabled={disabled}
            onClick={() => onChange([...value, { repo: "", branch: "", checks: [] }])}
          >
            Add a repository
          </Button>
        </div>
      </div>
    </FormField>
  );
}
