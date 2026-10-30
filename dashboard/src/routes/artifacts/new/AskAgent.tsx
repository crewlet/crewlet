/**
 * Asking an agent from a create page: one control in the bar and one panel at
 * the page's side, the same on every kind.
 *
 * # The page is the direct way; the agent is a hand-off
 *
 * A create page IS the artifact being made, written, laid out or composed by
 * the reader. Asking an agent does not swap that page for a form: "Ask an
 * agent" in the bar (with the suggested agent's character, so the reader sees
 * who would pick it up) opens the hand-off panel beside the page, where the
 * page stays live. The panel holds the brief, briefs to start from, who builds
 * it and in which project (`WhoBuildsIt`), what goes with the brief, the task
 * exactly as it is filed and what happens next, and the send itself. Whatever
 * the reader already made (a draft, the slides laid out, the board, the
 * columns) goes with the brief, and the panel lists it, so trying one way and
 * then the other loses nothing.
 *
 * It is the open artifact's own side panel (`ArtifactPanel` in an
 * `ArtifactWorkspace`, which `CreateShell` lays the body in): a column where
 * there is room, a drawer over the page's end where there is not, the screen
 * on a phone. Its state is the address's (`panel=ask`), as an open artifact's
 * panels are, so a link opens the page with it open. A kind an agent makes
 * best (design, which only an agent draws, and HTML, which most people
 * describe rather than write) opens with it, where it can stand beside the
 * page as a column: on a narrower screen it would arrive as a drawer over
 * the page's first step, so there it waits for the press.
 *
 * # One send, in reach
 *
 * The send is the panel's foot, "Ask SRE Lead" beside SRE Lead's character,
 * pressed by Command+Enter from anywhere in the panel. Held, it says why in
 * words beside the button. The engine's refusal is said above it, where the
 * reader looks after the press.
 */

import { useRef, type ReactNode, type Ref } from "react";
import {
  AgentHandoffPanel,
  Callout,
  useMediaMatch,
  type AgentHandoffItem,
  type AgentHandoffStarter,
  type ArtifactBarTool,
} from "@crewlethq/ui";
import { BotGlyph, TextQuoteGlyph } from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { Builder } from "~/lib/artifacts/builders.ts";
import type { Refusal } from "~/lib/useAct.ts";
import { accessToAsk } from "~/lib/authorityRemedy.ts";
import { BrandAgentNote } from "~/components/brand.tsx";
import { titleProblem } from "./create.ts";
import { WhoBuildsIt, type WhoBuildsItState } from "./WhoBuildsIt.tsx";

/** The panel's value in the address. */
const ASK = "ask";
/** The address's word for a panel closed on a page that opens with it. */
const CLOSED = "closed";

/**
 * Wide enough for the panel to stand beside the page as a column rather than
 * a drawer over it: the workspace's own two-column width (960px, the kit's
 * `ArtifactWorkspace`) plus the frame's sidebar beside it.
 */
const BESIDE = "(width >= 1200px)";

/** Whether the hand-off panel is open, and the way to open or close it. */
export interface AskPanel {
  open: boolean;
  /**
   * Open since the page arrived rather than by the reader's press: it does not
   * take focus from the field the page put the cursor in.
   */
  arrived: boolean;
  setOpen: (open: boolean) => void;
  toggle: () => void;
}

/**
 * The hand-off panel's open state, kept in the address. `opensWith` is a kind
 * an agent makes best, whose page arrives with the panel open.
 */
export function useAskPanel(opensWith = false): AskPanel {
  const beside = useMediaMatch(BESIDE);
  const arrivesOpen = opensWith && beside;
  const [value, set] = useParam("panel", arrivesOpen ? ASK : "");
  const pressed = useRef(false);
  const open = value === ASK;
  const setOpen = (next: boolean) => {
    pressed.current = true;
    set(next ? ASK : arrivesOpen ? CLOSED : "");
  };
  return { open, arrived: !pressed.current, setOpen, toggle: () => setOpen(!open) };
}

/** The agent the panel is set to ask, or the suggested one while the chart is read. */
export function chosenAgent(who: WhoBuildsItState): Builder | undefined {
  return who.builders.find((b) => b.seat.handle === who.seat) ?? who.builders[0];
}

/** An agent's face at a size, or the agent glyph while the chart has none. */
function face(builder: Builder | undefined, size: "xs" | "sm"): ReactNode {
  return builder ? (
    <SeatAvatar
      name={builder.seat.name}
      kind="agent"
      avatar={builder.seat.avatar}
      size={size}
      decorative
    />
  ) : (
    <BotGlyph size="sm" />
  );
}

