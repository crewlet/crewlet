/**
 * Add an MCP server, or edit one, step by step: where it comes from, its
 * name, how it connects, how it signs in, who gets it, the level each of its
 * tools runs at, a test, and a review the engine checks before anything is
 * stored. The engine's problems land on the fields
 * they are about, each step that holds one marked on the stepper.
 *
 * A PAGE OF ITS OWN, in the list's place, so its steps have the room; adding
 * and editing are the same wizard, an edit opened filled in at the step its
 * action is about (`?flow=edit&step=`), its name kept.
 *
 * THE COMMIT ORDER IS ENTRY THEN VALUES. The entry is written as one revision
 * (create-only for a new name, against its own revision for an edit), then
 * each value typed for it is stored write-only through the values route,
 * which tests the server with it first, seals it and points the key at it.
 * A value the server refuses stores nothing, and the wizard stays open on it
 * with the entry already saved, so a second Save sends only what is left.
 *
 * NO WAIT BETWEEN THE TWO. The values route tests a value with its entry as
 * the fleet's active revision states it, sent with the test, so a value
 * stored straight after the entry's write needs no node to have applied it.
 *
 * THE TEST IS THE ENGINE'S: the draft as a write would leave it, with the
 * values typed for it read through the variables they will be stored under
 * and sealed across the fleet (`POST /mcp-checks`); an edit that changes
 * nothing of the entry tests the saved server instead. It is sent without
 * the draft's levels, which decide how a call runs and never what the server
 * lists, so the approvals step is handed every tool, a blocked one included,
 * each with the level the engine starts it at.
 *
 * ENTER MOVES ON from a single-line field, a choice or a step's heading; a
 * multi-line field, a button and an open list keep Enter for themselves (the
 * Models wizard's keyboard).
 */

import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { Button, ConfirmModal, SetupWizard, useToast, type StepperStep } from "@crewlethq/ui";
import { readOnlyFrom, useAssistantDraft, useDraftState } from "~/app/assistant/draft.ts";
import type {
  McpAuthMethod,
  McpCatalogueEntry,
  McpCheckAnswer,
  McpMemberServerStatus,
  McpValueRefusal,
  McpValueState,
} from "~/contract/mcp.ts";
import { listed } from "~/lib/format.ts";
import { useAccess } from "~/routes/settings/integrations/access/api.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { findOpener, firstInvalid, focusWhenShown, openerNow } from "~/lib/wizardFocus.ts";
import {
  createEntity,
  dryRunCreate,
  dryRunEntity,
  getEntity,
  putEntity,
  type ConfigWriteOutcome,
} from "~/protocol/configWrite.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ConfigProblem } from "~/protocol/types.ts";
import { storeValue, testDraft, testServer, type McpAnswer } from "../api.ts";
import type { McpData } from "../data.ts";
import { writeRefusalWords } from "../words.ts";
import { SERVER_DRAFT_FIELDS, SERVER_DRAFT_HELP } from "./assistantDraft.ts";
import {
  emptyDraft,
  entityOf,
  fromServer,
  stepOf,
  stepsFor,
  testsValue,
  valuesOf,
  type Draft,
  type Step,
} from "./draft.ts";
import { SourceStep } from "./SourceStep.tsx";
import {
  AccessStep,
  ApprovalsStep,
  ConnectionStep,
  isComplete,
  missingIn,
  NameStep,
  ReviewStep,
  SignInStep,
  TestStep,
  testersOf,
  type ReviewCheck,
  type TestState,
} from "./steps.tsx";

const LABELS: Record<Step, string> = {
  source: "Source",
  name: "Name",
  connection: "Connection",
  signin: "Sign-in",
  access: "Who gets it",
  approvals: "Tool approvals",
  test: "Test",
  review: "Review",
};

const LEADS: Record<Step, string> = {
  source:
    "Start from a server the catalogue knows, an address, a pasted configuration, or a community listing.",
  name: "How the agents and this page name it.",
  connection: "Where the server is, or the command that starts it.",
  signin: "How the engine proves who it is to the server.",
  access: "One connection for the company or one per agent, and which agents get it.",
  approvals: "Whether each tool runs, asks a person first, or is blocked.",
  test: "Ask the server who it is and list its tools. No tool is called.",
  review: "Everything this saves. The engine checks it first.",
};

/**
 * A server as it is saved, for an edit: its entry as read and what it is
 * sent. A write re-reads the entry's tag as it is sent, so it is decided
 * against the revision it lands on.
 */
export interface Saved {
  server: McpMemberServerStatus;
  entity: Record<string, unknown>;
  values: readonly McpValueState[];
  entry: McpCatalogueEntry | null;
  signIn: McpAuthMethod;
}

