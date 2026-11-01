/**
 * What is left of the component library once the dashboard draws
 * `@crewlethq/ui`.
 *
 * Everything with a peer in the design system is gone — the badge, the panel,
 * the empty state, the stat row, the skeleton, the select, the tabs, the
 * avatar, the banner, the disclosure, the search input, the button, the chip
 * and `cx` itself all come from the package now, and a second recipe for a
 * mark the package already draws is exactly the shape this file was written to
 * stop being.
 *
 * WHAT SURVIVES IS OF TWO KINDS, and the distinction is the useful part of
 * this file:
 *
 *  1. A VOCABULARY the package does not have and should not: `Tone`, the six
 *     words the engine's own payloads are spelled in, and the single
 *     translation of them into the package's spelling.
 *  2. A TABLE, not a recipe: `PhaseTag` draws the package's `Tag` and adds
 *     the one thing the package cannot know — which of the SIX phase strings
 *     the engine emits map onto the three hues it publishes, and that the
 *     other three take neutral.
 * THE OTHERS LEFT when the package grew what they were waiting for, and the
 * note is here because the list above is only honest if things come off it:
 * `Segmented` went to `SegmentedControl` once it took `activate="manual"`, a
 * capped count, a disabled option that says why, and a stop that returns to
 * the choice when the value changes from outside the row; `Code` went to
 * `CodeBlock`'s `focusWhenScrollable`, the clipboard write to
 * the package's `writeClipboard`, and `CopyButton`, `DownloadButton` and
 * `downloadText` to the package's own, whose refusal holds until the next
 * press and whose every attempt is announced.
 */

import { EmptyValue, Tag, type Tone as UiletTone } from "@crewlethq/ui";

// ---------------------------------------------------------------------------
// Vocabulary
// ---------------------------------------------------------------------------

/**
 * The six words this product says about state.
 *
 * Kept although `@crewlethq/ui` has its own six, because they are the words
 * the ENGINE uses: an event's severity, a seat's health, a reconcile finding
 * and a tool's status all reach a screen spelled this way, and a dashboard
 * carrying the package's spelling instead would be translating at the point
 * where the data ARRIVES rather than at the point where it is drawn.
 */
export type Tone = "neutral" | "positive" | "caution" | "critical" | "info" | "accent";

/**
 * Our tone vocabulary, said in uilet's.
 *
 * The two sets name the same six ideas and agree on what they are FOR — uilet's
 * own tone doc carries this product's rule verbatim ("a tone says what a thing
 * IS, never who it is") — but they spell three of them differently: our
 * `positive`, `caution` and `critical` are their `success`, `warning` and
 * `danger`, and our `accent` is their `brand`.
 *
 * ONE TRANSLATION, not one per call site. The engine side of this repository
 * has paid for the alternative three times (`textcut`, `whsec`, `jsprovision`):
 * a rule spelled at each site is a rule whose copies come to disagree, and the
 * disagreement here is silent — a wrong `variant` string renders the neutral
 * pill rather than failing, so a caution reads as "nothing in particular" and
 * nothing in the build says so.
 *
 * It lives BESIDE `Tone` because that is the only place both spellings are in
 * view: a reader adding a seventh word to the union sees, in the next
 * paragraph, that it needs a uilet word too — and the `Record` below fails the
 * build if they do not give it one.
 */
const SPELLING: Record<Tone, UiletTone> = {
  neutral: "neutral",
  positive: "success",
  caution: "warning",
  critical: "danger",
  info: "info",
  accent: "brand",
};

/** The uilet variant a tone of ours is drawn as. */
export function uiletTone(tone: Tone): UiletTone {
  return SPELLING[tone];
}

// ---------------------------------------------------------------------------
// Marks
// ---------------------------------------------------------------------------

/**
 * A phase mark: the neutral pill with the phase's word.
 *
 * A PHASE IS A CATEGORY, AND A CATEGORY HAS NO COLOUR. The design system
 * removed its phase family in 0.5.0 for the reason this product gives for every
 * other identity: beside a state badge the state is the one coloured thing on
 * the row, and the phase is the word that says where the work is. Inside a
 * FIGURE a phase is a series (`lib/spend.ts`' `phaseColor`); on a row it is a
 * word, and every phase — the engine's six, and one a newer peer sends — is
 * drawn the same way, which is the behaviour a rolling upgrade needs.
 */
export function PhaseTag({ phase }: { phase: string }) {
  const key = (phase || "").toLowerCase();
  return <Tag variant="neutral">{key || <EmptyValue label="No phase on this record" />}</Tag>;
}
