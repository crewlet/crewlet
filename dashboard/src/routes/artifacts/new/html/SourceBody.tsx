/**
 * "Bring the HTML": the person writes, pastes, drops or picks the page, sees
 * the publish checks as they write, and previews it as a private draft.
 *
 * TWO PANES, THE SOURCE AND WHAT BECOMES OF IT: the files, the title and what
 * the page may use on the left; on the right the draft's live page (Preview)
 * and the linter's findings (Checks), every one taking the editor to its line.
 * On a phone the panes stack, the source first.
 *
 * THE BAR CARRIES THE GESTURES: Preview privately (Command+Enter) before the
 * first draft; then Update preview and Open page. Cancel asks before it
 * leaves anything behind: a draft is kept or discarded, unsaved text is lost
 * only once the person says so.
 */

import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Checkbox,
  EmptyState,
  FileDrop,
  FilePickButton,
  FindingList,
  FormField,
  Input,
  Kbd,
  Modal,
  SourceEditor,
  TabPanel,
  Tabs,
  Tag,
  useToast,
  type Finding,
  type PickedFile,
  type SourceEditorHandle,
  type SourceMark,
} from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  EyeGlyph,
  FolderInputGlyph,
  RefreshCwGlyph,
  UploadGlyph,
} from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import { ARTIFACT_TRASH_RETENTION_DAYS } from "~/contract/artifacts.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { asSentence } from "~/lib/format.ts";
import type { ArtifactCapability, ArtifactLintFinding } from "~/protocol/index.ts";
import { CreateShell, useLeaveCreate, type CreateAction } from "../CreateShell.tsx";
import { titleHold, titleProblem, useWorkItem } from "../create.ts";
import { DraftPreview } from "./DraftPreview.tsx";
import {
  PAGE_ACCEPT,
  capabilitiesUsed,
  entryOf,
  fingerprint,
  readIntake,
  skeleton,
  textFiles,
  titleOf,
  type PageFile,
} from "./source.ts";
import { useChecks, type CheckState } from "./useChecks.ts";
import { useDraft } from "./useDraft.ts";
import { CAPABILITY_CHOICES } from "./words.ts";

/** The id the side panes' tab row and panel share. */
const SIDE_PANEL = "html-create-side";

/** A finding's place, as a reader names it. */
function placeOf(f: ArtifactLintFinding): string {
  return f.line ? `${f.path}, line ${f.line}` : f.path;
}

/** What the last check said, or null while there is no answer to draw. */
function answerOf(state: CheckState) {
  switch (state.kind) {
    case "checked":
      return state.answer;
    case "checking":
    case "unavailable":
      return state.last;
    default:
      return null;
  }
}

