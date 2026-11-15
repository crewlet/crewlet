/**
 * The three writes Settings › Configuration makes of the whole company
 * document: a revert to an earlier revision, a reload of the active one, and
 * an edit (or an import) of the document as YAML. Each is a new revision with
 * the summary the person writes, and each says the engine's own refusal in its
 * words (`lib/configRefusal.ts`, over `protocol/configAnswer.ts`'s reading).
 *
 * # Asked first, in the page
 *
 * A revert and a reload change what every node runs, so each opens a
 * confirmation before anything is sent: a revert shows the diff it would
 * apply (the revision against the active one, `config_diff`), and both take
 * the summary the history shows for them. The two routes read no body, so the
 * summary travels in `X-Summary`, as its UTF-8 bytes (`rest.headerBytes`).
 *
 * # The whole document, as YAML
 *
 * The editor reads `GET /config?format=yaml` with the entity tag of the
 * revision it is, checks every draft with `PUT /config?dry_run=true` under
 * that tag, and saves under it, so a colleague's save in between is a
 * conflict to reload rather than a document written over theirs.
 *
 * CREDENTIALS ARE SAFE TO SEND BACK MASKED. The read masks every credential
 * (`__redacted__`), and the engine restores each mask from the revision the
 * write is conditional on before it validates, matching every member by its
 * identity (a seat by handle, a unit by key, an MCP server and a setup step by
 * name), never by position (`config.Company.RestoreRedacted`). A mask it
 * cannot match to exactly one member stays standing and the write is refused
 * naming the field, so no edit, reorder or rename silently clears a secret: a
 * credential changes only where a person writes a new value or a `${VAR}`.
 *
 * AN UNSEALED ACTIVE REVISION can't be read at all, and a whole document is
 * the one write that replaces it (it reads nothing from it, so no mask is
 * restored): the editor opens empty for that document, conditional on the
 * revision the refusal named.
 */

import { useEffect, useId, useMemo, useRef, useState } from "react";
import {
  Button,
  Callout,
  Card,
  FilePickButton,
  FormField,
  InlineCode,
  Input,
  Skeleton,
  SourceEditor,
  useToast,
  type PickedFile,
  type SourceEditorHandle,
  type SourceMark,
} from "@crewlethq/ui";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import {
  FileCodeGlyph,
  RefreshCwGlyph,
  RotateCcwGlyph,
  UploadGlyph,
} from "@crewlethq/icons/glyphs";
import {
  refuse,
  useAssistantDraft,
  useDraftState,
  writeParts,
  type DraftFieldSpec,
} from "~/app/assistant/draft.ts";
import { useLeaveGuard } from "~/app/router.tsx";
import { DiffView } from "~/components/DiffView.tsx";
import { QueryState } from "~/components/common.tsx";
import { configHunks } from "~/lib/configHunks.ts";
import { configRefusalWords, type RefusalWords } from "~/lib/configRefusal.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { isUnsealedRefusal } from "~/protocol/configAnswer.ts";
import {
  dryRunDocument,
  getConfigText,
  reloadConfig,
  revertRevision,
  revisionTag,
  saveDocument,
  type ConfigCheckOutcome,
} from "~/protocol/configWrite.ts";
import type { RevisionMeta } from "~/protocol/index.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ConfigProblem, ConfigWarning } from "~/protocol/types.ts";

/** How a conflict ends on this screen. */
const REREAD = "Reload to see the configuration as it is now.";

/** A refused write as the dialog draws it. */
function Refused({ words, onReload }: { words: RefusalWords; onReload?: () => void }) {
  return (
    <Callout variant="danger" title="Nothing was saved" role="alert">
      <span>{words.message}</span>
      {words.reload && onReload && (
        <div className="row gap-2">
          <Button size="small" variant="secondary" onClick={onReload}>
            Reload
          </Button>
        </div>
      )}
    </Callout>
  );
}

/** What one of the two summary-only writes came to. */
type Sent =
  | { readonly kind: "idle" }
  | { readonly kind: "sending" }
  | { readonly kind: "refused"; readonly words: RefusalWords };

/**
 * The words of an outcome that is not a stored revision. A `200` to a write
 * is a check the engine took for one, which stored nothing.
 */
function notSaved(outcome: Exclude<ConfigCheckOutcome, { kind: "saved" }>): RefusalWords {
  if (outcome.kind === "valid") {
    return {
      message: "The engine checked the change but stored nothing. Try again.",
      reload: false,
    };
  }
  return configRefusalWords(outcome, REREAD);
}

