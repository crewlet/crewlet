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
import { fmtExact } from "./format.ts";

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

/**
 * The mode a search runs in when nobody chose one: the engine's default
 * (`hybrid`) where it is served as asked, and otherwise the FIRST mode the
 * engine says it serves.
 *
 * THE DEFAULT IS NEVER A MODE THE CONTROL SAYS IT CANNOT RUN. A company with no
 * embeddings provider serves keyword alone; left at `hybrid`, the mode control
 * drew its checked segment disabled — "you are using the mode you cannot use"
 * — and every search nobody chose a mode for was answered "asked for Hybrid,
 * served Keyword", a degradation the reader never asked to be told about. A
 * mode the reader DID ask for (`mode=` in the address) is theirs and is kept,
 * and the answer says what it served instead.
 *
 * Until something has said which modes exist, the default is the engine's:
 * "not known yet" never moves a choice.
 */
export function defaultSearchMode(
  answer: Pick<SearchOutcome, "modes"> | null | undefined,
): SearchMode {
  const modes = answer?.modes ?? [];
  if (modes.length === 0 || modes.includes("hybrid")) return "hybrid";
  return SEARCH_MODES.find((m) => modes.includes(m.value))?.value ?? "hybrid";
}

/**
 * The mode a search runs in: the one the address names, or the default
 * ([defaultSearchMode]) when it names none. An unknown `mode=` is the
 * engine's default, as [asSearchMode] reads it.
 */
export function resolveSearchMode(
  asked: string | null,
  answer: Pick<SearchOutcome, "modes"> | null | undefined,
): SearchMode {
  return asked ? asSearchMode(asked) : defaultSearchMode(answer);
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

/**
 * Why a mode cannot be served as asked here — or null when it can, or when
 * nothing has said which modes this backend serves yet.
 *
 * FROM THE ANSWER'S `modes`, which is the engine's own statement of what it
 * serves as asked, and the reason is its `degraded`: the answer a probe gives
 * (`knowledge` with no phrase, asked for `semantic`) names the CONFIGURATION
 * reason — no embeddings provider, or a backend with no such ranker. An answer
 * that names no modes has not said, and "unknown" never disables a control.
 */
export function modeUnavailable(
  answer: Pick<SearchOutcome, "modes" | "degraded"> | null | undefined,
  mode: SearchMode,
): string | null {
  const modes = answer?.modes ?? [];
  if (modes.length === 0 || modes.includes(mode)) return null;
  const why = degradedWhy(answer?.degraded ?? "");
  return why
    ? `${modeLabel(mode)} is unavailable: ${why}`
    : `${modeLabel(mode)} is unavailable here`;
}

/**
 * What a ranked search did NOT cover, in a sentence — or null when it covered
 * everything it was asked to, or ran nothing (which [servedNote] says).
 *
 * A SEARCH SLICE IS NOT A NODE'S EVENTS. Every node holds the whole corpus and
 * scans a range of its buckets; a node that did not answer is a range nobody
 * scanned, so the sentence is about PAGES THAT MAY MATCH AND ARE NOT LISTED —
 * never "what that node published", which is the history answers' shape.
 */
export function searchCoverageNote(
  answer: Pick<SearchOutcome, "served_mode" | "coverage"> | null | undefined,
): { sentence: string; nodes: { id: string; error: string }[] } | null {
  if (!answer?.coverage || answer.coverage.complete) return null;
  const nodes = (answer.coverage.nodes ?? [])
    .filter((n) => !n.answered)
    .map((n) => ({ id: n.id, error: n.error }));
  const missing = answer.coverage.buckets_missing ?? 0;
  if (!answer.served_mode) {
    // NOTHING RAN AND IT IS NOT COMPLETE: the backend was asked and did
    // not answer — a vendor site that failed, or this node's own scan.
    return {
      sentence:
        "The search did not complete — the backend did not answer — so an empty list here is not proof that nothing matches.",
      nodes,
    };
  }
  const part =
    missing > 0
      ? `${fmtExact(missing)} of the corpus's buckets went unsearched`
      : "part of the fleet did not answer";
  return {
    sentence: `This search is partial: ${part}, so pages there may match and are not listed.`,
    nodes,
  };
}
