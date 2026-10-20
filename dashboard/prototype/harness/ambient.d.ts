/**
 * What the prototype's module graph imports that TypeScript cannot resolve on
 * its own.
 */

/** The shipped module behind a swap, reached past it (see `harness/vite.mjs`). */
declare module "*?original";

/** The design system's one sheet, answered by `vite.config.ts` or by the uilet plugin. */
declare module "virtual:crewlet-design-system.css";

/**
 * jsdom, which the dashboard's checks already run on and which ships no
 * types: the one class `harness/publish.mjs` parses an SVG with in Node.
 */
declare module "jsdom" {
  export class JSDOM {
    constructor(html?: string);
    readonly window: Window & typeof globalThis;
  }
}

/** The topic module and the variant a check runs, defined by `check/vitest.config.mjs`. */
declare const __PROTOTYPE_TOPIC__: string;
declare const __PROTOTYPE_VARIANT__: string;
/** The demo company's format, a digest of the code that builds it (`companyFormat` in `harness/vite.mjs`). */
declare const __PROTOTYPE_COMPANY_FORMAT__: string;
/** `dashboard/`, resolved, and the shipped modules the variant swaps out under it, for the same checks. */
declare const __PROTOTYPE_DASHBOARD__: string;
declare const __PROTOTYPE_SWAPPED__: readonly string[];
