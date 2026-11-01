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
 * "Ask an agent" in the bar opens the hand-off panel beside the page
 * (`AskAgent.tsx`): what the doc should cover, who drafts it, and what goes
 * with the brief. The page stays as it is, so the writer can go on writing
 * while they ask. The send files one real task to that agent
 * (`useFileArtifactTask`), which publishes the doc on the task, and the page
 * lands on the task. Nothing typed is lost: the title is the page's, and a
 * draft or an untouched template goes with the brief for the agent to build
 * on, as the panel lists.
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
  markdownWords,
  type RichEditorHandle,
} from "@crewlethq/ui";
import { CheckGlyph, FileTextGlyph, ListOrderedGlyph, LockGlyph } from "@crewlethq/icons/glyphs";
import { useLeaveGuard, useNavigator, useParam } from "~/app/router.tsx";
import { TASK_BODY_MAX_BYTES } from "~/contract/work.ts";
import { plural, utf8Bytes } from "~/lib/format.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { DOC_TEMPLATES, docTemplate, type DocTemplate } from "~/lib/artifacts/docTemplates.ts";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import {
  briefFolder,
  titleHold,
  titleProblem,
  useCreateArtifact,
  useSaveIn,
  useWorkItem,
} from "./create.ts";
import { artifactTaskBody, useFileArtifactTask } from "./fileTask.ts";
import { useWhoBuildsIt } from "./WhoBuildsIt.tsx";
import { createDraft } from "./assistantDraft.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import {
  AskAgentPanel,
  askTitleHold,
  askTool,
  chosenAgent,
  taskRefusal,
  useAskPanel,
} from "./AskAgent.tsx";

