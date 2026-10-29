/**
 * Where a model's coding runs happen, as three cards in the CLI step's shape:
 * each place's glyph and name, one line saying what it is, and two facts,
 * whether the fleet can start one there and how isolated it is. The company
 * default is tagged; a place no node can use is unavailable, its Ready fact
 * saying why and what fixes it (N5).
 *
 * THE FLEET ANSWERS, NOT THIS NODE (`llm_runners`): a seat's run starts on
 * whichever node holds the seat, so a place ready on some nodes and not on
 * others is offered, and says which.
 */

import type { ReactNode } from "react";
import { ChoiceCards, Skeleton, Text, type ChoiceCardOption } from "@crewlethq/ui";
import { GlobeGlyph, PackageGlyph, ServerGlyph } from "@crewlethq/icons/glyphs";
import type { RunnerCellName } from "~/contract/models.ts";
import { listed } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { RunnersAnswer } from "~/protocol/models.ts";
import { RUN_IN } from "../../words.ts";
import type { Draft, RunIn } from "../draft.ts";

const PLACES: readonly RunnerCellName[] = ["direct", "container", "e2b"];

/** Each place's line and isolation, its glyph drawn at the CLI marks' size so both card sets stand alike. */
const PLACE_WORDS: Record<
  RunnerCellName,
  { description: string; isolation: string; media: ReactNode }
> = {
  direct: {
    description: "Runs beside the engine.",
    isolation: "Shares the host",
    media: <ServerGlyph size="xl" />,
  },
  container: {
    description: "A fresh container for each run.",
    isolation: "Own filesystem and network",
    media: <PackageGlyph size="xl" />,
  },
  e2b: {
    description: "A cloud sandbox, off the host.",
    isolation: "A separate machine",
    media: <GlobeGlyph size="xl" />,
  },
};

/**
 * A place the company has not set up, in plain words: what it needs, never
 * the configuration's own sentence ("providers.sandbox declares no e2b
 * cell"), which says the same thing to someone who already knows the schema.
 */
const NOT_SET_UP: Record<RunnerCellName, string> = {
  direct: "Running on this engine's host isn't set up for this company.",
  container:
    "Containers aren't set up for this company. Add them under providers.sandbox to use them.",
  e2b: "E2B isn't set up for this company. Add it under providers.sandbox to use it.",
};

/**
 * Why a place cannot start a run on any node. A place the company never set
 * up says so plainly; one it set up that a node cannot use says that node's
 * own sentence and its fix, which name what to repair.
 */
function unready(answer: RunnersAnswer, place: RunnerCellName): string {
  if (!answer.cells[place].declared) return NOT_SET_UP[place];
  for (const node of answer.nodes) {
    const cell = node[place];
    if (cell && cell.state !== "ready") {
      return [cell.detail.replace(/\.$/, ""), cell.fix].filter(Boolean).join(". ");
    }
  }
  return "Not ready on any node.";
}

/** A place's readiness across the fleet, as a fact. */
export function readiness(
  answer: RunnersAnswer,
  place: RunnerCellName,
): { value: string; tone: "success" | "warning" | "neutral"; available: boolean } {
  const cell = answer.cells[place];
  if (cell.ready_on.length > 0 && cell.not_ready_on.length === 0)
    return { value: `Ready on ${listed(cell.ready_on)}`, tone: "success", available: true };
  if (cell.ready_on.length > 0)
    return {
      value: `Ready on ${listed(cell.ready_on)}; not on ${listed(cell.not_ready_on)}`,
      tone: "warning",
      available: true,
    };
  if (cell.not_ready_on.length === 0 && cell.declared)
    return { value: "Not reported yet", tone: "neutral", available: true };
  return { value: unready(answer, place), tone: "warning", available: false };
}

export function WhereItRuns({
  draft,
  error,
  onChange,
}: {
  draft: Draft;
  error?: string | undefined;
  onChange: (runIn: RunIn) => void;
}) {
  const read = useQuery("llm_runners");
  const answer = read.data;
  if (!answer) {
    if (read.error) {
      return (
        <Text as="p" variant="body" tone="secondary">
          Where coding can run could not be read. The model runs where the company default says.
        </Text>
      );
    }
    return <Skeleton variant="text" rows={4} label="Checking where coding can run" />;
  }
  const chosen: RunnerCellName = draft.runIn || answer.default_run_in;
  const hostLogin = draft.method === "host-login";
  const options: ChoiceCardOption[] = PLACES.map((place) => {
    const words = PLACE_WORDS[place];
    const ready = readiness(answer, place);
    // A CLI LOGIN STAYS ON ITS NODE, so a sandbox off the host cannot use one.
    const blocked = place === "e2b" && hostLogin;
    return {
      value: place,
      label: RUN_IN[place],
      media: words.media,
      ...(place === answer.default_run_in ? { tag: "Company default" } : {}),
      description: words.description,
      facts: [
        { label: "Ready", value: ready.value, tone: ready.tone },
        { label: "Isolation", value: words.isolation },
      ],
      ...(blocked
        ? {
            disabled: true,
            disabledReason: "A host login stays on its node; E2B needs a key or a token.",
            disabledReasonAt: 0,
          }
        : !ready.available
          ? { disabled: true, disabledReason: ready.value, disabledReasonAt: 0 }
          : {}),
    };
  });
  const reported = answer.nodes.filter((n) => n.reported).map((n) => n.id);
  return (
    <div className="models-stack">
      <ChoiceCards
        label="Where coding runs happen"
        options={options}
        value={chosen}
        onChange={(v) => onChange(v === answer.default_run_in ? "" : (v as RunnerCellName))}
      />
      {error ? (
        <Text as="p" variant="caption" className="models-field-error" role="alert">
          {error}
        </Text>
      ) : null}
      <Text as="p" variant="caption" tone="tertiary">
        {reported.length > 0 ? `Checked on ${listed(reported)}. ` : ""}Every other job runs on the
        host.
      </Text>
    </div>
  );
}