export function SourceBody({ modeSwitch }: { modeSwitch: ReactNode }) {
  const nav = useNavigator();
  const toast = useToast();
  const leave = useLeaveCreate();
  const workItem = useWorkItem();
  const access = useArtifactAccess({ app: true });
  const editor = useRef<SourceEditorHandle>(null);

  const initial = useMemo(() => skeleton(), []);
  const [files, setFiles] = useState<PageFile[]>([
    { path: "index.html", kind: "text", text: initial },
  ]);
  const [active, setActive] = useState("index.html");
  const [typedTitle, setTypedTitle] = useState<string | null>(null);
  const [overrides, setOverrides] = useState<ReadonlyMap<ArtifactCapability, boolean>>(new Map());
  const [skipped, setSkipped] = useState<string[]>([]);
  const [side, setSide] = useState<"preview" | "checks">("preview");
  const [leaving, setLeaving] = useState(false);
  const [reveal, setReveal] = useState<{ path: string; line: number } | null>(null);

  const entry = entryOf(files);
  const entryText = files.find((f) => f.path === entry && f.kind === "text");
  const pageTitle = entryText?.kind === "text" ? titleOf(entryText.text) : "";
  const title = typedTitle ?? pageTitle;
  const detected = useMemo(() => capabilitiesUsed(files), [files]);
  const capabilities = CAPABILITY_CHOICES.map((c) => c.capability).filter(
    (c) => overrides.get(c) ?? detected.has(c),
  );
  const texts = useMemo(() => textFiles(files), [files]);
  const shown = files.find((f) => f.path === active) ?? files[0];
  const print = fingerprint(files, capabilities);

  const checks = useChecks(texts, entry, entry !== "");
  const draft = useDraft();
  const running = draft.state.kind === "running";
  const dirty = draft.draft
    ? draft.draft.published !== print
    : files.length !== 1 || texts[0]?.text !== initial;

  // A FINDING IN ANOTHER FILE opens that file first, then its line once drawn.
  useEffect(() => {
    if (!reveal || reveal.path !== active) return;
    editor.current?.revealLine(reveal.line);
    setReveal(null);
  }, [reveal, active]);

  // ---- findings ------------------------------------------------------------
  // A REFUSAL IS ABOUT WHAT WAS RUN: once the page is edited, the checks
  // speak for it again, and the refused lines no longer mark the editor.
  const refused =
    draft.state.kind === "refused" && draft.state.fingerprint === print
      ? draft.state.findings
      : null;
  const answer = answerOf(checks);
  const refusals = refused ?? answer?.refusals ?? [];
  const warnings = refused ? [] : (answer?.warnings ?? draft.draft?.warnings ?? []);
  const blocking = refusals.length;
  const goTo = (f: ArtifactLintFinding) => {
    if (!f.line || !files.some((x) => x.path === f.path && x.kind === "text")) return undefined;
    return () => {
      setActive(f.path);
      setReveal({ path: f.path, line: f.line! });
    };
  };
  const findings: Finding[] = [
    ...refusals.map((f, i) => ({
      key: `r${i}`,
      tone: "danger" as const,
      location: placeOf(f),
      message: asSentence(f.message),
      onSelect: goTo(f),
    })),
    ...warnings.map((f, i) => ({
      key: `w${i}`,
      tone: "warning" as const,
      location: placeOf(f),
      message: asSentence(f.message),
      onSelect: goTo(f),
    })),
  ];
  const marks: SourceMark[] = [
    ...refusals
      .filter((f) => f.path === shown?.path && f.line)
      .map((f) => ({
        line: f.line!,
        tone: "danger" as const,
        message: asSentence(f.message),
      })),
    ...warnings
      .filter((f) => f.path === shown?.path && f.line)
      .map((f) => ({
        line: f.line!,
        tone: "warning" as const,
        message: asSentence(f.message),
      })),
  ];

  // ---- writing -------------------------------------------------------------
  const write = (path: string, text: string) =>
    setFiles((all) => all.map((f) => (f.path === path ? { path, kind: "text", text } : f)));

  const bring = async (picked: PickedFile[]) => {
    const intake = await readIntake(picked);
    setSkipped(intake.skipped);
    if (intake.files.length === 0) return;
    // A PAGE BROUGHT REPLACES THE PAGE; a stylesheet or a picture alone joins it.
    const replaces = intake.files.some((f) => f.path.endsWith(".html") || f.path.endsWith(".htm"));
    setFiles((all) => {
      if (replaces) return intake.files;
      const kept = all.filter((f) => !intake.files.some((n) => n.path === f.path));
      return [...kept, ...intake.files].sort((a, b) => a.path.localeCompare(b.path));
    });
    const first = replaces ? entryOf(intake.files) : intake.files[0]!.path;
    setActive(first || intake.files[0]!.path);
    toast.ok(
      intake.files.length === 1
        ? `Added ${intake.files[0]!.path}`
        : `Added ${intake.files.length} files`,
    );
  };

  // ---- the gestures ----------------------------------------------------------
  const hold = !access.can
    ? access.reason
    : entry === ""
      ? "Add an HTML page to preview."
      : (titleHold(title) ??
        (blocking > 0
          ? `Fix the ${blocking === 1 ? "problem" : `${blocking} problems`} in Checks first.`
          : running
            ? "Publishing the preview."
            : undefined));

  const preview = async () => {
    if (hold) return null;
    const landed = await draft.run({
      files,
      entry,
      title: title.trim(),
      capabilities,
      fingerprint: print,
      ...(workItem ? { workItem } : {}),
    });
    setSide(landed ? "preview" : "checks");
    return landed;
  };

  const open = async () => {
    const current = dirty ? await preview() : draft.draft;
    if (current) nav.to(artifactPath(current.id));
  };

  const discard = async () => {
    const refusal = await draft.discard();
    if (refusal) {
      toast.show({ variant: "danger", title: "The draft was not discarded", message: refusal });
      return;
    }
    toast.ok("Draft discarded");
    leave();
  };

  const runKeys = ["Mod", "Enter"] as const;
  const action: CreateAction = draft.draft
    ? {
        label: dirty ? "Save and open" : "Open page",
        icon: <ArrowUpRightGlyph size="sm" />,
        onSelect: () => void open(),
        ...(dirty && hold ? { disabledReason: hold } : {}),
      }
    : {
        label: "Preview privately",
        icon: <EyeGlyph size="sm" />,
        keys: runKeys,
        onSelect: () => void preview(),
        ...(hold ? { disabledReason: hold } : {}),
      };
  const tools = draft.draft
    ? [
        {
          key: "update",
          label: "Update preview",
          icon: <RefreshCwGlyph size="sm" />,
          keys: runKeys,
          labelled: true,
          onSelect: () => void preview(),
          ...(hold
            ? { disabledReason: hold }
            : !dirty
              ? { disabledReason: "The preview shows this version already." }
              : {}),
        },
      ]
    : [];
  const status = running
    ? "Publishing the preview…"
    : draft.draft
      ? dirty
        ? `Draft, version ${draft.draft.head}. Changes not previewed`
        : `Draft, version ${draft.draft.head}. Only you can see it`
      : null;

  return (
    <CreateShell
      kind="html"
      action={action}
      tools={tools}
      status={status}
      onCancel={() => (draft.draft || dirty ? setLeaving(true) : leave())}
    >
      <div className="html-create">
        <section className="html-create-main" aria-label="The page">
          <div className="html-create-head">
            {modeSwitch}
            <div className="html-create-pick">
              <FilePickButton
                size="small"
                variant="ghost"
                leadingIcon={<UploadGlyph size="sm" />}
                accept={PAGE_ACCEPT}
                onFiles={(picked) => void bring(picked)}
              >
                Upload files
              </FilePickButton>
              <FilePickButton
                size="small"
                variant="ghost"
                leadingIcon={<FolderInputGlyph size="sm" />}
                directory
                onFiles={(picked) => void bring(picked)}
              >
                Upload a folder
              </FilePickButton>
            </div>
          </div>

          <FormField
            label="Title"
            helper={typedTitle === null && pageTitle ? "Taken from the page’s title." : undefined}
            error={titleProblem(title)}
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid}
                width="full"
                value={title}
                placeholder="Offsite venue poll"
                onChange={(e) => setTypedTitle(e.target.value)}
              />
            )}
          </FormField>

          {skipped.length > 0 ? (
            <Callout variant="warning" title="Some files were left out">
              {`A page cannot hold ${skipped.join(", ")}. Pages, stylesheets, scripts, data, pictures and fonts can be uploaded.`}
            </Callout>
          ) : null}

          <div className="html-create-source">
            {files.length > 1 ? (
              <Tabs
                variant="underline"
                size="sm"
                ariaLabel="Files"
                value={active}
                onValueChange={setActive}
                items={files.map((f) => ({
                  value: f.path,
                  label: f.path === entry ? `${f.path} (page)` : f.path,
                }))}
              />
            ) : null}
            <FileDrop
              className="html-create-drop"
              title="Drop to add to the page"
              hint="An HTML file replaces the page; stylesheets, scripts and pictures join it."
              onFiles={(picked) => void bring(picked)}
            >
              {shown?.kind === "text" ? (
                <SourceEditor
                  ref={editor}
                  fill
                  label={`Source of ${shown.path}`}
                  value={shown.text}
                  marks={marks}
                  onChange={(text) => write(shown.path, text)}
                  onSubmit={() => void (draft.draft ? (dirty ? preview() : null) : preview())}
                />
              ) : shown ? (
                <div className="html-create-binary">
                  <p>{`${shown.path} is kept as it is: ${shown.blob.size.toLocaleString()} bytes.`}</p>
                  <Button
                    size="small"
                    variant="ghost"
                    onClick={() => {
                      setFiles((all) => all.filter((f) => f.path !== shown.path));
                      setActive(entry);
                    }}
                  >
                    Remove it
                  </Button>
                </div>
              ) : null}
            </FileDrop>
          </div>

          <FormField as="fieldset" label="What the page can use">
            <div className="html-create-caps">
              {CAPABILITY_CHOICES.map((c) => {
                const used = detected.has(c.capability);
                const on = overrides.get(c.capability) ?? used;
                return (
                  <Checkbox
                    key={c.capability}
                    size="sm"
                    checked={on}
                    onCheckedChange={(checked) =>
                      setOverrides((m) => new Map(m).set(c.capability, checked))
                    }
                    label={
                      <span className="html-create-cap">
                        {c.label}
                        {used ? <Tag size="sm">Used by the page</Tag> : null}
                      </span>
                    }
                    description={
                      used && !on
                        ? "The page’s script calls this, and it will be refused without it."
                        : c.detail
                    }
                  />
                );
              })}
            </div>
          </FormField>
        </section>

        <aside className="html-create-side" aria-label="Preview and checks">
          <Tabs
            variant="underline"
            size="sm"
            ariaLabel="Preview and checks"
            panelId={SIDE_PANEL}
            value={side}
            onValueChange={(v) => setSide(v === "checks" ? "checks" : "preview")}
            items={[
              { value: "preview", label: "Preview" },
              {
                value: "checks",
                label: "Checks",
                ...(blocking > 0
                  ? {
                      marker: {
                        tone: "danger" as const,
                        count: blocking,
                        label: `${blocking} ${blocking === 1 ? "problem" : "problems"}`,
                      },
                    }
                  : warnings.length > 0
                    ? { count: warnings.length }
                    : {}),
              },
            ]}
          />
          <TabPanel id={SIDE_PANEL} value={side} className="html-create-pane">
            {side === "preview" ? (
              draft.draft ? (
                <DraftPreview
                  id={draft.draft.id}
                  version={draft.draft.head}
                  title={draft.draft.title}
                />
              ) : (
                <div className="html-create-first">
                  <EmptyState
                    icon={<EyeGlyph />}
                    title="Preview publishes a private draft"
                    description="The page runs on its own isolated origin, so previewing saves it as a draft only you can see, and shows it here. Nothing is shared until you share it."
                    action={
                      <div className="html-create-first-action">
                        <Button
                          size="small"
                          variant="primary"
                          leadingIcon={<EyeGlyph size="sm" />}
                          loading={running}
                          disabledReason={hold}
                          onClick={() => void preview()}
                        >
                          Preview privately
                        </Button>
                        <span className="html-create-keys">
                          <Kbd keys={[...runKeys]} subtle /> from the editor
                        </span>
                      </div>
                    }
                  />
                  {draft.state.kind === "failed" ? (
                    <Callout variant="danger" title="The preview was not saved">
                      {draft.state.text}
                    </Callout>
                  ) : null}
                </div>
              )
            ) : (
              <ChecksPane checks={checks} refused={refused !== null} findings={findings} />
            )}
            {side === "preview" && draft.draft && draft.state.kind === "failed" ? (
              <Callout variant="danger" title="The preview was not updated">
                {draft.state.text}
              </Callout>
            ) : null}
          </TabPanel>
        </aside>
      </div>

      {leaving ? (
        <LeaveDialog
          title={draft.draft?.title ?? title}
          hasDraft={draft.draft !== null}
          discarding={draft.discarding}
          onStay={() => setLeaving(false)}
          onLeave={leave}
          onDiscard={() => void discard()}
        />
      ) : null}
    </CreateShell>
  );
}

