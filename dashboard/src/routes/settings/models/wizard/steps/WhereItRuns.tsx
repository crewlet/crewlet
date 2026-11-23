/**
 * Where a model's executors run, as two cards in the harness step's shape:
 * each place's glyph and name, one line saying what it is, and two facts,
 * whether the fleet can start one there and how isolated it is. The company
 * default is tagged; a place no node can use is unavailable, its Status fact
 * saying why and what fixes it, in a person's words (N5).
 *
 * A COMPANY WITH NO PLACE SET UP gets this host: the save writes the sandbox
 * block the engine asks for (`draft.ts`, `nextDocument`), so a first coding
 * model can be added from here, and the card says the save sets it up.
 *
 * THE FLEET ANSWERS, NOT THIS NODE (`llm_runners`): a seat's executor launches
 * from whichever node holds the seat, so a place ready on some nodes and not on
 * others is offered, and says which.
 */

import type { ReactNode } from "react";
import { ChoiceCards, Skeleton, Text, type ChoiceCardOption } from "@crewlethq/ui";
import { PackageGlyph, ServerGlyph } from "@crewlethq/icons/glyphs";
import type { RunnerCellName } from "~/contract/models.ts";
import { listed } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { RunnersAnswer } from "~/protocol/models.ts";
import { RUN_IN } from "../../words.ts";
import type { Draft, RunIn } from "../draft.ts";

const PLACES: readonly RunnerCellName[] = ["direct", "container"];

/** Each place's line and isolation, its glyph drawn at the harness marks' size so both card sets stand alike. */
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
};

/**
 * Why containers cannot run where the company has not set them up, in plain
 * words: what they need, never the configuration's own sentence
 * ("providers.sandbox declares no container cell"), which says the same thing
 * to someone who already knows the schema. This host is never said to be
 * missing: the save sets it up ({@link noPlaceSetUp}).
 */
const NOT_SET_UP = "Containers aren't set up for this company. Set them up in Configuration.";

/**
 * A node's reason in a person's words where the engine's names a field: the
 * container image is the one a node reports, and its fix is a field path.
 */
function nodeWords(detail: string, fix: string): string {
  if (/providers\.sandbox\.local\.image/.test(`${detail} ${fix}`))
    return "Containers need an image with the harness installed. Set it in Configuration.";
  return [detail.replace(/\.$/, ""), fix].filter(Boolean).join(". ");
}

/**
 * Whether the company sets up no place at all, so the save sets up this host.
 * One `providers.sandbox` block sets up both places, so neither is set up alone.
 */
export function noPlaceSetUp(answer: RunnersAnswer): boolean {
  return !answer.declared;
}

/**
 * Why a place cannot start a run on any node. A place the company never set
 * up says so plainly; one it set up that a node cannot use says that node's
 * own sentence and its fix, which name what to repair.
 */
function unready(answer: RunnersAnswer, place: RunnerCellName): string {
  if (!answer.declared) return NOT_SET_UP;
  for (const node of answer.nodes) {
    const cell = node[place];
    if (cell && cell.state !== "ready") return nodeWords(cell.detail, cell.fix ?? "");
  }
  return "Not ready on any node.";
}

/** A place's readiness across the fleet, as a fact. */
export function readiness(
  answer: RunnersAnswer,
  place: RunnerCellName,
): { value: string; tone: "success" | "warning" | "neutral"; available: boolean } {
  const cell = answer.cells[place];
  // THIS HOST, WHERE NOTHING IS SET UP: the save sets it up.
  if (place === "direct" && noPlaceSetUp(answer))
    return { value: "Set up when you save", tone: "neutral", available: true };
  if (cell.ready_on.length > 0 && cell.not_ready_on.length === 0)
    return { value: `Ready on ${listed(cell.ready_on)}`, tone: "success", available: true };
  if (cell.ready_on.length > 0)
    return {
      value: `Ready on ${listed(cell.ready_on)}; not on ${listed(cell.not_ready_on)}`,
      tone: "warning",
      available: true,
    };
  if (cell.not_ready_on.length === 0 && answer.declared)
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
  const chosen: RunnerCellName | "" =
    draft.runIn || answer.default_run_in || (noPlaceSetUp(answer) ? "direct" : "");
  const options: ChoiceCardOption[] = PLACES.map((place) => {
    const words = PLACE_WORDS[place];
    const ready = readiness(answer, place);
    return {
      value: place,
      label: RUN_IN[place],
      media: words.media,
      ...(place === answer.default_run_in ? { tag: "Company default" } : {}),
      description: words.description,
      facts: [
        { label: "Status", value: ready.value, tone: ready.tone },
        { label: "Isolation", value: words.isolation },
      ],
      ...(!ready.available
        ? { disabled: true, disabledReason: ready.value, disabledReasonAt: 0 }
        : {}),
    };
  });
  const reported = answer.nodes.filter((n) => n.reported).map((n) => n.id);
  return (
    <div className="models-stack">
      <ChoiceCards
        label="Where executors run"
        minCardWidth="sm"
        density="compact"
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