/** A JSON value with its object keys in order, so two spellings of one entry compare equal. */
function canonical(v: unknown): string {
  return JSON.stringify(v, (_k, x: unknown) =>
    typeof x === "object" && x !== null && !Array.isArray(x)
      ? Object.fromEntries(
          Object.entries(x as Record<string, unknown>).sort(([a], [b]) => a.localeCompare(b)),
        )
      : x,
  );
}

/** The problems a refusal's body carried, in the engine's shape. */
function problemsIn(body: Record<string, unknown>): ConfigProblem[] {
  return Array.isArray(body.problems)
    ? (body.problems.filter(
        (p) => typeof p === "object" && p !== null && typeof p.message === "string",
      ) as ConfigProblem[])
    : [];
}

/** A write is sent without a signal: started, it runs to its answer whatever the screen does. */
const unbounded = () => new AbortController().signal;

export function ServerWizard({
  data,
  saved,
  start,
  onClose,
  onSaved,
}: {
  data: McpData;
  /** The server to edit; null adds one. */
  saved: Saved | null;
  /** The step the address names, or null for the first. */
  start: Step | null;
  onClose: () => void;
  /** After a save: the server's name, and whether a person signs it in next (a new OAuth sign-in). */
  onSaved: (name: string, signInNext: boolean) => void;
}) {
  const toast = useToast();
  const lifetime = useLifetimeSignal();
  const editing = saved !== null;
  const steps = stepsFor(editing);
  // THE CONTROL THAT OPENED THE WIZARD is read while its page is still on screen.
  const [opener] = useState(openerNow);
  const [initial] = useState<Draft>(() =>
    saved ? fromServer(saved.entity, saved.values, saved.entry, saved.signIn) : emptyDraft(),
  );
  const [draft, setDraft] = useDraftState<Draft>(initial);
  const [step, setStep] = useState<Step>(start && steps.includes(start) ? start : steps[0]!);
  const [reached, setReached] = useState<Step>(editing ? "review" : step);
  const [attempted, setAttempted] = useState<ReadonlySet<Step>>(new Set());
  const [problems, setProblems] = useState<ConfigProblem[]>([]);
  const [test, setTest] = useState<TestState>({ kind: "idle" });
  const [check, setCheck] = useState<ReviewCheck>({ kind: "idle" });
  const [saving, setSaving] = useState(false);
  const [discarding, setDiscarding] = useState(false);
  /** A save that wrote the entry and stopped at a value: what is left, in a sentence. */
  const [partial, setPartial] = useState<string | null>(null);
  /** The entry as last written by this wizard, and the tag it was written against. */
  const [written, setWritten] = useState<{ entity: string } | null>(null);
  /** The values stored by this wizard, by key, so a second Save sends only the rest. */
  const [storedFor, setStoredFor] = useState<Readonly<Record<string, string>>>({});
  const reach = saved ? (data.reaches(saved.server.name) ?? []) : [];
  const [seat, setSeat] = useState(() => reach[0]?.handle ?? data.agents[0]?.handle ?? "");
  // A SERVER AN INTEGRATION SIGNS IN is tested only as an agent the integration
  // has turned on: as any other, the engine refuses it, so the test could only
  // fail. The seat moves to the first of them, and nothing runs while none is.
  const bound = draft.signIn === "integration" ? draft.catalogue?.integration || null : null;
  const access = useAccess(bound);
  const testers = useMemo(
    () => testersOf(data.agents, bound, access),
    // `access` is a new object each render; what it says is its data and error.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [data.agents, bound, access.data, access.error],
  );
  useEffect(() => {
    if (draft.shared || !testers || testers.length === 0) return;
    if (!testers.some((s) => s.handle === seat)) setSeat(testers[0]!.handle);
  }, [draft.shared, testers, seat]);
  const testable = draft.shared || (testers?.some((s) => s.handle === seat) ?? false);
  const root = useRef<HTMLDivElement>(null);
  const inflight = useRef<AbortController | null>(null);

  useAssistantDraft<Draft>({
    kind: "client",
    id: saved?.server.name ?? null,
    step,
    help: SERVER_DRAFT_HELP,
    scope: saved
      ? `The changes to the MCP server ${saved.server.name} (not saved)`
      : "The MCP server you are adding (not saved)",
    value: draft,
    fields: SERVER_DRAFT_FIELDS,
    write: readOnlyFrom(setDraft),
  });

  const entity = useMemo(() => entityOf(draft, saved?.entity ?? null), [draft, saved]);
  // WHAT THE TEST IS SENT: the entry without its levels (see above).
  const tested = useMemo(() => {
    const { tools: _levels, ...rest } = entity;
    return rest;
  }, [entity]);
  const values = useMemo(() => valuesOf(draft), [draft]);
  const unchanged = saved !== null && canonical(entity) === canonical(saved.entity);
  // THE SAVED SERVER, as it runs, for an edit that changes nothing of it.
  const testsSaved = unchanged && Object.keys(values).length === 0;
  const testSeat = draft.shared ? "" : seat;
  const signature = JSON.stringify({ entity: tested, values, seat: testSeat });

  const set = useCallback((patch: Partial<Draft>) => {
    setDraft((d) => ({ ...d, ...patch }));
    setProblems([]);
    setCheck({ kind: "idle" });
  }, []);

  // FOCUS ON OPEN: the step's heading.
  useEffect(() => {
    focusWhenShown(
      () => root.current?.querySelector<HTMLElement>(".crewlet-setup-wizard__heading") ?? null,
    );
    return () => inflight.current?.abort();
  }, []);

  /** The test: the saved server for an edit that changes nothing of it, the draft otherwise. */
  async function runTest() {
    const sig = signature;
    setTest({ kind: "running", for: sig });
    const answer: McpAnswer<McpCheckAnswer> = testsSaved
      ? await testServer(draft.name, testSeat)
      : await testDraft(tested, values, testSeat);
    if (answer.kind === "ok") {
      setTest({ kind: "done", for: sig, answer: answer.value });
      // THE TOOLS IT LISTED, for the approvals step: a change of levels is
      // the draft's own, so it clears no problem and no review. A tool not
      // offered under its name takes no level (the test says why).
      const listed = answer.value.tools.filter((t) => !t.refused);
      if (listed.length > 0) setDraft((d) => ({ ...d, listed }));
      return;
    }
    const found = problemsIn(answer.body);
    if (found.length > 0) setProblems(found);
    setTest({
      kind: "refused",
      for: sig,
      message:
        found.length > 0
          ? `${answer.sentence} Correct what is marked and test again.`
          : answer.sentence,
    });
  }

  /** Review's check: the engine's dry run of the entry, as the write would make it. */
  async function runCheck() {
    inflight.current?.abort();
    const ctrl = new AbortController();
    inflight.current = ctrl;
    setCheck({ kind: "checking" });
    try {
      const outcome =
        saved || written
          ? await (async (): Promise<ConfigWriteOutcome | { kind: "missing" }> => {
              const read = await getEntity("mcp-servers", draft.name.trim(), ctrl.signal);
              if (read.kind !== "entity") return read;
              return dryRunEntity("mcp-servers", draft.name.trim(), entity, read.etag, ctrl.signal);
            })()
          : await dryRunCreate("mcp-servers", draft.name.trim(), entity, ctrl.signal);
      settleCheck(outcome, "check");
    } catch (err) {
      if (!isAbort(err)) throw err;
    }
  }

  /** What a write or its dry run came to, on the review. */
  function settleCheck(
    outcome: ConfigWriteOutcome | { kind: "missing" },
    stage: "check" | "write",
  ) {
    if (outcome.kind === "valid" || outcome.kind === "saved") {
      setCheck({ kind: "accepted" });
    } else if (outcome.kind === "problems") {
      setProblems([...outcome.problems]);
      setCheck({ kind: "problems" });
    } else {
      setCheck({ kind: "blocked", message: writeRefusalWords(outcome, draft.name, stage) });
    }
  }

  // REVIEW IS CHECKED whenever it is opened; a change to the draft clears the answer.
  useEffect(() => {
    if (step === "review" && check.kind === "idle") void runCheck();
    // `runCheck` reads the draft as it stands when Review opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, check.kind]);

  // THE TEST RUNS AS ITS STEP OPENS, once for each version of the draft, and
  // as the approvals step opens with no tools listed yet, to list them.
  useEffect(() => {
    if (step !== "test" && !(step === "approvals" && draft.listed.length === 0)) return;
    if (test.kind !== "idle" && test.for === signature) return;
    if (!testable) return;
    void runTest();
    // `runTest` reads the draft as it stands when the step opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, signature, testable]);

  async function save() {
    const screen = lifetime();
    const name = draft.name.trim();
    setSaving(true);
    try {
      // THE ENTRY FIRST, unless this wizard has written it as it stands, or an edit leaves it as it was.
      const entitySig = canonical(entity);
      if (written?.entity !== entitySig && !unchanged) {
        let outcome: ConfigWriteOutcome | { kind: "missing" };
        if (saved || written) {
          const read = await getEntity("mcp-servers", name, unbounded());
          outcome =
            read.kind === "entity"
              ? await putEntity(
                  "mcp-servers",
                  name,
                  entity,
                  read.etag,
                  `Edited the MCP server ${name}`,
                  unbounded(),
                )
              : read;
        } else {
          outcome = await createEntity(
            "mcp-servers",
            name,
            entity,
            `Added the MCP server ${name}`,
            unbounded(),
          );
        }
        if (screen.aborted) return;
        if (outcome.kind !== "saved") {
          settleCheck(outcome, "write");
          return;
        }
        setWritten({ entity: entitySig });
      }
      // THEN EACH VALUE, tested and sealed. A per-agent server's own value is
      // tested as each agent, so it is stored untested; the Test step tried it.
      const stored: Record<string, string> = { ...storedFor };
      for (const [key, value] of Object.entries(values)) {
        if (stored[key] === value) continue;
        const answer = await storeValue(name, key, value, { test: testsValue(draft, key) });
        if (screen.aborted) return;
        if (answer.kind === "ok") {
          stored[key] = value;
          setStoredFor({ ...stored });
          continue;
        }
        const refused = answer.body as Partial<McpValueRefusal>;
        const found = problemsIn(answer.body);
        setProblems(found);
        if (refused.check) setTest({ kind: "done", for: signature, answer: refused.check });
        const left = Object.keys(values).filter((k) => stored[k] !== values[k]);
        setPartial(
          `${name} is ${saved ? "saved" : "added"}, and ${listed(left)} ${left.length === 1 ? "is" : "are"} not stored yet. ${answer.sentence}`,
        );
        return;
      }
      setPartial(null);
      // A NEW SERVER A PERSON SIGNS IN TO starts once somebody does, which is next.
      const signInNext = !saved && draft.signIn === "oauth";
      toast.show({
        variant: "success",
        title: saved ? `${name} saved` : `${name} added`,
        message: saved
          ? "It applies with no restart; a running turn finishes first."
          : signInNext
            ? "Sign in to it next: no agent reaches its tools until somebody does."
            : "It starts with no restart, and the agents it reaches see its tools on their next turn.",
      });
      onSaved(name, signInNext);
    } finally {
      if (!screen.aborted) setSaving(false);
    }
  }

  const go = (id: Step) => {
    // REVIEW LISTS EVERYTHING, so reaching it is acting on every step.
    if (id === "review") setAttempted(new Set(steps));
    setStep(id);
    if (steps.indexOf(id) > steps.indexOf(reached)) setReached(id);
  };
  const at = steps.indexOf(step);
  const next = steps[at + 1];
  const back = steps[at - 1];
  const missing = missingIn(draft, step);
  const shownMissing = attempted.has(step) ? missing : {};
  const sourceGap =
    step !== "source"
      ? null
      : draft.catalogue
        ? null
        : draft.source === "catalogue" || draft.source === "community"
          ? "Choose a server, or start from an address."
          : draft.source === "url" && !draft.url.trim()
            ? "Give the server's address."
            : draft.source === "json" && !draft.url.trim() && !draft.command.trim()
              ? "Paste a configuration that names a server."
              : null;

  /** On to the next step when this one has what it needs; otherwise say what, and focus it. */
  function advance() {
    if (step === "review") {
      if (check.kind === "accepted") void save();
      else if (check.kind === "idle") void runCheck();
      return;
    }
    if (sourceGap) {
      toast.show({ variant: "warning", title: sourceGap });
      return;
    }
    if (!isComplete(missing)) {
      setAttempted((a) => new Set([...a, step]));
      focusWhenShown(() => firstInvalid(root.current));
      return;
    }
    if (next) go(next);
  }

  const close = () => {
    onClose();
    focusWhenShown(() => findOpener(opener));
  };
  const dirty = JSON.stringify(draft) !== JSON.stringify(initial);
  const cancel = () => (dirty && !written ? setDiscarding(true) : close());

  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
    const el = event.target as HTMLElement;
    if (el instanceof HTMLTextAreaElement || el instanceof HTMLButtonElement) return;
    if (el.closest("[role='listbox'], [role='menu'], [aria-expanded='true'], [role='dialog']"))
      return;
    if (el.closest("form")) return;
    if (
      el instanceof HTMLInputElement ||
      /^H[1-6]$/.test(el.tagName) ||
      el.getAttribute("role") === "radio"
    ) {
      event.preventDefault();
      advance();
    }
  }

  const attention = new Set(problems.map((p) => stepOf(p, draft.transport)));
  const stepList: StepperStep[] = steps.map((id) => ({
    id,
    label: LABELS[id],
    ...(attention.has(id) && id !== step ? { status: "attention" as const } : {}),
  }));
  const reviewing = step === "review";
  const checking = check.kind === "checking";
  const testRow = (
    <span className="mcp-test-row">
      <span>
        {test.kind === "running"
          ? "Running"
          : test.kind === "done"
            ? test.answer.ok
              ? `Passed with ${test.answer.tools.length} ${test.answer.tools.length === 1 ? "tool" : "tools"}`
              : "Failed"
            : test.kind === "refused"
              ? "Did not run"
              : "Not run"}
        {test.kind !== "idle" && test.kind !== "running" && test.for !== signature
          ? ", before the last change"
          : ""}
      </span>
      {test.kind === "done" && test.answer.ok && test.for === signature ? null : (
        <button type="button" className="t-link" onClick={() => go("test")}>
          {test.kind === "done" || test.kind === "refused" ? "See why" : "Run the test"}
        </button>
      )}
    </span>
  );
  const primary = next ? (
    <Button variant="primary" onClick={advance}>
      Continue
    </Button>
  ) : (
    <Button
      variant="primary"
      onClick={advance}
      loading={saving || checking}
      disabled={check.kind === "problems" || check.kind === "blocked"}
    >
      {checking ? "Checking…" : saved ? "Save changes" : "Add server"}
    </Button>
  );

  return (
    // The wizard's keyboard: Enter moves on (see onKeyDown). Laid out as if absent.
    <div className="mcp-keys" ref={root} onKeyDown={onKeyDown}>
      <SetupWizard
        title={saved ? `Edit ${saved.server.title}` : "Add an MCP server"}
        footerStart={
          <Button variant="ghost" onClick={cancel}>
            Cancel
          </Button>
        }
        steps={stepList}
        current={step}
        reached={reached}
        onSelect={(id) => go(id as Step)}
        stepsLabel={saved ? `Steps to edit ${saved.server.title}` : "Steps to add an MCP server"}
        heading={LABELS[step]}
        lead={LEADS[step]}
        wide={step === "source" || step === "signin" || reviewing}
        footer={
          <>
            {/* BACK IS ALWAYS IN THE ROW, disabled on the first step, so the row keeps one shape. */}
            <Button variant="secondary" disabled={!back} onClick={() => back && go(back)}>
              Back
            </Button>
            {step === "test" ? (
              <Button
                variant="secondary"
                disabled={test.kind === "running" || (!draft.shared && !seat)}
                onClick={() => void runTest()}
              >
                {test.kind === "idle" ? "Run the test" : "Test again"}
              </Button>
            ) : null}
            {saved && !reviewing ? (
              <Button variant="secondary" onClick={() => go("review")}>
                Review changes
              </Button>
            ) : null}
          </>
        }
        primaryAction={primary}
      >
        <div className="mcp-stack">
          {step === "source" ? <SourceStep draft={draft} set={set} servers={data.servers} /> : null}
          {step === "name" ? (
            <NameStep
              draft={draft}
              set={set}
              editing={editing}
              problems={problems}
              missing={shownMissing}
            />
          ) : null}
          {step === "connection" ? (
            <ConnectionStep draft={draft} set={set} problems={problems} missing={shownMissing} />
          ) : null}
          {step === "signin" ? (
            <SignInStep
              draft={draft}
              set={set}
              problems={problems}
              missing={shownMissing}
              editing={editing}
              callbackUrl={
                data.answer && "callback_url" in data.answer ? data.answer.callback_url : ""
              }
            />
          ) : null}
          {step === "access" ? <AccessStep draft={draft} set={set} /> : null}
          {step === "approvals" ? (
            <ApprovalsStep
              draft={draft}
              set={set}
              listing={test.kind === "running"}
              onList={() => void runTest()}
            />
          ) : null}
          {step === "test" ? (
            <TestStep
              draft={draft}
              test={test}
              current={signature}
              agents={testers}
              seat={seat}
              onSeat={setSeat}
              ofDraft={!testsSaved}
            />
          ) : null}
          {reviewing ? (
            <ReviewStep
              draft={draft}
              problems={problems}
              check={check}
              testRow={testRow}
              editing={editing}
              partial={partial}
            />
          ) : null}
        </div>
      </SetupWizard>
      <ConfirmModal
        open={discarding}
        onClose={() => setDiscarding(false)}
        onConfirm={() => {
          setDiscarding(false);
          close();
        }}
        title={saved ? "Discard these changes?" : "Discard this server?"}
        message="Nothing has been saved."
        confirmLabel="Discard"
        cancelLabel="Keep editing"
        destructive
      />
    </div>
  );
}