/** Briefs to start from, one press each. */
const DOC_BRIEFS = [
  {
    key: "onboarding",
    label: "Onboarding guide",
    brief:
      "A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Doclet.",
  },
  {
    key: "proposal",
    label: "Proposal",
    brief:
      "A proposal for the change we discussed: the problem, the options we weighed, the one we recommend and what it costs. Written for the leads who decide.",
  },
  {
    key: "summary",
    label: "Summary of a thread",
    brief:
      "A summary of where the project stands for someone joining it today: what was decided, what is open, who owns what and where the work lives.",
  },
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

/** What a new doc holds beyond what every create page does. */
interface DocDraft {
  readonly body: string;
  readonly template: string;
}

/**
 * A new doc's own fields: its body, and the template it starts from, which
 * is offered while nothing of the writer's own is on the page (`untouched`)
 * and fills the body as a press of it would.
 */
export function docFields(untouched: boolean): DraftFieldSpec<DocDraft>[] {
  return [
    {
      path: "body",
      label: "Body",
      hint: "Markdown",
      read: (d) => d.body,
      write: (d, body) =>
        typeof body === "string" ? { ...d, body } : refuse("The body is Markdown text."),
      summary: (body) => plural(markdownWords(typeof body === "string" ? body : ""), "word"),
    },
    {
      path: "template",
      label: "Template",
      hint: `One of ${DOC_TEMPLATES.map((t) => t.key).join(", ")}, or empty for none`,
      read: (d) => d.template,
      write: (d, key) => {
        if (!untouched) {
          return refuse("A template is offered while the body is empty or still a template's.");
        }
        if (typeof key !== "string") return refuse("A template is one choice.");
        if (key.trim() === "") return { body: "", template: "" };
        const chosen = docTemplate(key.trim());
        return chosen
          ? { body: chosen.body, template: chosen.key }
          : refuse(`Choose one of ${DOC_TEMPLATES.map((t) => t.key).join(", ")}.`);
      },
    },
  ];
}

export function DocCreate() {
  const nav = useNavigator();
  const create = useCreateArtifact();
  const ask = useFileArtifactTask();
  const who = useWhoBuildsIt("doc");
  const access = useArtifactAccess({ app: true });
  const workItem = useWorkItem();
  const saveIn = useSaveIn();
  const leaveCreate = useLeaveCreate();
  const panel = useAskPanel();
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

  const agent = chosenAgent(who)?.seat;
  const handed = docBrief(brief, body, template);
  const taskText = artifactTaskBody("doc", handed, undefined, briefFolder(saveIn));
  const taskBytes = utf8Bytes(taskText);
  const busy = create.busy || ask.busy;
  const hold = !access.can ? access.reason : titleHold(title);
  const askHold =
    ask.held ??
    (!agent
      ? "Add an agent to the chart first."
      : brief.trim() === ""
        ? "Say what the doc should cover."
        : (askTitleHold(title, "doc") ??
          (!who.project
            ? "Choose the project to file the task in."
            : taskBytes > TASK_BODY_MAX_BYTES
              ? `The brief and the draft are ${taskBytes.toLocaleString()} bytes, and a task holds at most ${TASK_BODY_MAX_BYTES.toLocaleString()}.`
              : undefined)));

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

  // WHAT IS OPEN, for the assistant: the title, the body and the template it
  // started from, and the hand-off beside them, each set through the page's
  // own setters; Create and Ask stay the writer's presses.
  useAssistantDraft(
    createDraft<DocDraft>({
      what: "doc",
      help:
        "The body is Markdown and a change replaces all of it. A template fills the body " +
        "while it is empty or still holds a template untouched.",
      step: panel.open ? "ask" : "write",
      title,
      setTitle,
      brief,
      setBrief,
      who,
      extra: {
        value: { body, template: template?.key ?? "" },
        fields: docFields(untouched),
        apply: (next) => {
          if (next.template !== (template?.key ?? "")) setTemplate(docTemplate(next.template));
          setBody(next.body);
          // THE EDITOR TAKES A WHOLE NEW BODY only as a fresh document.
          setLoaded((n) => n + 1);
        },
      },
      blocked: () => (busy ? "The doc is being created." : null),
    }),
  );

  // CANCEL AND ESCAPE ASK FIRST while anything is written: the shell leaves
  // by replacing the entry, which no guard holds.
  const cancel = () => (dirty ? setLeaving(() => leaveCreate) : leaveCreate());

  const press = async () => {
    if (hold || busy) return;
    released.current = true;
    await create.fromStarter("doc", name, written ? { doc: { body } } : {});
    // STILL HERE: the engine refused, and its sentence is on the page.
    released.current = false;
  };

  const send = async () => {
    if (askHold || busy) return;
    released.current = true;
    const filed = await ask.fileArtifactTask({
      kind: "doc",
      title: name,
      brief: handed,
      seat: who.seat,
      project: who.project,
    });
    if (filed?.item) nav.to(["worklet", filed.item]);
    // STILL HERE: the engine refused, and its sentence is in the panel.
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
      editor.current?.focus();
    } else if (event.key === "Escape") {
      event.preventDefault();
      cancel();
    }
  };

  const refusal = create.refusal && { title: "The doc was not created", text: create.refusal };

  // WHAT GOES WITH THE BRIEF, as the panel lists it: the page's title, and
  // the draft or the template's outline the agent builds on.
  const carried = [
    ...(name
      ? [
          {
            key: "title",
            icon: <FileTextGlyph size="sm" />,
            label: `“${name}”`,
            detail: "The doc's title",
          },
        ]
      : []),
    ...(!written
      ? []
      : untouched && template
        ? [
            {
              key: "template",
              icon: <ListOrderedGlyph size="sm" />,
              label: `The ${template.label.toLowerCase()} outline`,
              detail: "For the agent to follow",
            },
          ]
        : [
            {
              key: "draft",
              icon: <FileTextGlyph size="sm" />,
              label: "Your draft",
              detail: `${plural(words, "word")}, for the agent to build on`,
            },
          ]),
  ];

  return (
    <CreateShell
      kind="doc"
      onCancel={cancel}
      status={
        create.busy
          ? "Creating…"
          : ask.busy
            ? "Filing the task…"
            : words > 0
              ? plural(words, "word")
              : undefined
      }
      action={{
        label: "Create doc",
        icon: <CheckGlyph size="sm" />,
        onSelect: () => void press(),
        ...(busy ? { disabledReason: "Creating the doc." } : hold ? { disabledReason: hold } : {}),
      }}
      handoff={{
        tool: askTool(who, panel),
        open: () => panel.setOpen(true),
        panel: panel.open ? (
          <AskAgentPanel
            panel={panel}
            who={who}
            intro={`${agent?.name ?? "The agent"} gets a task with your brief, drafts the doc and publishes it on the task.`}
            brief={brief}
            onBrief={setBrief}
            briefRef={briefField}
            briefLabel="What should the doc cover?"
            briefPlaceholder="A one-page guide for new engineers on how we review incidents."
            briefHelper={
              written
                ? "What you wrote on the page goes with it."
                : "Name the readers, the outline you have in mind and the pages to draw on."
            }
            starters={DOC_BRIEFS}
            carried={carried}
            alone="Write on the page, or start from a template, and it goes with the brief."
            task={{ title: name || "Untitled doc", body: taskText }}
            steps={[
              `A task is filed to ${agent?.name ?? "the agent"}${who.project ? ` in ${who.project}` : ""}, with your brief.`,
              `${agent?.name ?? "The agent"} drafts the doc and publishes it on the task.`,
              "You review it, edit it in place and share it.",
            ]}
            notice={taskRefusal(ask.refusal)}
            sendLabel={(n) => `Ask ${n}`}
            onSend={() => void send()}
            hold={askHold}
            busy={ask.busy}
          />
        ) : null,
      }}
    >
      <div className="doc-create">
        <div className="doc-create-page">
          <p className="doc-create-where">
            <LockGlyph size="sm" />
            <span>
              Private to you until you share it
              {workItem ? `, and listed on ${workItem}` : ""}
            </span>
          </p>

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
        </div>
      </div>

      <ConfirmModal
        open={leaving !== null}
        destructive
        title="Discard this doc?"
        message="Its title and what you wrote are not saved anywhere yet."
        cancelLabel="Keep editing"
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