/** The summary field every write here carries: required, as the history needs it. */
function SummaryField({
  value,
  error,
  placeholder,
  disabled,
  onChange,
}: {
  value: string;
  error: string | undefined;
  placeholder: string;
  disabled: boolean;
  onChange: (next: string) => void;
}) {
  const id = useId();
  return (
    <FormField
      htmlFor={id}
      label="Summary"
      error={error}
      helper="What the revision history shows for it: why, in one line."
    >
      <Input
        id={id}
        value={value}
        placeholder={placeholder}
        autoComplete="off"
        disabled={disabled}
        error={Boolean(error)}
        onChange={(e) => onChange(e.target.value)}
      />
    </FormField>
  );
}

/** Why a typed summary cannot be sent, or undefined. */
function summaryError(summary: string): string | undefined {
  return summary.trim() === "" ? "Say why, in one line: the history shows it." : undefined;
}

/** A stored revision, said once the write is answered. */
export interface Stored {
  readonly revisionId: string;
  readonly epoch: number;
}

/**
 * Re-activating an earlier revision, asked first with the diff it would
 * apply: the revision against the active one is exactly what the revert
 * changes.
 */
export function RevertDialog({
  revision,
  onClose,
  onStored,
}: {
  revision: RevisionMeta;
  onClose: () => void;
  /** Called once the revert is stored, with the new revision. */
  onStored: (stored: Stored) => void;
}) {
  const id = revision.revision_id;
  const diff = useQuery("config_diff", { revision_id: id, against: "active" });
  const hunks = useMemo(() => configHunks(diff.data?.changes ?? []), [diff.data?.changes]);
  const [summary, setSummary] = useState("");
  const [error, setError] = useState<string | undefined>();
  const [sent, setSent] = useState<Sent>({ kind: "idle" });
  const inflight = useRef<AbortController | null>(null);
  useEffect(() => () => inflight.current?.abort(), []);
  const busy = sent.kind === "sending";

  async function revert() {
    if (busy) return;
    const problem = summaryError(summary);
    if (problem) {
      setError(problem);
      return;
    }
    inflight.current = new AbortController();
    setSent({ kind: "sending" });
    try {
      const outcome = await revertRevision(id, summary, inflight.current.signal);
      if (outcome.kind === "saved") {
        onStored({ revisionId: outcome.revisionId, epoch: outcome.epoch });
        return;
      }
      setSent({ kind: "refused", words: notSaved(outcome) });
    } catch (err) {
      if (!isAbort(err)) setSent({ kind: "refused", words: failedWords(err) });
    }
  }

  const short = id.slice(0, 10);
  // THE REVISION BY WHEN IT WAS WRITTEN, which the history lists it by: a
  // clipped id ("1bc1f4ea-9") names nothing a person has seen.
  const made = fmtDateTime(revision.created_at);
  const total = diff.data?.changes_total ?? 0;
  const shown = diff.data?.changes.length ?? 0;
  return (
    <Modal
      open
      size="lg"
      title={`Revert to the revision from ${made}?`}
      subtitle={revision.summary || "No summary was written"}
      icon={<RotateCcwGlyph size="sm" />}
      onClose={onClose}
      onSubmit={() => void revert()}
      dismissable={!busy}
      stackBody
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" loading={busy}>
            Revert
          </Button>
        </>
      }
    >
      <p className="t-body" style={{ margin: 0 }}>
        The company becomes this revision&rsquo;s document again, stored as a new revision that
        every node applies. The history keeps every revision, so a revert can itself be reverted.
      </p>
      <section className="col gap-2" aria-labelledby="revert-changes">
        <div className="t-label" id="revert-changes">
          What it changes in the active revision
        </div>
        {diff.loading && !diff.data ? (
          <Skeleton variant="text" rows={4} label="Loading" />
        ) : (
          <QueryState
            error={diff.error}
            refusal={diff.refusal}
            detail={diff.detail}
            loading={diff.loading}
            empty={
              shown > 0
                ? undefined
                : {
                    title: "No differences",
                    hint: "The active revision already holds this document: the revert re-publishes it as a reload would.",
                  }
            }
          >
            {/* BOUNDED, so the sentence above it and the summary below it
                stay on screen however much the revert changes. */}
            <div className="config-revert-diff">
              <DiffView hunks={hunks} aria-label={`What reverting to ${short} changes`} />
            </div>
            {total > shown && (
              <div className="t-caption">
                {shown} of {total} shown. <InlineCode>crewlet config diff</InlineCode> shows all.
              </div>
            )}
          </QueryState>
        )}
      </section>
      <SummaryField
        value={summary}
        error={error}
        placeholder={`Revert to ${short}: why`}
        disabled={busy}
        onChange={(next) => {
          setSummary(next);
          setError(undefined);
          if (sent.kind === "refused") setSent({ kind: "idle" });
        }}
      />
      {sent.kind === "refused" && <Refused words={sent.words} />}
    </Modal>
  );
}