/** The Checks pane: what the checks are doing, then every finding. */
function ChecksPane({
  checks,
  refused,
  findings,
}: {
  checks: CheckState;
  refused: boolean;
  findings: Finding[];
}) {
  const said = refused
    ? "The preview was refused, so nothing was saved. Fix these and preview again."
    : checks.kind === "checking"
      ? "Checking the page…"
      : checks.kind === "unavailable"
        ? checks.text
        : checks.kind === "idle"
          ? "Add an HTML page and it is checked as you write."
          : findings.length === 0
            ? "No problems. The page passes the checks a publish makes."
            : "The checks a publish makes, as you write.";
  return (
    <div className="html-create-checks">
      <p className="html-create-checks-said" role="status">
        {said}
      </p>
      {findings.length > 0 ? (
        <FindingList label="Findings in the page" findings={findings} />
      ) : null}
    </div>
  );
}

/** Leaving with something made: a draft kept or discarded, or unsaved text lost. */
function LeaveDialog({
  title,
  hasDraft,
  discarding,
  onStay,
  onLeave,
  onDiscard,
}: {
  title: string;
  hasDraft: boolean;
  discarding: boolean;
  onStay: () => void;
  onLeave: () => void;
  onDiscard: () => void;
}) {
  return (
    <Modal
      open
      onClose={onStay}
      size="sm"
      title={hasDraft ? "Keep this draft?" : "Leave without saving?"}
      footer={
        hasDraft ? (
          <>
            <Button size="small" variant="ghost" onClick={onStay}>
              Stay
            </Button>
            <Button size="small" variant="danger" loading={discarding} onClick={onDiscard}>
              Discard draft
            </Button>
            <Button size="small" variant="primary" onClick={onLeave}>
              Keep draft
            </Button>
          </>
        ) : (
          <>
            <Button size="small" variant="ghost" onClick={onStay}>
              Stay
            </Button>
            <Button size="small" variant="danger" onClick={onLeave}>
              Leave
            </Button>
          </>
        )
      }
    >
      <p className="html-create-leave">
        {hasDraft
          ? `“${title || "Untitled page"}” is saved as a private draft only you can see. Keep it to find it under Mine in Artifacts, or discard it to the trash, where it is kept for ${ARTIFACT_TRASH_RETENTION_DAYS} days.`
          : "Nothing has been saved yet, so the page written here will be lost."}
      </p>
    </Modal>
  );
}
