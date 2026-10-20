/**
 * The topic API: what a design topic declares, and the one function that
 * declares it.
 *
 * A TOPIC is one design question (the agents roster, the integrations
 * catalogue) asked as several VARIANTS of the real dashboard, each a build
 * that swaps specific modules for a proposal and otherwise runs the shipped
 * screens, the shipped stylesheets and the shipped `@crewlethq/ui`, on a demo
 * company with no engine behind it. A topic also names its SCENARIOS: the
 * states of that company worth looking at a proposal in (as today, a busy
 * company, a first day), switched at runtime from the variant bar.
 *
 * The declaration is read twice, by two different programs: `build.mjs` reads
 * `modules`, `css` and `uilet` in Node to decide what to bundle, and the
 * bundle itself reads the labels, the summaries and the scenarios in the
 * browser. So everything here is a plain value, validated once by
 * [defineTopic] wherever it is evaluated, and a path is a STRING the build
 * resolves rather than an import the bundle would follow.
 */

import type { DemoCompany } from "./engine/company.ts";

/** One state of the demo company a topic is worth looking at. */
export interface Scenario {
  /** A slug, unique within the topic, and what the variant bar remembers. */
  readonly id: string;
  /** What the variant bar calls it. */
  readonly label: string;
  /**
   * The scenario, as a PURE function of the base company: it returns the
   * company to show and never mutates the one it is given. The helpers in
   * `fixtures/scenarios.ts` compose.
   */
  readonly apply: (company: DemoCompany) => DemoCompany;
}

/** One build of the dashboard: the shipped one, or a proposal. */
export interface Variant {
  /** A slug, unique within the topic. It is the variant's folder in the build. */
  readonly id: string;
  /** What the variant bar calls it, short: `A · One list`. */
  readonly label: string;
  /** One line saying what this variant proposes. */
  readonly summary: string;
  /**
   * Dashboard modules swapped for a replacement, at build time and for every
   * importer. The key names the shipped module by its path under the
   * dashboard (`src/routes/agents/People.tsx`, or `~/routes/agents/People.tsx`);
   * the value is the replacement, relative to the topic's own folder. A
   * replacement reaches the module it replaces through `@original/…`
   * (`@original/routes/agents/People.tsx`), which bypasses every swap.
   */
  readonly modules?: Readonly<Record<string, string>>;
  /** Stylesheets loaded after every stylesheet the app ships, relative to the topic's folder. */
  readonly css?: readonly string[];
  /**
   * A local uilet checkout to build this variant against, instead of the
   * `@crewlethq/*` packages the dashboard installs: absolute, or relative to
   * the topic's folder. The checkout must have run `npm ci --ignore-scripts`
   * and `npm run build`, as its README says.
   */
  readonly uilet?: string;
}

/** A design topic: one question, its variants and the company states it is looked at in. */
export interface Topic {
  /** A slug, and the folder name under `topics/`. */
  readonly id: string;
  /** Two to four words: the launcher page's title. */
  readonly title: string;
  /** The dashboard address every variant opens on, `#/agents`. */
  readonly start: string;
  /** The first is the default. */
  readonly scenarios: readonly Scenario[];
  /** The first is `today`: the dashboard as it ships, swapping nothing. */
  readonly variants: readonly Variant[];
  /**
   * Folders copied into the build as they are, shared by every variant: the
   * key is the folder in the build (`frames`), the value its source,
   * relative to the topic's folder. For what the engine serves from another
   * origin, such as the pages an artifact frame opens.
   */
  readonly static?: Readonly<Record<string, string>>;
}

/** The variant every topic starts with, and the one its launcher opens. */
export const TODAY = "today";

const SLUG = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/;

/** Every reason `topic` is not a topic, or an empty list. */
/**
 * The longest label a scenario or a variant may carry. Each is drawn on the
 * variant bar as one control, a trigger or a chip, which reads as a name at
 * this length and as a sentence past it. A dozen characters ("A · One list")
 * keeps six variants on one row at a laptop's width; longer ones wrap.
 */
export const LABEL_MAX = 32;

export function topicProblems(topic: Topic): string[] {
  const problems: string[] = [];
  if (!SLUG.test(topic.id)) problems.push(`id "${topic.id}" is not a lowercase slug`);
  const words = topic.title.trim().split(/\s+/).filter(Boolean).length;
  if (words < 2 || words > 4) problems.push(`title "${topic.title}" is not two to four words`);
  if (!topic.start.startsWith("#/")) problems.push(`start "${topic.start}" does not begin with #/`);

  if (topic.scenarios.length === 0) problems.push("it declares no scenario");
  const scenarios = new Set<string>();
  for (const scenario of topic.scenarios) {
    if (!SLUG.test(scenario.id)) problems.push(`scenario id "${scenario.id}" is not a slug`);
    if (scenarios.has(scenario.id)) problems.push(`scenario "${scenario.id}" is declared twice`);
    scenarios.add(scenario.id);
    if (!scenario.label.trim()) problems.push(`scenario "${scenario.id}" has no label`);
    if (scenario.label.length > LABEL_MAX) {
      problems.push(`scenario "${scenario.id}"'s label is longer than ${LABEL_MAX} characters`);
    }
    if (typeof scenario.apply !== "function") {
      problems.push(`scenario "${scenario.id}" has no apply function`);
    }
  }

  const first = topic.variants[0];
  if (!first || first.id !== TODAY) problems.push(`the first variant is not "${TODAY}"`);
  if (first && first.id === TODAY && (first.modules || first.css || first.uilet)) {
    problems.push(`"${TODAY}" is the dashboard as it ships, so it swaps nothing`);
  }
  const variants = new Set<string>();
  for (const variant of topic.variants) {
    if (!SLUG.test(variant.id)) problems.push(`variant id "${variant.id}" is not a slug`);
    if (variants.has(variant.id)) problems.push(`variant "${variant.id}" is declared twice`);
    variants.add(variant.id);
    if (!variant.label.trim()) problems.push(`variant "${variant.id}" has no label`);
    if (variant.label.length > LABEL_MAX) {
      problems.push(`variant "${variant.id}"'s label is longer than ${LABEL_MAX} characters`);
    }
    if (!variant.summary.trim() || /\n/.test(variant.summary)) {
      problems.push(`variant "${variant.id}" needs a one-line summary`);
    }
  }
  for (const dest of Object.keys(topic.static ?? {})) {
    const parts = dest.split("/");
    if (parts.some((part) => !/^[_a-z0-9][_a-z0-9.-]*$/.test(part))) {
      problems.push(`static folder "${dest}" is not a relative path of plain names`);
    }
    if (parts[0] === "fonts" || topic.variants.some((v) => v.id === parts[0])) {
      problems.push(`static folder "${dest}" would overwrite the build's own "${parts[0]}"`);
    }
  }
  return problems;
}

/** Declare a topic. Throws, naming every problem, on a declaration that is not one. */
export function defineTopic(topic: Topic): Topic {
  const problems = topicProblems(topic);
  if (problems.length > 0) {
    throw new Error(`The topic "${topic.id}" is not valid: ${problems.join("; ")}.`);
  }
  return topic;
}
