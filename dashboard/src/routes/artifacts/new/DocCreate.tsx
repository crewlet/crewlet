/**
 * A new doc, written where it will be read, or asked of an agent: its own
 * create page, in the shared frame (`CreateShell`).
 *
 * # Straight into the page
 *
 * There is no form before the writing. The page opens as the doc will read,
 * the title set in the document's own title type with the cursor in it, and
 * the body in the editor every doc is edited with (`RichEditor`, ADR-0044),
 * so the doc looks the same before it exists, while it is written and once
 * it is saved. Enter in the title moves to the body; Command+Enter creates
 * from either; Escape asks to leave.
 *
 * # Templates fill the page, they never gate it
 *
 * While the body is empty, or still holds the template it took untouched, a
 * row of templates sits above it (`lib/artifacts/docTemplates.ts`); one press
 * fills the body, another press clears it. Once anybody writes, the row
 * steps aside, so a template never replaces writing.
 *
 * # Or an agent drafts it
 *
 * "Ask an agent" turns the same column into a brief: the title, what the doc
 * should cover, and who drafts it (`WhoBuildsIt`, each agent as its
 * character). The press files one real task to that agent
 * (`useFileArtifactTask`), which publishes the doc on the task, and the page
 * lands on the task. Nothing typed is lost between the two: the title is
 * shared, and a draft or an untouched template goes with the brief for the
 * agent to build on. The mode is in the address (`mode=agent`).
 *
 * # Nothing exists until the press
 *
 * Create is one keyed write: the engine begins the doc from its starter with
 * the text as written (`doc`), its title as the tab's heading, and the page
 * opens it. A body left empty begins from the starter's own line. Until a
 * press nothing is made, so leaving makes nothing either; with anything
 * written every move away asks first (`useLeaveGuard`).
 */

import { useRef, useState, type KeyboardEvent } from "react";
import {
  Callout,
  ConfirmModal,
  DocumentTitleField,
  FilterChip,
  FilterChipGroup,
  FormField,
  RichEditor,
  SegmentedControl,
  Textarea,
  markdownWords,
  type RichEditorHandle,
} from "@crewlethq/ui";
import { CheckGlyph, LockGlyph, PencilGlyph, SendGlyph, BotGlyph } from "@crewlethq/icons/glyphs";
import { useLeaveGuard, useNavigator, useParam } from "~/app/router.tsx";
import { TASK_BODY_MAX_BYTES } from "~/contract/work.ts";
import { plural, utf8Bytes } from "~/lib/format.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { DOC_TEMPLATES, docTemplate, type DocTemplate } from "~/lib/artifacts/docTemplates.ts";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import { titleHold, titleProblem, useCreateArtifact, useWorkItem } from "./create.ts";
import { artifactTaskBody, useFileArtifactTask } from "./fileTask.ts";
import { WhoBuildsIt, useWhoBuildsIt } from "./WhoBuildsIt.tsx";

/** The two ways the page makes a doc, as the address names them. */
const MODES = [
  { value: "write", label: "Write it", icon: <PencilGlyph size="sm" /> },
  { value: "agent", label: "Ask an agent", icon: <BotGlyph size="sm" /> },
] as const;

/**
 * The brief an agent is handed: the reader's words, then what is already on
 * the page for it to build on, a draft or an untouched template's outline.
 */
export function docBrief(brief: string, body: string, template: DocTemplate | undefined): string {
  const own = brief.trim();
  if (body.trim() === "") return own;
  if (template && body === template.body) {
    return `${own}\n\nFollow the ${template.label.toLowerCase()} outline:\n\n${body.trim()}`;
  }
  return `${own}\n\nBuild on this first draft:\n\n${body.trim()}`;
}