/**
 * The bar's "Ask an agent": the suggested agent's character beside its words,
 * pressed while the panel is open. On a kind only an agent makes, it is the
 * page's primary action while the panel is closed (`primary`).
 */
export function askTool(
  who: WhoBuildsItState,
  panel: AskPanel,
  primary = false,
): ArtifactBarTool & { emphasis?: "primary" } {
  const suggested = who.builders[0];
  return {
    key: "ask",
    label: "Ask an agent",
    icon: face(suggested, "xs"),
    onSelect: panel.toggle,
    labelled: true,
    ...(primary && !panel.open ? { emphasis: "primary" as const } : { pressed: panel.open }),
  };
}

/**
 * Why a hand-off waits on the page's title, said from the panel: the title is
 * on the page beside it, so the panel names where to give one.
 */
export function askTitleHold(title: string, what: string): string | undefined {
  return title.trim() === "" ? `Give the ${what} a title first.` : titleProblem(title);
}

/** The engine's refusal of a task, as the panel says it above the send. */
export function taskRefusal(refusal: Refusal | null, title = "The task was not filed"): ReactNode {
  if (!refusal) return null;
  return (
    <Callout variant="danger" title={title} live="polite">
      {accessToAsk(refusal) ?? [refusal.sentence, refusal.hint].filter(Boolean).join(" ")}
    </Callout>
  );
}

export interface AskAgentPanelProps {
  panel: AskPanel;
  who: WhoBuildsItState;
  /** What the agent does with the brief, in the kind's words. */
  intro: ReactNode;
  brief: string;
  onBrief: (brief: string) => void;
  briefLabel: string;
  briefPlaceholder: string;
  briefHelper?: ReactNode;
  briefMaxLength?: number;
  briefRef?: Ref<HTMLTextAreaElement>;
  starters?: readonly (AgentHandoffStarter & { brief: string })[];
  /** A starter was pressed; its brief is set already. */
  onStarter?: (key: string) => void;
  /** The kind's own fields under the brief. */
  children?: ReactNode;
  /** Leaves the project out of who builds it, for a kind that files none. */
  withProject?: boolean;
  carried?: readonly AgentHandoffItem[];
  /**
   * What the panel says goes with the brief while nothing else does: how
   * what the reader makes on the page would go with it.
   */
  alone?: string;
  task?: { title: string; body: string };
  steps: readonly ReactNode[];
  notice?: ReactNode;
  /** The send's words: "Ask SRE Lead", "Create and ask SRE Lead". */
  sendLabel: (agent: string) => string;
  onSend: () => void;
  hold?: string | undefined;
  busy?: boolean;
}

/** The hand-off panel over the shared pieces: who builds it, the agent's face on the send. */
export function AskAgentPanel({
  panel,
  who,
  intro,
  brief,
  onBrief,
  briefLabel,
  briefPlaceholder,
  briefHelper,
  briefMaxLength,
  briefRef,
  starters,
  onStarter,
  children,
  withProject = true,
  carried = [],
  alone,
  task,
  steps,
  notice,
  sendLabel,
  onSend,
  hold,
  busy = false,
}: AskAgentPanelProps) {
  const agent = chosenAgent(who);
  return (
    <AgentHandoffPanel
      icon={<BotGlyph size="sm" />}
      onClose={() => panel.setOpen(false)}
      focusOnOpen={!panel.arrived}
      intro={intro}
      brief={brief}
      onBriefChange={onBrief}
      briefLabel={briefLabel}
      briefPlaceholder={briefPlaceholder}
      briefHelper={briefHelper}
      briefMaxLength={briefMaxLength}
      briefRef={briefRef}
      starters={starters}
      onStarter={(key) => {
        const starter = starters?.find((s) => s.key === key);
        if (starter) onBrief(starter.brief);
        onStarter?.(key);
      }}
      agent={<WhoBuildsIt who={who} withProject={withProject} />}
      carried={
        carried.length > 0 || !alone
          ? carried
          : [
              {
                key: "alone",
                icon: <TextQuoteGlyph size="sm" />,
                label: "Your brief alone",
                detail: alone,
              },
            ]
      }
      task={task}
      steps={steps}
      notice={notice}
      send={{
        label: agent ? sendLabel(agent.seat.name) : "Ask an agent",
        media: face(agent, "xs"),
        onSend,
        busy,
        disabledReason: hold,
      }}
    >
      <BrandAgentNote />
      {children}
    </AgentHandoffPanel>
  );
}