/**
 * Re-publishing the active document unchanged: every node applies it again,
 * re-reading the secret store, which is how a rotated credential reaches the
 * company. Open on a managed document too (`useConfigReloadAccess`).
 */
export function ReloadDialog({
  onClose,
  onStored,
}: {
  onClose: () => void;
  onStored: (stored: Stored) => void;
}) {
  const [summary, setSummary] = useState("");
  const [error, setError] = useState<string | undefined>();
  const [sent, setSent] = useState<Sent>({ kind: "idle" });
  const inflight = useRef<AbortController | null>(null);
  useEffect(() => () => inflight.current?.abort(), []);
  const busy = sent.kind === "sending";

  async function reload() {
    if (busy) return;
    const problem = summaryError(summary);
    if (problem) {
      setError(problem);
      return;
    }
    inflight.current = new AbortController();
    setSent({ kind: "sending" });
    try {
      const outcome = await reloadConfig(summary, inflight.current.signal);
      if (outcome.kind === "saved") {
        onStored({ revisionId: outcome.revisionId, epoch: outcome.epoch });
        return;
      }
      setSent({ kind: "refused", words: notSaved(outcome) });
    } catch (err) {
      if (!isAbort(err)) setSent({ kind: "refused", words: failedWords(err) });
    }
  }

  return (
    <Modal
      open
      size="md"
      title="Reload the configuration?"
      icon={<RefreshCwGlyph size="sm" />}
      onClose={onClose}
      onSubmit={() => void reload()}
      dismissable={!busy}
      stackBody
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" loading={busy}>
            Reload
          </Button>
        </>
      }
    >
      <p className="t-body" style={{ margin: 0 }}>
        The active document is published again, unchanged, as a new revision. Every node applies it
        and reads the secret store again, which is how a credential rotated there reaches the
        company.
      </p>
      <SummaryField
        value={summary}
        error={error}
        placeholder="Rotate the leaked GitLab token"
        disabled={busy}
        onChange={(next) => {
          setSummary(next);
          setError(undefined);
          if (sent.kind === "refused") setSent({ kind: "idle" });
        }}
      />
      {sent.kind === "refused" && <Refused words={sent.words} />}
    </Modal>
  );
}

/** What the editor opened on. */
type Opened =
  | { readonly kind: "reading" }
  | {
      readonly kind: "ready";
      /** The document as read, "" for one replacing an unsealed revision. */
      readonly text: string;
      /** The condition every check and the save are sent under. */
      readonly etag: string;
      /** The engine's sentence where the active revision could not be read and is being replaced. */
      readonly replacing: RefusalWords | null;
    }
  | { readonly kind: "refused"; readonly words: RefusalWords };

/** What the last check or save came to. */
type Checked =
  | { readonly kind: "idle" }
  | { readonly kind: "checking" }
  | { readonly kind: "saving" }
  /** The document holds what was read: nothing to save. */
  | { readonly kind: "unchanged" }
  /** Valid; `warnings` are the ones this document introduces. */
  | {
      readonly kind: "valid";
      readonly warnings: readonly ConfigWarning[];
      readonly confirm: boolean;
    }
  /** Refused for the document: each problem located where the engine could. */
  | {
      readonly kind: "problems";
      readonly problems: readonly ConfigProblem[];
    }
  | { readonly kind: "refused"; readonly words: RefusalWords };

/** The warnings a dry run of a whole document says it introduces. */
function introduced(outcome: Extract<ConfigCheckOutcome, { kind: "valid" }>): ConfigWarning[] {
  return outcome.warnings.filter((w) => w.introduced === true);
}

/** What the document editor declares to the assistant: its text, and its save's summary. */
interface ConfigDraft {
  document: string;
  summary: string;
}