export function DocCreate() {
  const nav = useNavigator();
  const create = useCreateArtifact();
  const ask = useFileArtifactTask();
  const who = useWhoBuildsIt("doc");
  const access = useArtifactAccess({ app: true });
  const workItem = useWorkItem();
  const leaveCreate = useLeaveCreate();
  const [modeParam, setMode] = useParam("mode", "write");
  const mode = modeParam === "agent" ? "agent" : "write";
  const [templateParam] = useParam("template", "");
  const [template, setTemplate] = useState<DocTemplate | undefined>(() =>
    docTemplate(templateParam),
  );
  const [title, setTitle] = useState("");
  const [body, setBody] = useState(template?.body ?? "");
  const [brief, setBrief] = useState("");
  // A NEW KEY LOADS THE EDITOR AGAIN: a template replaces the whole body,
  // which the editor takes only as a fresh document.
  const [loaded, setLoaded] = useState(0);
  const editor = useRef<RichEditorHandle>(null);
  const briefField = useRef<HTMLTextAreaElement>(null);
  // WHERE A HELD MOVE WAS GOING, while Discard is asked about.
  const [leaving, setLeaving] = useState<(() => void) | null>(null);
  // LET GO, read by the guard as it is asked: the move a press makes is
  // asked of the guard in the same press that lets it go.
  const released = useRef(false);

  const name = title.trim();
  const written = body.trim() !== "";
  // THE TEMPLATES ARE OFFERED while nothing of the writer's own is on the
  // page: an empty body, or a template's untouched.
  const untouched = !written || (template !== undefined && body === template.body);
  const words = markdownWords(body);
  const problem = titleProblem(title);

  const agent = who.builders.find((b) => b.seat.handle === who.seat)?.seat;
  const handed = docBrief(brief, body, template);
  const taskBytes = utf8Bytes(artifactTaskBody("doc", handed));
  const busy = create.busy || ask.busy;
  const hold =
    mode === "write"
      ? !access.can
        ? access.reason
        : titleHold(title)
      : (ask.held ??
        titleHold(title) ??
        (!agent
          ? "Add an agent to the chart first."
          : brief.trim() === ""
            ? "Say what the doc should cover."
            : !who.project
              ? "Choose the project to file the task in."
              : taskBytes > TASK_BODY_MAX_BYTES
                ? `The brief and the draft are ${taskBytes} bytes, and a task holds at most ${TASK_BODY_MAX_BYTES}.`
                : undefined));

  const dirty = name !== "" || brief.trim() !== "" || (written && !untouched);
  useLeaveGuard(
    dirty
      ? (_to, leave) => {
          if (released.current) return false;
          setLeaving(() => leave);
          return true;
        }
      : null,
  );

  // CANCEL AND ESCAPE ASK FIRST while anything is written: the shell leaves
  // by replacing the entry, which no guard holds.
  const cancel = () => (dirty ? setLeaving(() => leaveCreate) : leaveCreate());

  const press = async () => {
    if (hold || busy) return;
    released.current = true;
    if (mode === "write") {
      await create.fromStarter("doc", name, written ? { doc: { body } } : {});
    } else {
      const filed = await ask.fileArtifactTask({
        kind: "doc",
        title: name,
        brief: handed,
        seat: who.seat,
        project: who.project,
      });
      if (filed?.item) nav.to(["work", filed.item]);
    }
    // STILL HERE: the engine refused, and its sentence is on the page.
    released.current = false;
  };

  const startFrom = (key: string | null) => {
    const next = key ? docTemplate(key) : undefined;
    setTemplate(next);
    setBody(next?.body ?? "");
    setLoaded((n) => n + 1);
  };

  // Command+Enter is the shell's (`create.start`): it passes through here.
  const onTitleKey = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) return;
    if (event.key === "Enter" || event.key === "ArrowDown") {
      event.preventDefault();
      if (mode === "write") editor.current?.focus();
      else briefField.current?.focus();
    } else if (event.key === "Escape") {
      event.preventDefault();
      cancel();
    }
  };

  const refusal =
    mode === "write"
      ? create.refusal && { title: "The doc was not created", text: create.refusal }
      : ask.refusal && {
          title: "The task was not filed",
          text: [ask.refusal.sentence, ask.refusal.hint].filter(Boolean).join(" "),
        };

  return (
    <CreateShell
      kind="doc"
      onCancel={cancel}
      status={
        busy
          ? mode === "write"
            ? "Creating…"
            : "Filing the task…"
          : mode === "write" && words > 0
            ? plural(words, "word")
            : undefined
      }
      action={{
        label: mode === "write" ? "Create doc" : agent ? `Ask ${agent.name}` : "Ask an agent",
        icon: mode === "write" ? <CheckGlyph size="sm" /> : <SendGlyph size="sm" />,
        onSelect: () => void press(),
        ...(busy
          ? { disabledReason: mode === "write" ? "Creating the doc." : "Filing the task." }
          : hold
            ? { disabledReason: hold }
            : {}),
      }}
    >
      <div className="doc-create">
        <div className="doc-create-page">
          <div className="doc-create-head">
            <SegmentedControl
              label="How to make it"
              semantics="radio"
              size="sm"
              options={MODES.map((m) => ({ value: m.value, label: m.label, icon: m.icon }))}
              value={mode}
              onValueChange={(next: string) => setMode(next === "agent" ? "agent" : "")}
            />
            {mode === "write" && (
              <p className="doc-create-where">
                <LockGlyph size="sm" />
                <span>
                  Private to you until you share it
                  {workItem ? `, and listed on ${workItem}` : ""}
                </span>
              </p>
            )}
          </div>

          {/* THE ENGINE'S REFUSAL AT THE TOP OF THE PAGE, where the writer
              looks after the press, not under a long body. */}
          {refusal && (
            <Callout variant="danger" title={refusal.title} role="alert">
              {refusal.text}
            </Callout>
          )}

          <FormField htmlFor="doc-create-title" error={problem}>
            {(field) => (
              <DocumentTitleField
                id={field.id}
                aria-label="Title"
                aria-describedby={field.describedBy}
                placeholder={template?.titleExample ?? "Untitled doc"}
                autoComplete="off"
                autoFocus
                value={title}
                error={problem !== undefined}
                disabled={busy}
                onChange={(event) => setTitle(event.target.value)}
                onKeyDown={onTitleKey}
              />
            )}
          </FormField>

          {mode === "write" ? (
            <>
              {untouched && (
                <FilterChipGroup
                  className="doc-create-starts"
                  label="Start from a template"
                  hideLabel
                  semantics="radio"
                  activate="manual"
                  allowNone
                  value={template?.key ?? null}
                  onValueChange={startFrom}
                >
                  {DOC_TEMPLATES.map((t) => (
                    <FilterChip key={t.key} value={t.key} aria-description={t.description}>
                      {t.label}
                    </FilterChip>
                  ))}
                </FilterChipGroup>
              )}

              <RichEditor
                key={loaded}
                ref={editor}
                className="doc-create-editor"
                value={body}
                onChange={setBody}
                label={name ? `The text of “${name}”` : "The text of the new doc"}
                placeholder="Write, or type a slash for blocks"
                disabled={busy}
                onSubmit={() => void press()}
                onEscape={cancel}
              />
            </>
          ) : (
            <div className="doc-create-brief">
              <FormField
                label="Brief"
                helper={
                  written
                    ? untouched && template
                      ? `The ${template.label.toLowerCase()} outline goes with it, for the agent to follow.`
                      : "Your draft goes with it, for the agent to build on."
                    : "What it should cover, who reads it, and what to draw on."
                }
              >
                {(field) => (
                  <Textarea
                    ref={briefField}
                    id={field.id}
                    aria-describedby={field.describedBy}
                    autoResize
                    rows={5}
                    placeholder="A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Knowledge."
                    value={brief}
                    disabled={busy}
                    onChange={(event) => setBrief(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === "Escape") {
                        event.preventDefault();
                        cancel();
                      }
                    }}
                  />
                )}
              </FormField>

              <WhoBuildsIt who={who} />

              <Callout variant="neutral" title="What happens next">
                <ol className="doc-create-next">
                  <li>
                    A task is filed to {agent?.name ?? "the agent"}
                    {who.project ? ` in ${who.project}` : ""}, with your brief.
                  </li>
                  <li>{agent?.name ?? "The agent"} drafts the doc and publishes it on the task.</li>
                  <li>You review it, edit it in place and share it.</li>
                </ol>
              </Callout>
            </div>
          )}
        </div>
      </div>

      <ConfirmModal
        open={leaving !== null}
        destructive
        title="Discard this doc?"
        message="What you wrote is not saved anywhere."
        cancelLabel="Keep writing"
        confirmLabel="Discard"
        onClose={() => setLeaving(null)}
        onConfirm={() => {
          const go = leaving;
          released.current = true;
          setLeaving(null);
          go?.();
        }}
      />
    </CreateShell>
  );
}
