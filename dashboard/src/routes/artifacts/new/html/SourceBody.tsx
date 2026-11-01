/**
 * A new HTML page: the person writes, pastes, drops or picks the page, sees
 * the publish checks as they write, and previews it as a private draft. Or
 * they describe it, and an agent builds it.
 *
 * THE SOURCE IS THE PAGE, AND DESCRIBING IT IS THE HAND-OFF. Most people
 * describe a page rather than write one, so the page arrives with the
 * hand-off panel open beside the source (`AskAgent.tsx`): the brief, starting
 * points that tick what that kind of page needs, who builds it, and the task
 * exactly as it is filed. Closing it leaves the source to write in. Nothing
 * is lost between the two: what the page may use is the one set of choices
 * under the source, and a source the person has changed goes with the brief
 * for the agent to build on.
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

import { useEffect, useMemo, useRef, useState } from "react";
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
  SourceEditor,
  TabPanel,
  Tabs,
  Tag,
  useMediaMatch,
  useToast,
  type Finding,
  type PickedFile,
  type SourceEditorHandle,
  type SourceMark,
} from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  EyeGlyph,
  FileCodeGlyph,
  FileTextGlyph,
  FolderInputGlyph,
  ListChecksGlyph,
  RefreshCwGlyph,
  UploadGlyph,
} from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { asSentence, utf8Bytes } from "~/lib/format.ts";
import { TASK_BODY_MAX_BYTES } from "~/contract/work.ts";
import type { ArtifactCapability, ArtifactLintFinding } from "~/protocol/index.ts";
import { CreateShell, useLeaveCreate, type CreateAction } from "../CreateShell.tsx";
import { titleHold, titleProblem, useSaveIn, useWorkItem } from "../create.ts";
import { artifactTaskBody, useFileArtifactTask } from "../fileTask.ts";
import { useWhoBuildsIt } from "../WhoBuildsIt.tsx";
import { createDraft, linesOf } from "../assistantDraft.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { AskAgentPanel, askTool, chosenAgent, taskRefusal, useAskPanel } from "../AskAgent.tsx";
import { pageName, taskBody, taskTitle } from "./brief.ts";
import { DraftPreview } from "./DraftPreview.tsx";
import { LeaveDialog } from "./LeaveDialog.tsx";
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
import { CAPABILITY_CHOICES, STARTERS } from "./words.ts";

/** The longest brief, in characters: a page's whole description, never a document. */
const BRIEF_MAX = 4000;

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

/** What a new HTML page holds beyond what every create page does. */
interface PageState {
  readonly files: readonly PageFile[];
  readonly capabilities: readonly ArtifactCapability[];
}

/** The capabilities a page may be granted, by the word a value names one with. */
const CAPABILITY_IDS: readonly string[] = CAPABILITY_CHOICES.map((c) => c.capability);

/**
 * A new HTML page's own fields: each text file's source by its place, the
 * file's path its label, and what the page may use from the choices under
 * the source. A file of bytes (a picture, a font) is read by its path only.
 */
export function pageFields(files: readonly PageFile[]): DraftFieldSpec<PageState>[] {
  return [
    ...files.map((file, i): DraftFieldSpec<PageState> => {
      const path = `files.${i + 1}`;
      if (file.kind !== "text") {
        return { path, label: file.path, hint: "A file the person brought", read: () => "" };
      }
      return {
        path,
        label: file.path,
        hint: "The file's whole source",
        read: (d) => {
          const now = d.files[i];
          return now?.kind === "text" ? now.text : "";
        },
        write: (d, text) => {
          if (typeof text !== "string") return refuse("A file's source is text.");
          const now = d.files[i];
          if (!now || now.kind !== "text" || now.path !== file.path) {
            return refuse("That file is no longer on the page.");
          }
          return {
            ...d,
            files: d.files.map((f, at) => (at === i ? { path: f.path, kind: "text", text } : f)),
          };
        },
      };
    }),
    {
      path: "capabilities",
      label: "What the page may use",
      hint: `Any of ${CAPABILITY_IDS.join(", ")}, one per line`,
      read: (d) => d.capabilities,
      write: (d, value) => {
        const chosen = linesOf(value);
        if (chosen === null) return refuse("What the page may use is a list, one per line.");
        const unknown = chosen.find((c) => !CAPABILITY_IDS.includes(c));
        return unknown
          ? refuse(`A page may use ${CAPABILITY_IDS.join(", ")}; “${unknown}” is not one.`)
          : {
              ...d,
              capabilities: CAPABILITY_CHOICES.map((c) => c.capability).filter((c) =>
                chosen.includes(c),
              ),
            };
      },
    },
  ];
}