/** The editor's two fields: the document, a credential, and the summary, which may be proposed. */
const CONFIG_DRAFT_FIELDS: readonly DraftFieldSpec<ConfigDraft>[] = [
  {
    path: "document",
    label: "The company document, as YAML",
    secret: true,
    read: (draft) => draft.document,
  },
  {
    path: "summary",
    label: "Summary",
    hint: "why, in one line",
    read: (draft) => draft.summary,
    write: (draft, value) =>
      typeof value === "string" && !value.includes("\n")
        ? { ...draft, summary: value }
        : refuse("A summary is one line of text."),
  },
];

/**
 * The company document as YAML, edited in place on the Active lens: check it,
 * import a file over it, save it with a summary.
 */
export function DocumentEditor({
  onClose,
  onStored,
}: {
  onClose: () => void;
  onStored: (stored: Stored) => void;
}) {
  const [opened, setOpened] = useState<Opened>({ kind: "reading" });
  const [text, setText] = useDraftState("");
  const [checked, setChecked] = useState<Checked>({ kind: "idle" });
  const [summary, setSummary] = useDraftState("");
  const [summaryErr, setSummaryErr] = useState<string | undefined>();
  const [discarding, setDiscarding] = useState<null | (() => void)>(null);
  const editor = useRef<SourceEditorHandle>(null);
  const inflight = useRef<AbortController | null>(null);
  useEffect(() => () => inflight.current?.abort(), []);

  useEffect(() => {
    const controller = new AbortController();
    void (async () => {
      try {
        const read = await getConfigText(controller.signal);
        if (read.kind === "text") {
          setText(read.text);
          setOpened({ kind: "ready", text: read.text, etag: read.etag, replacing: null });
          return;
        }
        if (read.kind === "none") {
          setOpened({
            kind: "refused",
            words: {
              message:
                "No company is configured yet. Import one with crewlet config import, or finish setup.",
              reload: false,
            },
          });
          return;
        }
        // THE ONE UNREADABLE REVISION A WRITE REPLACES: stored without a
        // seal, it opens to nobody, and a whole document is how it is put
        // right. The editor opens empty, conditional on the revision named.
        if (isUnsealedRefusal(read) && read.kind === "problems" && read.revisionId) {
          setOpened({
            kind: "ready",
            text: "",
            etag: revisionTag(read.revisionId),
            replacing: configRefusalWords(read, REREAD),
          });
          return;
        }
        setOpened({ kind: "refused", words: configRefusalWords(read, REREAD) });
      } catch (err) {
        if (!isAbort(err)) setOpened({ kind: "refused", words: failedWords(err) });
      }
    })();
    return () => controller.abort();
  }, []);

  const ready = opened.kind === "ready" ? opened : null;
  const dirty = ready !== null && text !== ready.text;
  const busy = checked.kind === "checking" || checked.kind === "saving";

  // WHAT IS OPEN, for the assistant: the company document, in this editor,
  // and the summary its save carries. THE DOCUMENT NEVER LEAVES THE PAGE: a
  // person writes every credential in it as its value (all of them when it
  // replaces an unreadable revision), so it is declared as a credential, sent
  // with no value and never set. Checking and saving stay the person's.
  const draft: ConfigDraft = { document: text, summary };
  useAssistantDraft<ConfigDraft>(
    ready
      ? {
          kind: "client",
          revision: ready.etag,
          scope: "The company document you are editing (not saved)",
          help:
            "The person is editing the company document as YAML. Its text holds credentials, " +
            "so it is never shown to you and you cannot change it; you can propose the " +
            "summary the revision history shows for this edit. Checking and saving stay theirs.",
          value: draft,
          fields: CONFIG_DRAFT_FIELDS,
          write: writeParts(draft, { document: setText, summary: setSummary }, () =>
            setSummaryErr(undefined),
          ),
          blocked: () => (busy ? "The document is being checked or saved." : null),
        }
      : null,
  );

  useLeaveGuard(
    dirty
      ? (_to, leave) => {
          setDiscarding(() => leave);
          return true;
        }
      : null,
  );

  const edit = (next: string) => {
    setText(next);
    if (!busy) setChecked({ kind: "idle" });
  };

  /** Checks the document; resolves with whether it may be saved as it stands. */
  async function check(signal: AbortSignal): Promise<Checked> {
    if (!ready) return { kind: "idle" };
    const outcome = await dryRunDocument(text, ready.etag, signal);
    if (outcome.kind === "valid") {
      return { kind: "valid", warnings: introduced(outcome), confirm: false };
    }
    if (outcome.kind === "saved") {
      return {
        kind: "refused",
        words: {
          message: `The engine stored the check as revision ${outcome.revisionId.slice(0, 10)}. ${REREAD}`,
          reload: true,
        },
      };
    }
    if (outcome.kind === "problems" && !isUnsealedRefusal(outcome)) {
      return { kind: "problems", problems: outcome.problems };
    }
    return { kind: "refused", words: configRefusalWords(outcome, REREAD) };
  }

  function begin(): AbortSignal {
    inflight.current?.abort();
    inflight.current = new AbortController();
    return inflight.current.signal;
  }

  async function runCheck() {
    if (busy || !ready) return;
    const signal = begin();
    setChecked({ kind: "checking" });
    try {
      setChecked(await check(signal));
    } catch (err) {
      if (!isAbort(err)) setChecked({ kind: "refused", words: failedWords(err) });
    }
  }

  async function save() {
    if (busy || !ready) return;
    if (!dirty && ready.replacing === null) {
      setChecked({ kind: "unchanged" });
      return;
    }
    const problem = summaryError(summary);
    if (problem) {
      setSummaryErr(problem);
      return;
    }
    const signal = begin();
    try {
      // CHECKED FIRST, every time, unless the check just warned and this is
      // the second press it asked for: a warning the document introduces is
      // said before it is stored, never after.
      if (!(checked.kind === "valid" && checked.confirm)) {
        setChecked({ kind: "checking" });
        const result = await check(signal);
        if (result.kind !== "valid") {
          setChecked(result);
          return;
        }
        if (result.warnings.length > 0) {
          setChecked({ ...result, confirm: true });
          return;
        }
      }
      setChecked({ kind: "saving" });
      const outcome = await saveDocument(text, ready.etag, summary, signal);
      if (outcome.kind === "saved") {
        onStored({ revisionId: outcome.revisionId, epoch: outcome.epoch });
        return;
      }
      if (outcome.kind === "problems" && !isUnsealedRefusal(outcome)) {
        setChecked({ kind: "problems", problems: outcome.problems });
        return;
      }
      setChecked({ kind: "refused", words: notSaved(outcome) });
    } catch (err) {
      if (!isAbort(err)) setChecked({ kind: "refused", words: failedWords(err) });
    }
  }

  async function importFile(files: PickedFile[]) {
    const file = files[0]?.file;
    if (!file) return;
    edit(await file.text());
  }

  function cancel() {
    if (busy) return;
    if (dirty) {
      setDiscarding(() => onClose);
      return;
    }
    onClose();
  }

  const marks: SourceMark[] =
    checked.kind === "problems"
      ? checked.problems
          .filter((p): p is ConfigProblem & { line: number } => typeof p.line === "number")
          .map((p) => ({ line: p.line, tone: "danger" as const, message: p.message }))
      : [];
  const confirming = checked.kind === "valid" && checked.confirm;

  return (
    <Card>
      <Card.Header
        icon={<FileCodeGlyph size="sm" />}
        subtitle="as YAML, checked by the engine before it is saved"
        actions={
          <span className="row gap-2">
            <FilePickButton
              size="small"
              variant="secondary"
              leadingIcon={<UploadGlyph size="sm" />}
              accept=".yaml,.yml,.json,application/yaml,application/json,text/yaml"
              disabled={!ready || busy}
              onFiles={(files) => void importFile(files)}
            >
              Import a file
            </FilePickButton>
            <Button
              size="small"
              variant="secondary"
              disabled={!ready || busy}
              loading={checked.kind === "checking"}
              onClick={() => void runCheck()}
            >
              Check
            </Button>
            <Button size="small" variant="ghost" onClick={cancel} disabled={busy}>
              Cancel
            </Button>
          </span>
        }
      >
        <Card.Title>
          {ready?.replacing ? "Replace the company document" : "Edit the company document"}
        </Card.Title>
      </Card.Header>
      <div className="col gap-3">
        {opened.kind === "reading" && <Skeleton variant="text" rows={10} label="Loading" />}
        {opened.kind === "refused" && <Refused words={opened.words} />}
        {ready && (
          <>
            {ready.replacing ? (
              <Callout variant="warning" title="The active revision can't be read">
                <span>{ready.replacing.message}</span>
                <span>
                  {" "}
                  Paste or import the whole document here: nothing is read from the revision it
                  replaces, so every credential is written as its value or a{" "}
                  <InlineCode>{"${VAR}"}</InlineCode> reference.
                </span>
              </Callout>
            ) : (
              <p className="t-caption" style={{ margin: 0 }}>
                Credentials are shown masked and keep their stored values: the engine restores each
                mask from the revision you opened, matching seats, units, servers and setup steps by
                name. Write a new value or a <InlineCode>{"${VAR}"}</InlineCode> where you mean to
                change one.
              </p>
            )}
            {/* WHAT THE CHECK SAID, ABOVE THE TEXT it is about: under a
                document of hundreds of lines it was below the fold, and a
                Check that seemed to do nothing was pressed again. */}
            <CheckResult checked={checked} onReveal={(line) => editor.current?.revealLine(line)} />
            <SourceEditor
              ref={editor}
              label="The company document, as YAML"
              value={text}
              onChange={edit}
              marks={marks}
              rows={28}
              indent={4}
              readOnly={busy}
              onSubmit={() => void runCheck()}
            />
            <div className="config-document-save">
              <SummaryField
                value={summary}
                error={summaryErr}
                placeholder="What this edit changes"
                disabled={busy}
                onChange={(next) => {
                  setSummary(next);
                  setSummaryErr(undefined);
                }}
              />
              <Button
                variant="primary"
                loading={checked.kind === "saving"}
                disabled={busy && checked.kind !== "saving"}
                onClick={() => void save()}
              >
                {confirming ? "Save anyway" : "Save"}
              </Button>
            </div>
          </>
        )}
        <ConfirmModal
          open={discarding !== null}
          destructive
          title="Discard your changes?"
          message="Your edits to the company document aren’t saved."
          cancelLabel="Keep editing"
          confirmLabel="Discard changes"
          onClose={() => setDiscarding(null)}
          onConfirm={() => {
            const go = discarding;
            setDiscarding(null);
            go?.();
          }}
        />
      </div>
    </Card>
  );
}

