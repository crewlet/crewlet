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
import { Checkbox, ChoiceButtons, Composer, FormField, Input, Timeline } from "@crewlethq/ui";
import type { ArtifactCapability } from "~/protocol/index.ts";
import { CreateShell, useLeaveCreate } from "../CreateShell.tsx";
import { titleProblem } from "../create.ts";
import { taskBody, taskTitle } from "./brief.ts";
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
          <section className="html-create-sent">
            <h2 className="html-create-side-title">What will be sent</h2>
            <p className="html-create-sent-title">{taskTitle(asked)}</p>
            <pre className="html-create-sent-body">
              {brief.trim() ? taskBody(asked) : "Describe the page to see the task."}
            </pre>
          </section>
          <section>
            <h2 className="html-create-side-title">What happens next</h2>
            <Timeline itemLabel="Step">
              <Timeline.Item>The agent picks up the task.</Timeline.Item>
              <Timeline.Item>
                It builds the page and checks it against the publish rules.
              </Timeline.Item>
              <Timeline.Item>
                It publishes the page and shares it with you as an editor.
              </Timeline.Item>
              <Timeline.Item>You hear about it in your Inbox and on the task.</Timeline.Item>
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
