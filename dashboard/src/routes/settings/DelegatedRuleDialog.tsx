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
 * FOUR GROUPS: what, who decides, who hears about it, and a merge or deploy
 * rule's conditions. A kind another rule decides is held with its number.
 *
 * THE ENGINE JUDGES THE RULE. Which seats may delegate, who an escalation may
 * be put to, a panel on one model: each is refused by the dry run every save
 * makes first, in the engine's words, under the field it names.
 */

import { useEffect, useId, useMemo, useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  Combobox,
  FormField,
  IconButton,
  Input,
  Section,
  SegmentedControl,
  Skeleton,
  TagsInput,
  VisuallyHidden,
  type ComboboxOption,
  type TagsInputOption,
} from "@crewlethq/ui";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { PlusGlyph, ShieldCheckGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import { configRefusalWords } from "~/lib/configRefusal.ts";
import { kindMeaning, kindWords } from "~/lib/delegation.ts";
import {
  addRulePlan,
  DELEGABLE_KINDS,
  documentRules,
  draftOf,
  editRulePlan,
  MAX_DELEGATES,
  placeProblems,
  readDraft,
  releasesIn,
  SAMPLE_PRESETS,
  sampleWords,
  type DocumentRule,
  type RuleDraft,
  type RuleField,
  type SampleRange,
} from "~/lib/delegatedRules.ts";
import { handleLabel, useOrgIndex, type Seat } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { SettingWrite } from "~/lib/useSettingWrite.ts";
import { getConfig } from "~/protocol/configWrite.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { DelegatedRepository } from "~/contract/delegated.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";

/** Which rule the sheet is for. */
export type RuleTarget =
  { readonly kind: "add" } | { readonly kind: "edit"; readonly number: number };

/** What the sheet was filled from. */
type Filled =
  | { readonly kind: "reading" }
  | {
      readonly kind: "ready";
      /** The rule as the document writes it; null for a new rule. */
      readonly was: DocumentRule | null;
      /** Its 0-based place in the document's rules, as a refusal names it. */
      readonly index: number;
      /** Each kind another rule decides, by that rule's number. */
      readonly taken: ReadonlyMap<string, number>;
      /** The company's Gitlet repositories, which hold their own checks. */
      readonly gitlet: readonly string[];
    }
  | { readonly kind: "refused"; readonly message: string; readonly reload: boolean };

export const RULES_REREAD = "Reload to see the rules as they are now.";

export function DelegatedRuleDialog({
  target,
  write,
  sample,
  onClose,
  onReload,
}: {
  target: RuleTarget;
  /** The page's one write of the rules, which the sheet's Save drives. */
  write: SettingWrite;
  /** The spot-check rate's default and range, as the engine's read says them. */
  sample: SampleRange | undefined;
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
        const rules = documentRules(read.document);
        const index = number === 0 ? rules.length : number - 1;
        const taken = new Map<string, number>();
        rules.forEach((r, i) => {
          if (i !== index) for (const k of r.kinds) taken.set(k, i + 1);
        });
        const gitlet = gitletRepos(read.document);
        if (number === 0) {
          setFilled({ kind: "ready", was: null, index, taken, gitlet });
          return;
        }
        const was = rules[index];
        if (was === undefined) {
          setFilled({
            kind: "refused",
            message: `The company document no longer has a rule ${number}. ${RULES_REREAD}`,
            reload: true,
          });
          return;
        }
        setDraft(draftOf(was));
        setFilled({ kind: "ready", was, index, taken, gitlet });
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
    const read = readDraft(draft, sample);
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

  // THE ENGINE'S REFUSAL, each problem under the field it names.
  const refused =
    write.state.kind === "refused" && write.state.problems && filled.kind === "ready"
      ? placeProblems(write.state.problems, filled.index)
      : null;
  const errorOf = (field: RuleField) => errors[field] ?? refused?.fields[field];

  const releases = releasesIn(draft.kinds);
  const kindsId = useId();
  const title = target.kind === "add" ? "Add rule" : `Edit rule ${number}`;
  return (
    <>
      <Modal
        open
        variant="sheet"
        size="md"
        title={title}
        subtitle="An agent decides these requests first. What it can't decide goes to people."
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
          <Callout
            variant="danger"
            title="The rule could not be opened"
            action={
              filled.reload && (
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
              )
            }
          >
            {filled.message}
          </Callout>
        )}
        {filled.kind === "ready" && (
          <>
            <Section title="What it decides" spacing="tight" className="delegated-form-group">
              <fieldset
                className="col gap-2 delegated-kinds"
                aria-labelledby={kindsId}
                aria-describedby={errorOf("kinds") ? `${kindsId}-error` : undefined}
                disabled={write.busy}
              >
                <VisuallyHidden as="legend" id={kindsId}>
                  Requests it decides
                </VisuallyHidden>
                {DELEGABLE_KINDS.map((kind) => {
                  const other = filled.taken.get(kind);
                  return (
                    <Checkbox
                      key={kind}
                      label={kindWords(kind)}
                      description={
                        other ? `Rule ${other} already decides these.` : kindMeaning(kind)
                      }
                      checked={draft.kinds.includes(kind)}
                      disabled={other !== undefined && !draft.kinds.includes(kind)}
                      onCheckedChange={(on) =>
                        set({
                          kinds: on
                            ? DELEGABLE_KINDS.filter((k) => k === kind || draft.kinds.includes(k))
                            : draft.kinds.filter((k) => k !== kind),
                        })
                      }
                    />
                  );
                })}
                {errorOf("kinds") && (
                  <p id={`${kindsId}-error`} className="t-caption delegated-field-error">
                    {errorOf("kinds")}
                  </p>
                )}
              </fieldset>
            </Section>
            <Section title="Who decides" spacing="tight" className="delegated-form-group">
              <SeatsField
                label="Agent"
                helper="One agent, or two that must both allow."
                error={errorOf("delegates")}
                value={draft.delegates}
                options={agents}
                placeholder={
                  draft.delegates.length >= MAX_DELEGATES ? "Two agents chosen" : "Search agents"
                }
                disabled={write.busy}
                onChange={(delegates) => set({ delegates })}
              />
            </Section>
            <Section title="Who hears about it" spacing="tight" className="delegated-form-group">
              <SeatsField
                label="Escalates to"
                helper="People who decide what the agent can't. Admins always can."
                error={errorOf("escalateTo")}
                value={draft.escalateTo}
                options={people}
                placeholder="Search people"
                disabled={write.busy}
                onChange={(escalateTo) => set({ escalateTo })}
              />
              <SampleField
                value={draft.sample}
                range={sample}
                error={errorOf("sample")}
                disabled={write.busy}
                onChange={(next) => set({ sample: next })}
              />
            </Section>
            {releases && (
              <Section
                title="Conditions"
                description="For merges and deploys only."
                spacing="tight"
                className="delegated-form-group"
              >
                <ChecksField
                  value={draft.checks}
                  gitlet={filled.gitlet}
                  error={errorOf("checks")}
                  disabled={write.busy}
                  onChange={(checks) => set({ checks })}
                />
                <PathsField
                  value={draft.paths}
                  error={errorOf("paths")}
                  disabled={write.busy}
                  onChange={(paths) => set({ paths })}
                />
              </Section>
            )}
            <SettingWriteStatus
              write={write}
              unchanged="The rule already says that."
              onReload={onReload}
              refused={
                refused === null
                  ? undefined
                  : refused.rest.length > 0
                    ? refused.rest.join(" ")
                    : "Fix the fields marked above."
              }
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

/** The names of the Gitlet repositories the document declares (`code.repos`). */
function gitletRepos(document: CompanyDocument): string[] {
  const code = (document as Record<string, unknown>).code;
  const repos = code && typeof code === "object" ? (code as Record<string, unknown>).repos : null;
  if (!Array.isArray(repos)) return [];
  return repos
    .map((r) => (r && typeof r === "object" ? (r as Record<string, unknown>).name : null))
    .filter((n): n is string => typeof n === "string" && n !== "");
}

/** The seats of one kind as a picker's options: the face, the name, the handle. */
export function seatOptions(seats: readonly Seat[], kind: "agent" | "human"): TagsInputOption[] {
  return seats
    .filter((s) => s.kind === kind)
    .map((s) => ({
      value: s.handle,
      label: s.name || s.handle,
      description: handleLabel(s.handle),
      leading: (
        <SeatAvatar
          name={s.name || s.handle}
          kind={kind}
          avatar={s.avatar}
          size="text"
          decorative
        />
      ),
    }));
}

/**
 * Seats picked by handle, each chip its face, name and handle. A handle the
 * chart does not hold stays a chip of its own (`allowCustom`), so a rule
 * naming a seat this page cannot see is written back as it was.
 */
function SeatsField({
  label,
  helper,
  error,
  value,
  options,
  placeholder,
  disabled,
  onChange,
}: {
  label: string;
  helper: string;
  error: string | undefined;
  value: string[];
  options: TagsInputOption[];
  placeholder: string;
  disabled: boolean;
  onChange: (next: string[]) => void;
}) {
  return (
    <FormField label={label} helper={helper} error={error}>
      {(field) => (
        <TagsInput
          id={field.id}
          value={value}
          options={options}
          describeChips
          onChange={onChange}
          disabled={disabled}
          label={label}
          placeholder={placeholder}
          aria-describedby={field.describedBy}
          aria-invalid={field.invalid}
        />
      )}
    </FormField>
  );
}

/** The preset a rate is, or "custom". Empty is the engine's default. */
function presetOf(value: string, range: SampleRange | undefined): string {
  const rate = value.trim() === "" ? range?.default : Number(value);
  return rate !== undefined && SAMPLE_PRESETS.includes(rate) ? String(rate) : "custom";
}

/**
 * How often the escalation people review decisions: a preset or a number in
 * the engine's range. The default is written as no value.
 */
function SampleField({
  value,
  range,
  error,
  disabled,
  onChange,
}: {
  value: string;
  range: SampleRange | undefined;
  error: string | undefined;
  disabled: boolean;
  onChange: (next: string) => void;
}) {
  const [custom, setCustom] = useState(() => presetOf(value, range) === "custom");
  const chosen = custom ? "custom" : presetOf(value, range);
  const rate = value.trim() === "" ? range?.default : Number(value);
  const options = [
    ...SAMPLE_PRESETS.map((n) => ({
      value: String(n),
      label: n === 1 ? "Every decision" : `1 in ${n}`,
    })),
    { value: "custom", label: "Custom" },
  ];
  return (
    <FormField
      label="Spot-checks"
      helper={
        rate !== undefined && Number.isInteger(rate) && rate > 0
          ? sampleWords(rate)
          : "How often these people review a decision afterwards."
      }
      error={error}
    >
      {(field) => (
        <div className="col gap-2">
          <SegmentedControl
            label="Spot-checks"
            semantics="radio"
            layout="chips"
            options={options.map((o) => ({ ...o, disabled }))}
            value={chosen}
            onValueChange={(next) => {
              if (next === "custom") {
                setCustom(true);
                return;
              }
              setCustom(false);
              onChange(range !== undefined && Number(next) === range.default ? "" : next);
            }}
          />
          {chosen === "custom" && (
            <span className="row gap-2 delegated-sample-custom">
              <span className="t-body">1 in</span>
              <Input
                id={field.id}
                aria-label="Spot-check one decision in"
                aria-describedby={field.describedBy}
                value={value}
                inputMode="numeric"
                autoComplete="off"
                placeholder={range ? String(range.default) : undefined}
                error={Boolean(error)}
                disabled={disabled}
                onChange={(e) => onChange(e.target.value)}
                width="xs"
              />
              {range && <span className="t-caption">{`${range.min} to ${range.max}`}</span>}
            </span>
          )}
        </div>
      )}
    </FormField>
  );
}

function PathsField({
  value,
  error,
  disabled,
  onChange,
}: {
  value: string[];
  error: string | undefined;
  disabled: boolean;
  onChange: (next: string[]) => void;
}) {
  return (
    <FormField
      label="Always send to people"
      optional
      error={error}
      helper="A merge or deploy touching one of these paths goes to people, in any repository."
    >
      {(field) => (
        <TagsInput
          id={field.id}
          value={value}
          onChange={onChange}
          disabled={disabled}
          monospace
          submitKeys={["Enter"]}
          label="Always send to people"
          placeholder="A path such as billing/**, then Enter"
          aria-describedby={field.describedBy}
          aria-invalid={field.invalid}
        />
      )}
    </FormField>
  );
}

/**
 * The checks a rule requires per GitHub repository and branch, picked from the
 * company's GitHub or typed. Only Enter adds a check: a name may hold a comma.
 */
function ChecksField({
  value,
  gitlet,
  error,
  disabled,
  onChange,
}: {
  value: RuleDraft["checks"];
  gitlet: readonly string[];
  error: string | undefined;
  disabled: boolean;
  onChange: (next: RuleDraft["checks"]) => void;
}) {
  const repos = useQuery("delegated_repositories");
  const put = (i: number, patch: Partial<RuleDraft["checks"][number]>) =>
    onChange(value.map((entry, at) => (at === i ? { ...entry, ...patch } : entry)));
  const unread = repos.data?.unread;
  return (
    <FormField
      label="Required checks"
      optional
      error={error}
      helper={
        gitlet.length > 0
          ? "Checks that must pass on a GitHub branch before the agent's merge or deploy goes ahead. Gitlet repositories use their own merge gate."
          : "Checks that must pass on a GitHub branch before the agent's merge or deploy goes ahead."
      }
    >
      <div className="col gap-3">
        {repos.data && !repos.data.connected && (
          <p className="t-caption">The company has no GitHub connected, so type a repository.</p>
        )}
        {unread && <p className="t-caption">{`${unread} Type a repository instead.`}</p>}
        {value.map((entry, i) => (
          <ChecksEntry
            key={i}
            at={i}
            entry={entry}
            repos={repos.data?.repos ?? []}
            loading={repos.loading}
            gitlet={gitlet}
            disabled={disabled}
            onChange={(patch) => put(i, patch)}
            onRemove={() => onChange(value.filter((_, at) => at !== i))}
          />
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

/** Options narrowed to what is typed. */
function offered(names: readonly string[], typed: string): ComboboxOption[] {
  const needle = typed.trim().toLowerCase();
  return names
    .filter((n) => needle === "" || n.toLowerCase().includes(needle))
    .map((n) => ({ value: n, label: n }));
}

/** One required-checks entry: the repository, its branch and the checks, each searchable. */
function ChecksEntry({
  at,
  entry,
  repos,
  loading,
  gitlet,
  disabled,
  onChange,
  onRemove,
}: {
  at: number;
  entry: RuleDraft["checks"][number];
  repos: readonly DelegatedRepository[];
  loading: boolean;
  gitlet: readonly string[];
  disabled: boolean;
  onChange: (patch: Partial<RuleDraft["checks"][number]>) => void;
  onRemove: () => void;
}) {
  const n = at + 1;
  const [open, setOpen] = useState<"repo" | "branch" | null>(null);
  const repo = entry.repo.trim();
  const listed = repos.find((r) => r.repo.toLowerCase() === repo.toLowerCase());
  const branches = useQuery("delegated_branches", { repo }, { enabled: listed !== undefined });
  const branch = entry.branch.trim();
  const checks = useQuery(
    "delegated_checks",
    { repo, branch },
    { enabled: listed !== undefined && branch !== "" },
  );
  const repoOptions: ComboboxOption[] = [
    ...offered(
      repos.filter((r) => !r.archived).map((r) => r.repo),
      open === "repo" ? entry.repo : "",
    ),
    ...gitlet.map((name) => ({
      value: `gitlet:${name}`,
      label: name,
      hint: "Gitlet: its own merge gate",
      disabled: true,
    })),
  ];
  const branchNames = branches.data?.branches ?? (listed ? [listed.default_branch] : []);
  const suggested: TagsInputOption[] = (checks.data?.checks ?? []).map((c) => ({ value: c }));
  return (
    <div className="delegated-checks-entry">
      <div className="delegated-checks-target">
        <Combobox
          label={`Repositories for entry ${n}`}
          aria-label={`Repository ${n}`}
          value={entry.repo}
          placeholder="owner/name"
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
          mono
          open={open === "repo"}
          onOpenChange={(next) => setOpen(next ? "repo" : null)}
          onFocus={() => setOpen("repo")}
          onValueChange={(next) => {
            onChange({ repo: next });
            setOpen("repo");
          }}
          options={repoOptions}
          emptyMessage={loading ? "Reading repositories…" : "Not listed. Type it as owner/name."}
          onCommit={(option) => {
            const pick = repos.find((r) => r.repo === option.value);
            onChange({
              repo: option.value,
              ...(entry.branch.trim() === "" && pick ? { branch: pick.default_branch } : {}),
            });
            setOpen(null);
          }}
        />
        <Combobox
          label={`Branches of ${repo || `repository ${n}`}`}
          aria-label={`Branch ${n}`}
          value={entry.branch}
          placeholder="main"
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
          mono
          open={open === "branch"}
          onOpenChange={(next) => setOpen(next ? "branch" : null)}
          onFocus={() => setOpen("branch")}
          onValueChange={(next) => {
            onChange({ branch: next });
            setOpen("branch");
          }}
          options={offered(branchNames, open === "branch" ? entry.branch : "")}
          emptyMessage={
            branches.loading
              ? "Reading branches…"
              : (branches.data?.unread ?? "Not listed. Type the branch's name.")
          }
          onCommit={(option) => {
            onChange({ branch: option.value });
            setOpen(null);
          }}
        />
        <IconButton
          size="sm"
          variant="ghost"
          icon={<TrashGlyph size="sm" />}
          label={`Remove repository ${n}`}
          disabled={disabled}
          onClick={onRemove}
        />
      </div>
      <TagsInput
        aria-label={`Checks ${n}`}
        value={entry.checks}
        options={suggested.length > 0 ? suggested : undefined}
        onChange={(next) => onChange({ checks: next })}
        disabled={disabled}
        submitKeys={["Enter"]}
        label={`Checks ${n}`}
        placeholder={
          suggested.length > 0
            ? "Pick a check, or type one and press Enter"
            : "A check's name, then Enter"
        }
        emptyMessage={(q) => `Press Enter to add “${q}”`}
      />
    </div>
  );
}