/**
 * What the last check or save said: valid (with the warnings this document
 * introduces), or each problem in the engine's words, a problem the parser
 * placed on a line offering to go there.
 */
function CheckResult({
  checked,
  onReveal,
}: {
  checked: Checked;
  onReveal: (line: number) => void;
}) {
  return (
    <div className="ceiling-status" role="status" aria-live="polite">
      {checked.kind === "checking" && <span className="t-caption">Checking with the engine…</span>}
      {checked.kind === "saving" && <span className="t-caption">Saving…</span>}
      {checked.kind === "unchanged" && (
        <span className="t-caption">Nothing changed since the document was opened.</span>
      )}
      {checked.kind === "valid" && checked.warnings.length === 0 && (
        <Callout variant="success" title="The engine accepts this document">
          <span>Nothing in it is refused, and it brings no new warning.</span>
        </Callout>
      )}
      {checked.kind === "valid" && checked.warnings.length > 0 && (
        <Callout variant="warning" title="The engine accepts this, with a warning">
          <ul className="ceiling-warnings">
            {checked.warnings.map((w) => (
              <li key={`${w.path}:${w.message}`}>{w.message}</li>
            ))}
          </ul>
        </Callout>
      )}
      {checked.kind === "problems" && (
        <Callout variant="danger" title="The engine refuses this document">
          <ul className="ceiling-warnings">
            {checked.problems.map((p, i) => (
              <li key={`${i}:${p.path}`}>
                {typeof p.line === "number" && (
                  <>
                    <button type="button" className="t-link" onClick={() => onReveal(p.line!)}>
                      Line {p.line}
                    </button>{" "}
                  </>
                )}
                {p.message}
              </li>
            ))}
          </ul>
        </Callout>
      )}
      {checked.kind === "refused" && <Refused words={checked.words} />}
    </div>
  );
}

/** A fault in this page rather than a refusal the engine made. */
function failedWords(err: unknown): RefusalWords {
  return {
    message: `The request could not be sent: ${err instanceof Error ? err.message : String(err)}. Reload before trying again.`,
    reload: true,
  };
}

/** A toast's words for a stored write, the same for all three. */
export function useStoredToast(): (what: string, stored: Stored) => void {
  const toast = useToast();
  return (what, stored) =>
    toast.show({
      variant: "success",
      title: `${what}: revision ${stored.revisionId.slice(0, 10)}`,
      message: "Stored and activated. Every node applies it next.",
    });
}
