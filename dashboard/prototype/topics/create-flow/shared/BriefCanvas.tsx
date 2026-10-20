/**
 * Asking an agent for a design, a dashboard or a page: the brief, starting
 * points for one, who builds it, and what happens once it is sent. These
 * three kinds are published by an agent's tools rather than typed by a
 * person, so their create experience is a request to one of the crew, and a
 * proposal draws it in its own frame (a page, a panel, a composer card).
 *
 * WHO BUILDS IT is chosen among the company's agents, drawn as they are drawn
 * everywhere (their character in their hue), the one best suited suggested
 * first. Sending files a task to them with the brief as its description, so
 * the request is followed in Work like any other.
 */

import { useMemo, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  ChoiceList,
  Composer,
  PromptStarters,
  Select,
  Timeline,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { KIND, NEXT_STEPS, STARTERS, type BuiltKind, type Starter } from "./kinds.ts";
import { useAskAgent } from "./commit.ts";

/** Words in an agent's name or goal that make it the one to suggest for a kind. */
const SUITED: Readonly<Record<BuiltKind, RegExp>> = {
  design: /design|ux|ui\b/i,
  dashboard: /data|analy|metric|dashboard/i,
  html: /engineer|swe|developer|frontend|build/i,
};

/** The company's agents, the one best suited to the kind first. */
export function useBuilders(kind: BuiltKind): Seat[] {
  const org = useOrg();
  return useMemo(() => {
    const agents = indexOrg(org).seats.filter((s) => s.kind === "agent");
    const fits = (s: Seat) => SUITED[kind].test(`${s.name} ${s.goal}`);
    return [...agents.filter(fits), ...agents.filter((s) => !fits(s))];
  }, [org, kind]);
}

/** What a seat's row says under its name: its unit, or that it reports to nobody. */
function seatLine(s: Seat): string {
  return s.unit ? s.unit.name : "Reports to the company";
}

export interface BriefState {
  prompt: string;
  picked: string | null;
  builder: string;
}

export function briefOf(builders: readonly Seat[], starter?: Starter): BriefState {
  return {
    prompt: starter?.prompt ?? "",
    picked: starter?.id ?? null,
    builder: builders[0]?.handle ?? "",
  };
}

export function BriefCanvas({
  kind,
  layout = "page",
  autoFocus = true,
  onCancel,
  starter,
  aside,
}: {
  kind: BuiltKind;
  /**
   * `page` sets who builds it and the next steps beside the brief; `stack`
   * sets them under it; `inline` keeps only the brief, its starting points
   * and one line naming the builder, for a composer inside the gallery.
   */
  layout?: "page" | "stack" | "inline";
  autoFocus?: boolean;
  onCancel?: () => void;
  /** The starting point the brief opens filled with. */
  starter?: string;
  /** Drawn first beside the brief: an example of what an agent made from one like it. */
  aside?: ReactNode;
}) {
  const builders = useBuilders(kind);
  const [state, setState] = useState<BriefState>(() =>
    briefOf(
      builders,
      STARTERS[kind].find((s) => s.id === starter),
    ),
  );
  const builder = builders.find((s) => s.handle === state.builder) ?? builders[0];
  const ask = useAskAgent();
  const words = KIND[kind];
  const send = () => {
    if (!builder || state.prompt.trim() === "") return;
    void ask.ask(kind, state.prompt, builder.handle, builder.name, builder.unit?.id ?? "");
  };

  if (ask.asked?.ok) {
    return (
      <div className={`cf-brief cf-brief--${layout} cf-brief--sent`}>
        <div className="cf-brief-sent">
          {builder && (
            <SeatAvatar
              name={builder.name}
              kind="agent"
              avatar={builder.avatar}
              size={56}
              decorative
            />
          )}
          <h2 className="cf-brief-sent-title">
            {builder ? `${builder.name} is on it` : "Your request is filed"}
          </h2>
          <p className="cf-brief-sent-text">
            {`${ask.asked.item ? `${ask.asked.item} is filed to ${builder?.name ?? "the agent"}. ` : ""}When ${words.one} is published it appears in Artifacts, private to you until you share it.`}
          </p>
          <Timeline itemLabel="Step" className="cf-brief-steps">
            {NEXT_STEPS.map((step, i) => (
              <Timeline.Item key={step.id} state={i === 1 ? "active" : "default"}>
                <span className="cf-step-title">{step.title}</span>
              </Timeline.Item>
            ))}
          </Timeline>
          <div className="cf-brief-sent-actions">
            {ask.asked.item && (
              <ButtonLink size="small" variant="secondary" href={href(["work", ask.asked.item])}>
                Follow the task
              </ButtonLink>
            )}
            <ButtonLink size="small" variant="primary" href={href(["artifacts"])}>
              Back to Artifacts
            </ButtonLink>
          </div>
        </div>
      </div>
    );
  }

  // WHAT HAPPENS NEXT reads on from the brief on a page, under the send it
  // explains; stacked, it closes the column.
  const next = (
    <section aria-labelledby="cf-next" className="cf-brief-block cf-brief-next">
      <h2 className="cf-brief-heading" id="cf-next">
        What happens next
      </h2>
      <Timeline itemLabel="Step" className="cf-brief-steps">
        {NEXT_STEPS.map((step) => (
          <Timeline.Item key={step.id}>
            <span className="cf-step-title">{step.title}</span>
            <span className="cf-step-detail">{step.detail}</span>
          </Timeline.Item>
        ))}
      </Timeline>
    </section>
  );

  return (
    <div className={`cf-brief cf-brief--${layout}`}>
      <div className="cf-brief-main">
        <Composer
          label={`What should ${words.one} show?`}
          value={state.prompt}
          onChange={(prompt) => setState((s) => ({ ...s, prompt, picked: null }))}
          onSubmit={send}
          rows={5}
          autoFocus={autoFocus}
          placeholder={`Describe ${words.one}: who it is for, what it should show, and anything it must follow.`}
          actions={
            <>
              {onCancel && (
                <Button size="small" variant="ghost" onClick={onCancel}>
                  Cancel
                </Button>
              )}
              <Button
                size="small"
                variant="primary"
                loading={ask.busy}
                disabledReason={
                  state.prompt.trim() === ""
                    ? `Say what ${words.one} should show.`
                    : !builder
                      ? "This company has no agent to ask."
                      : undefined
                }
                onClick={send}
              >
                {builder ? `Ask ${builder.name}` : words.action}
              </Button>
            </>
          }
        />
        <PromptStarters
          label="Starting points"
          starters={STARTERS[kind]}
          picked={state.picked}
          onPick={(s) => setState((was) => ({ ...was, prompt: s.prompt, picked: s.id }))}
        />
        {ask.refusal && <p className="artifact-refusal">{ask.refusal.sentence}</p>}
        {layout === "page" && next}
        {layout === "inline" && builder && (
          <div className="cf-brief-by">
            <span className="cf-brief-by-label">Built by</span>
            <Select
              size="sm"
              width="auto"
              appearance="quiet"
              ariaLabel="Who builds it"
              value={builder.handle}
              onChange={(handle) => setState((s) => ({ ...s, builder: String(handle) }))}
              options={builders.map((s, i) => ({
                value: s.handle,
                label: i === 0 ? `${s.name} (suggested)` : s.name,
                leading: (
                  <SeatAvatar name={s.name} kind="agent" avatar={s.avatar} size="xs" decorative />
                ),
              }))}
            />
            <span className="cf-brief-by-note">
              {`A task is filed to ${builder.name}, and ${words.one} appears here when it is published.`}
            </span>
          </div>
        )}
      </div>
      {layout !== "inline" && (
        <aside className="cf-brief-side">
          {aside}
          <section aria-labelledby="cf-builder" className="cf-brief-block">
            <h2 className="cf-brief-heading" id="cf-builder">
              Who builds it
            </h2>
            <ChoiceList
              label="Who builds it"
              value={builder?.handle ?? null}
              onChange={(handle) => setState((s) => ({ ...s, builder: handle }))}
              options={builders.map((s, i) => ({
                value: s.handle,
                label: s.name,
                detail: seatLine(s),
                leading: (
                  <SeatAvatar name={s.name} kind="agent" avatar={s.avatar} size="md" decorative />
                ),
                ...(i === 0 ? { tag: "Suggested" } : {}),
              }))}
            />
          </section>
          {layout === "stack" && next}
        </aside>
      )}
    </div>
  );
}
