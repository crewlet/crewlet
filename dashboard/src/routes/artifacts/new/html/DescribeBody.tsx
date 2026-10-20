/**
 * "Describe it": the person says what the page is for, and an agent builds it
 * in a task.
 *
 * THE BRIEF FIRST, THEN EVERYTHING THAT SHAPES IT: a starting point fills an
 * empty brief with an outline to make their own and ticks what that kind of
 * page needs; what the page can do is said in plain words; the title is
 * optional. The side panel shows the task exactly as it is filed and what
 * happens after, so nothing about the ask is hidden.
 */

import { useState, type ReactNode } from "react";
import {
  Callout,
  Checkbox,
  ChoiceButtons,
  Composer,
  FormField,
  Input,
  Timeline,
} from "@crewlethq/ui";
import { SendGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { youNeedWords } from "~/lib/access.ts";
import type { ArtifactCapability } from "~/protocol/index.ts";
import { CreateShell, useLeaveCreate } from "../CreateShell.tsx";
import { titleProblem } from "../create.ts";
import { artifactTaskBody, useFileArtifactTask } from "../fileTask.ts";
import { WhoBuildsIt, useWhoBuildsIt } from "../WhoBuildsIt.tsx";
import { pageName, taskBody, taskTitle } from "./brief.ts";
import { CAPABILITY_CHOICES, STARTERS } from "./words.ts";
import { LeaveDialog } from "./LeaveDialog.tsx";

/** The longest brief, in characters: a page's whole description, never a document. */
const BRIEF_MAX = 4000;

export function DescribeBody({ modeSwitch }: { modeSwitch: ReactNode }) {
  const leave = useLeaveCreate();
  const [brief, setBrief] = useState("");
  const [title, setTitle] = useState("");
  const [caps, setCaps] = useState<ReadonlySet<ArtifactCapability>>(new Set());
  const [leaving, setLeaving] = useState(false);
  const capabilities = CAPABILITY_CHOICES.map((c) => c.capability).filter((c) => caps.has(c));
  const asked = { title, brief, capabilities };
  const nav = useNavigator();
  const who = useWhoBuildsIt("html");
  const task = useFileArtifactTask();
  const agent = who.builders.find((b) => b.seat.handle === who.seat)?.seat;
  const project = who.projects.find((p) => p.key === who.project);
  // WHAT IS SHOWN IS WHAT IS FILED: the shared filing's body over this brief.
  const sent = artifactTaskBody("html", taskBody(asked));

  const hold =
    brief.trim() === ""
      ? "Describe the page first."
      : (titleProblem(title) ??
        task.held ??
        (who.seat === ""
          ? "This company has no agent to ask."
          : who.project === ""
            ? "Choose the project the task is filed in."
            : task.busy
              ? "Filing the task."
              : undefined));

  const ask = async () => {
    if (hold) return;
    const filed = await task.fileArtifactTask({
      kind: "html",
      title: taskTitle(asked),
      brief: taskBody(asked),
      seat: who.seat,
      project: who.project,
    });
    if (filed) nav.to(["work", filed.item]);
  };

  const start = (key: string) => {
    const starter = STARTERS.find((s) => s.key === key);
    if (!starter) return;
    // A STARTING POINT NEVER OVERWRITES WORDS: it fills an empty brief, and
    // always ticks what that kind of page needs.
    if (brief.trim() === "") setBrief(starter.brief);
    setCaps((held) => new Set([...held, ...starter.capabilities]));
  };

  return (
    <CreateShell
      kind="html"
      action={{
        label: agent ? `Ask ${agent.name}` : "Ask an agent",
        short: "Ask",
        icon: <SendGlyph size="sm" />,
        onSelect: () => void ask(),
        ...(hold ? { disabledReason: hold } : {}),
      }}
      status={task.busy ? "Filing the task…" : null}
      onCancel={() => (brief.trim() || title.trim() ? setLeaving(true) : leave())}
    >
      <div className="html-create">
        <section className="html-create-main" aria-label="The ask">
          <div className="html-create-head">{modeSwitch}</div>
          <FormField label="Describe the page">
            {(field) => (
              <Composer
                id={field.id}
                aria-describedby={field.describedBy}
                label="Describe the page"
                value={brief}
                onChange={setBrief}
                onSubmit={() => void ask()}
                maxLength={BRIEF_MAX}
                rows={6}
                autoFocus
                placeholder="A poll for the offsite venue: one vote per person, results update live."
              />
            )}
          </FormField>
          <FormField as="fieldset" label="Start from">
            <ChoiceButtons
              className="html-create-starters"
              label="Start from"
              options={STARTERS.map((s) => ({ value: s.key, label: s.label, detail: s.purpose }))}
              onChoose={start}
            />
          </FormField>
          <FormField as="fieldset" label="What the page can do">
            <div className="html-create-caps">
              {CAPABILITY_CHOICES.map((c) => (
                <Checkbox
                  key={c.capability}
                  size="sm"
                  checked={caps.has(c.capability)}
                  onCheckedChange={(on) =>
                    setCaps((held) => {
                      const next = new Set(held);
                      if (on) next.add(c.capability);
                      else next.delete(c.capability);
                      return next;
                    })
                  }
                  label={c.label}
                  description={c.detail}
                />
              ))}
            </div>
          </FormField>
          <WhoBuildsIt who={who} />
          <FormField
            label="Title"
            optional
            helper="The agent names the page when this is empty."
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
                onChange={(e) => setTitle(e.target.value)}
              />
            )}
          </FormField>
        </section>

        <aside className="html-create-side html-create-ask" aria-label="The task">
          <section>
            <h2 className="html-create-side-title">What will be sent</h2>
            {/* THE TASK AS IT IS FILED, one card: its title over its body. */}
            <div className="html-create-sent">
              <p className="html-create-sent-title">{taskTitle(asked)}</p>
              <pre className="html-create-sent-body">
                {brief.trim() ? sent : "Describe the page to see the task."}
              </pre>
            </div>
            {agent && who.project ? (
              <p className="html-create-sent-to">{`Filed to ${agent.name} in ${project?.name ?? who.project}.`}</p>
            ) : null}
            {task.refusal ? (
              <Callout variant="danger" title="The task was not filed" live="polite">
                {task.refusal.grants.length > 0
                  ? `${youNeedWords(task.refusal.grants)} Ask an admin.`
                  : task.refusal.sentence}
              </Callout>
            ) : null}
          </section>
          <section>
            <h2 className="html-create-side-title">What happens next</h2>
            <Timeline itemLabel="Step">
              <Timeline.Item>{`${agent?.name ?? "The agent"} picks up the task.`}</Timeline.Item>
              <Timeline.Item>
                It builds the page and checks it against the publish rules.
              </Timeline.Item>
              <Timeline.Item>
                It publishes the page and shares it with you as an editor.
              </Timeline.Item>
              <Timeline.Item>
                {`You hear about ${brief.trim() ? `“${pageName(asked)}”` : "it"} in your Inbox and on the task.`}
              </Timeline.Item>
            </Timeline>
          </section>
        </aside>
      </div>
      {leaving ? (
        <LeaveDialog
          title={title}
          hasDraft={false}
          discarding={false}
          onStay={() => setLeaving(false)}
          onLeave={leave}
          onDiscard={leave}
        />
      ) : null}
    </CreateShell>
  );
}