export function SourceBody() {
  const nav = useNavigator();
  const toast = useToast();
  const leave = useLeaveCreate();
  const workItem = useWorkItem();
  const { folder } = useSaveIn();
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
  const phone = useMediaMatch("(width < 40rem)");
  // CHANGED FROM THE SKELETON: the person has written or brought a page.
  const edited = files.length !== 1 || texts[0]?.text !== initial;
  const dirty = draft.draft ? draft.draft.published !== print : edited;

  // ---- describing it to an agent ---------------------------------------------
  const panel = useAskPanel(true);
  const who = useWhoBuildsIt("html");
  const task = useFileArtifactTask();
  const [brief, setBrief] = useState("");
  const agent = chosenAgent(who)?.seat;
  // THE PAGE'S TITLE NAMES THE TASK once the person has given one, or written
  // a page that has one; the skeleton's own placeholder title names nothing.
  const askTitle = typedTitle ?? (edited ? pageTitle : "");
  const asked = { title: askTitle, brief, capabilities };
  const entrySource = edited && entryText?.kind === "text" ? entryText.text : "";
  const handed = entrySource
    ? `${taskBody(asked)}\n\nBuild on the page as it stands (${entry}):\n\n\`\`\`html\n${entrySource.trimEnd()}\n\`\`\``
    : taskBody(asked);
  const sent = artifactTaskBody("html", handed);
  const sentBytes = utf8Bytes(sent);
  const askHold =
    brief.trim() === ""
      ? "Describe the page first."
      : (titleProblem(askTitle) ??
        task.held ??
        (who.seat === ""
          ? "Add an agent to the chart first."
          : who.project === ""
            ? "Choose the project to file the task in."
            : sentBytes > TASK_BODY_MAX_BYTES
              ? `The brief and the page are ${sentBytes.toLocaleString()} bytes, and a task holds at most ${TASK_BODY_MAX_BYTES.toLocaleString()}.`
              : undefined));
  const ask = async () => {
    if (askHold || task.busy) return;
    const filed = await task.fileArtifactTask({
      kind: "html",
      title: taskTitle(asked),
      brief: handed,
      seat: who.seat,
      project: who.project,
    });
    if (filed) nav.to(["worklet", filed.item]);
  };
  const used = CAPABILITY_CHOICES.filter((c) => capabilities.includes(c.capability));

  // WHAT IS OPEN, for the assistant: the title, each text file's source, what
  // the page may use, and the hand-off, set through the page's own setters;
  // previewing and publishing stay the person's presses.
  useAssistantDraft(
    createDraft<PageState>({
      what: "HTML page",
      help:
        "You can set the title, the source of each text file and what the page may use. A " +
        "value here marked cut is longer than shown, so propose a whole new source only for " +
        "a file you know in full. You make no images; pictures are files the person brings.",
      step: panel.open ? "ask" : side,
      title,
      setTitle: setTypedTitle,
      brief,
      setBrief,
      who,
      extra: {
        value: { files, capabilities },
        fields: pageFields(files),
        apply: (next) => {
          if (next.files !== files) setFiles([...next.files]);
          if (next.capabilities !== capabilities) {
            setOverrides(
              new Map(
                CAPABILITY_CHOICES.map((c) => [
                  c.capability,
                  next.capabilities.includes(c.capability),
                ]),
              ),
            );
          }
        },
      },
      blocked: () => (running ? "The preview is being published." : null),
    }),
  );

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
    const outcome = await draft.run({
      files,
      entry,
      title: title.trim(),
      capabilities,
      fingerprint: print,
      ...(workItem ? { workItem } : {}),
      ...(folder ? { folder } : {}),
    });
    // A REFUSAL IS READ IN CHECKS, where its findings are; a preview that
    // landed, or failed for any other reason, is said in Preview.
    setSide(outcome.kind === "refused" ? "checks" : "preview");
    return outcome.kind === "landed" ? outcome.draft : null;
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
        short: "Open",
        icon: <ArrowUpRightGlyph size="sm" />,
        onSelect: () => void open(),
        ...(dirty && hold ? { disabledReason: hold } : {}),
      }
    : {
        label: "Preview privately",
        short: "Preview",
        icon: <EyeGlyph size="sm" />,
        onSelect: () => void preview(),
        ...(hold ? { disabledReason: hold } : {}),
      };
  const tools = draft.draft
    ? [
        {
          key: "update",
          label: "Update preview",
          icon: <RefreshCwGlyph size="sm" />,
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
  // A PHONE'S BAR KEEPS ROOM FOR THE PAGE'S TITLE: there it says only that
  // a preview is being saved, and the draft itself is in the Preview pane.
  const status = running
    ? phone
      ? "Saving…"
      : "Publishing the preview…"
    : draft.draft && !phone
      ? `Private draft, v${draft.draft.head}${dirty ? ", edited" : ""}`
      : null;

  return (
    <CreateShell
      kind="html"
      action={action}
      tools={tools}
      status={status}
      onCancel={() => (draft.draft || dirty || brief.trim() ? setLeaving(true) : leave())}
      handoff={{
        tool: askTool(who, panel),
        open: () => panel.setOpen(true),
        panel: panel.open ? (
          <AskAgentPanel
            panel={panel}
            who={who}
            intro={`Describe the page and ${agent?.name ?? "an agent"} builds it in a task. Or close this and write the HTML yourself.`}
            brief={brief}
            onBrief={setBrief}
            briefLabel="Describe the page"
            briefPlaceholder="A poll for the offsite venue: one vote per person, results update live."
            briefHelper="What it is for, who uses it and what it keeps. A starting point ticks what that kind of page needs."
            briefMaxLength={BRIEF_MAX}
            starters={STARTERS.map((s) => ({ key: s.key, label: s.label, brief: s.brief }))}
            onStarter={(key) => {
              const starter = STARTERS.find((s) => s.key === key);
              if (!starter) return;
              // A STARTING POINT TICKS what that kind of page needs, and
              // never unticks what the person chose.
              setOverrides((m) => {
                const next = new Map(m);
                for (const c of starter.capabilities) next.set(c, true);
                return next;
              });
            }}
            carried={[
              ...(askTitle.trim()
                ? [
                    {
                      key: "title",
                      icon: <FileTextGlyph size="sm" />,
                      label: `“${askTitle.trim()}”`,
                      detail: "The page's title",
                    },
                  ]
                : []),
              {
                key: "caps",
                icon: <ListChecksGlyph size="sm" />,
                label:
                  used.length > 0
                    ? used.map((c) => c.label).join(", ")
                    : "Keeps nothing and knows no one",
                detail: "What the page can use, as chosen under the source",
              },
              ...(entrySource
                ? [
                    {
                      key: "source",
                      icon: <FileCodeGlyph size="sm" />,
                      label: `Your HTML, ${entry}`,
                      detail: "For the agent to build on",
                    },
                  ]
                : []),
            ]}
            task={{ title: taskTitle(asked), body: sent }}
            steps={[
              `${agent?.name ?? "The agent"} picks up the task.`,
              "It builds the page and checks it against the publish rules.",
              "It publishes the page and shares it with you as an editor.",
              `You hear about ${brief.trim() ? `“${pageName(asked)}”` : "it"} in your Inbox and on the task.`,
            ]}
            notice={taskRefusal(task.refusal)}
            sendLabel={(n) => `Ask ${n}`}
            onSend={() => void ask()}
            hold={askHold}
            busy={task.busy}
          />
        ) : null,
      }}
    >
      <div className="html-create">
        <section className="html-create-main" aria-label="The page">
          <div className="html-create-head">
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
            {shown?.kind === "text" ? (
              <p className="html-create-keys html-create-editor-keys">
                <Kbd keys={[...runKeys]} subtle /> to preview
              </p>
            ) : null}
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
                    icon={<EyeGlyph size={32} />}
                    title="Preview publishes a private draft"
                    description="The page runs on its own isolated origin, so previewing saves it as a draft only you can see, and shows it here. Nothing is shared until you share it."
                    action={
                      // ITS KEY IS SAID ONCE, under the editor it is pressed in.
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
                    }
                  />
                  {draft.state.kind === "failed" ? (
                    <Callout variant="danger" title="The preview was not saved" live="polite">
                      {draft.state.text}
                    </Callout>
                  ) : null}
                </div>
              )
            ) : (
              <ChecksPane checks={checks} refused={refused !== null} findings={findings} />
            )}
            {side === "preview" && draft.draft && draft.state.kind === "failed" ? (
              <Callout variant="danger" title="The preview was not updated" live="polite">
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
            : "Checked as you write, by the rules a publish uses.";
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
