/**
 * The ranked-search vocabulary every search screen shares: the three modes by
 * the words a person reads, and what an answer that served less than it was
 * asked for says about itself.
 *
 * ONE VOCABULARY, because the wire has one (`knowledge.Mode`: `hybrid`,
 * `keyword`, `semantic`) and both ranked answers — `knowledge` and
 * `work_search` — carry the same outcome fields (`SearchOutcome`). The word
 * for `semantic` is "Meaning": what the mode ranks by, rather than the name of
 * the technique.
 */

import type { SearchMode, SearchOutcome } from "~/protocol/index.ts";

/**
 * The modes, in the order a control draws them; the first is the engine's
 * default. The HINT is each mode's one-line tooltip, written for a person
 * choosing between them — what it matches — rather than for somebody who
 * knows what BM25 or an embedding is.
 */
export const SEARCH_MODES: readonly { value: SearchMode; label: string; hint: string }[] = [
  {
    value: "hybrid",
    label: "Hybrid",
    hint: "Matches the words and what they mean, combined — the default",
  },
  {
    value: "keyword",
    label: "Keyword",
    hint: "Matches the words you typed, exactly as written",
  },
  {
    value: "semantic",
    label: "Meaning",
    hint: "Matches what the text is about, even when it uses other words",
  },
];

/** A `mode=` off an address, or the default for one this build does not draw. */
export function asSearchMode(value: string): SearchMode {
  return SEARCH_MODES.some((m) => m.value === value) ? (value as SearchMode) : "hybrid";
}

/** The word a person reads for a mode. */
export function modeLabel(mode: SearchMode | ""): string {
  return SEARCH_MODES.find((m) => m.value === mode)?.label ?? mode;
}

/**
 * Why the engine served less than it was asked, in a sentence — or null when
 * it served what was asked.
 *
 * FROM `degraded`, never from prose: the value is what the engine decided,
 * and a screen that string-matched a note would break the day the note was
 * reworded. A reason this build does not know is still said, by its value,
 * rather than dropped — it is a newer engine telling the reader something.
 */
export function servedNote(answer: SearchOutcome | null | undefined): string | null {
  if (!answer) return null;
  // AN OLDER NODE'S ANSWER CARRIES NONE OF THE OUTCOME, and that is not a
  // degradation: it ranked the one way it knew and says nothing about it.
  const degraded = answer.degraded ?? "";
  const asked = modeLabel(answer.mode ?? "hybrid");
  const served = answer.served_mode ?? "";
  const why = degradedWhy(degraded);
  if (served === "" && degraded) {
    return `${asked} search could not run here — ${why}. Nothing was ranked by it, so nothing is listed rather than a different ranking passed off as this one.`;
  }
  if (served && served !== (answer.mode ?? "hybrid")) {
    return `Asked for ${asked}, served ${modeLabel(served)}${why ? ` — ${why}` : ""}.`;
  }
  if (degraded) return `${asked} search ran in part — ${why}.`;
  return null;
}

function degradedWhy(degraded: string): string {
  switch (degraded) {
    case "":
      return "";
    case "no_embeddings":
      return "this company has no embeddings provider, so nothing is ranked by meaning";
    case "embedding_failed":
      return "the question's own embedding could not be computed this time";
    case "semantic_partial":
      return "part of the fleet answered without its meaning half, so meaning was ranked over less of the corpus than words were";
    case "unsupported":
      return "this knowledge backend ranks by words alone";
    default:
      return degraded.replace(/_/g, " ");
  }
}
