// @ts-check
/**
 * The route check's runner: one topic's one variant, its module graph built
 * exactly as its page is (the same swaps, the same uilet resolution), mounted
 * in jsdom. `check.mjs` runs it once per variant; to run one by hand:
 *
 *     PROTO_TOPIC=smoke PROTO_VARIANT=a node scripts/owned-tmpdir.mjs \
 *       npx vitest run --config prototype/check/vitest.config.mjs
 */

import { realpathSync } from "node:fs";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";
import {
  checkPlugins,
  companyFormat,
  DASHBOARD,
  loadTopic,
  planVariant,
  SRC,
  swappedModules,
} from "../harness/vite.mjs";

export default defineConfig(async () => {
  const topic = process.env.PROTO_TOPIC ?? "smoke";
  const variant = process.env.PROTO_VARIANT ?? "today";
  const plan = planVariant(await loadTopic(topic), variant);
  return {
    root: DASHBOARD,
    plugins: [...checkPlugins(plan), react()],
    resolve: { alias: { "~": SRC }, dedupe: ["react", "react-dom"] },
    define: {
      __PROTOTYPE_TOPIC__: JSON.stringify(plan.topicFile),
      __PROTOTYPE_VARIANT__: JSON.stringify(variant),
      // Where the shipped modules live, as a module's own path spells it, and
      // which of them this variant swaps out: the route check attributes a
      // known dashboard bug to the module that drew it by both.
      __PROTOTYPE_DASHBOARD__: JSON.stringify(realpathSync(DASHBOARD)),
      __PROTOTYPE_SWAPPED__: JSON.stringify(swappedModules(plan)),
      __PROTOTYPE_COMPANY_FORMAT__: JSON.stringify(companyFormat()),
    },
    test: {
      environment: "jsdom",
      globals: false,
      // THE ROUTE WALK RUNS ON EVERY VARIANT; everything else is about the
      // harness and the base company, and runs once, with the shipped screens.
      include:
        variant === "today"
          ? ["prototype/check/**/*.test.ts", "prototype/check/**/*.test.tsx"]
          : ["prototype/check/routes.test.tsx"],
      setupFiles: ["./src/test/setup.ts"],
      // The kit's modules import their stylesheets, which Node cannot load;
      // inlined, they go through Vite, as the dashboard's own suites do.
      server: { deps: { inline: [/@crewlethq\//] } },
      testTimeout: 120_000,
      hookTimeout: 120_000,
    },
  };
});
