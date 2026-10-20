/**
 * What a prototype remembers for the length of a browser tab: which scenario
 * the reader chose, the demo company as their writes left it, and whether the
 * variant bar is folded away.
 *
 * Switching variants loads another page, so without this every switch would
 * start the company over, and a reader comparing two proposals of the same
 * state (an agent they just paused, a decision they just answered) would have
 * to make the change again in each. Reset is the way back to the scenario as
 * written.
 *
 * Session storage for the company, local storage for the bar's fold, and
 * EVERY ACCESS WRAPPED: an artifact frame may refuse storage outright, and a
 * prototype that cannot remember still has to boot. The keys are the
 * prototype's own and never the dashboard's (`src/lib/storage.ts`).
 */

import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { Topic } from "./defineTopic.ts";
import type { DemoCompany } from "./engine/company.ts";

/**
 * The demo company's format, so a tab holding a company another build stored
 * starts over instead of misreading it. A digest of the code that builds a
 * company, given by the build (`companyFormat`), so it moves whenever that
 * code does.
 */
const FORMAT = __PROTOTYPE_COMPANY_FORMAT__;

const companyKey = (topic: Topic) => `crewlet-prototype:${topic.id}:state`;
const BAR_KEY = "crewlet-prototype:bar";
/** The dashboard's own single-pane width. */
const PHONE = `(width < ${PHONE_BREAKPOINT}px)`;

interface Saved {
  format: string;
  scenario: string;
  company: DemoCompany | null;
}

function storage(area: "sessionStorage" | "localStorage"): Storage | null {
  try {
    return globalThis[area] ?? null;
  } catch {
    return null;
  }
}

/** The scenario and company this tab left the topic in, or nothing. */
export function loadSession(
  topic: Topic,
): { scenario: string; company: DemoCompany | null } | null {
  try {
    const raw = storage("sessionStorage")?.getItem(companyKey(topic));
    if (!raw) return null;
    const saved = JSON.parse(raw) as Saved;
    if (!topic.scenarios.some((s) => s.id === saved.scenario)) return null;
    // A COMPANY ANOTHER BUILD STORED is started over; the scenario chosen is kept.
    return { scenario: saved.scenario, company: saved.format === FORMAT ? saved.company : null };
  } catch {
    return null;
  }
}

/** Remember the scenario, and the company as it stands (null to rebuild it from the scenario). */
export function saveSession(topic: Topic, scenario: string, company: DemoCompany | null): void {
  try {
    const saved: Saved = { format: FORMAT, scenario, company };
    storage("sessionStorage")?.setItem(companyKey(topic), JSON.stringify(saved));
  } catch {
    // Refused or full: the demo runs on, and a switch starts the scenario over.
  }
}

/**
 * Start the demo over from `scenario` as written, for the next page load: the
 * company this tab's writes left, and every draft of it, forgotten; the
 * scenario kept. Choosing a scenario and Reset both come here, so a reset
 * reopens the scenario the reader chose rather than the topic's first.
 */
export function startOver(topic: Topic, scenario: string): void {
  saveSession(topic, scenario, null);
  // THE ORG BUILDER'S UNSAVED DRAFT IS DEMO STATE TOO: it is a draft of the
  // demo company, and a start over that kept it would reopen the editor on
  // changes to a company that no longer has them.
  try {
    storage("sessionStorage")?.removeItem(STORAGE_KEYS.orgDraft);
  } catch {
    // Refused: there is no draft to keep.
  }
}

/**
 * What the variant bar's scenario menu and its Reset do to the tab: start the
 * demo over from a scenario and reload into it. A chosen scenario starts over
 * from that one; Reset starts over from the one on screen.
 */
export function scenarioActions(
  topic: Topic,
  scenario: string,
  reload: () => void = () => location.reload(),
): { onScenario: (id: string) => void; onReset: () => void } {
  const restart = (id: string) => {
    startOver(topic, id);
    reload();
  };
  return { onScenario: restart, onReset: () => restart(scenario) };
}

/**
 * Forget who this tab was read by (`lib/reader.ts`), for a company built
 * afresh from its scenario.
 *
 * A SCENARIO SAYS WHO THE BROWSER IS SIGNED IN AS, and one may name somebody
 * other than the last. The dashboard hands a tab whose session now belongs to
 * another person over to them by emptying the tab's storage and reloading
 * (`takeSession` in `lib/session.ts`), which would take the chosen scenario
 * with it and boot the topic's first in its place. A company started over is
 * a fresh tab, so its session is adopted as the tab's first reader instead.
 */
export function freshReader(): void {
  try {
    storage("sessionStorage")?.removeItem(STORAGE_KEYS.reader);
  } catch {
    // Refused: no reader was kept.
  }
}

/**
 * Whether the variant bar is folded away: as the reader last left it, and
 * folded on a phone until they open it, where it would cover a third of the
 * screen it is beside.
 */
export function barFolded(): boolean {
  try {
    const said = storage("localStorage")?.getItem(BAR_KEY);
    if (said === "folded" || said === "open") return said === "folded";
  } catch {
    // Nothing said; fall through to the width.
  }
  try {
    return window.matchMedia(PHONE).matches;
  } catch {
    return false;
  }
}

export function setBarFolded(folded: boolean): void {
  try {
    storage("localStorage")?.setItem(BAR_KEY, folded ? "folded" : "open");
  } catch {
    // The fold lasts until the next page.
  }
}

/**
 * The variant a reader last had open, by topic, so the launcher reopens it:
 * a published topic is one link, and a reader comparing variants comes back
 * to the one they were on rather than to Today. The launcher (build.mjs)
 * reads the same key, spelled by [lastVariantKey].
 */
export function lastVariantKey(topicId: string): string {
  return `crewlet-prototype:${topicId}:variant`;
}

export function rememberVariant(topicId: string, variantId: string): void {
  try {
    storage("localStorage")?.setItem(lastVariantKey(topicId), variantId);
  } catch {
    // Not kept: the launcher opens Today.
  }
}
