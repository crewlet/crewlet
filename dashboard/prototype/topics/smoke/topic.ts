/**
 * The harness's own proof: Today; a variant that wraps two shipped screens,
 * one of them drawing a known dashboard bug, and adds one stylesheet rule; a
 * variant built against a local uilet checkout; and a variant that replaces
 * two shipped modules outright, one of them the one module spelling an
 * engine-relative URL and the other the one holding that known bug. It is
 * what `check/` runs and what `build.mjs` is first proved on.
 */

import { defineTopic } from "../../harness/defineTopic.ts";
import { compose, resumeAll, scenario, seatWorking } from "../../fixtures/scenarios.ts";

export default defineTopic({
  id: "smoke",
  title: "Prototype harness smoke",
  start: "#/settings/appearance",
  scenarios: [
    scenario("today", "As today", (c) => c),
    scenario(
      "working",
      "Both agents working",
      compose(resumeAll, seatWorking("pm", "FAIR-1"), seatWorking("engineer", "FAIR-3")),
    ),
  ],
  variants: [
    { id: "today", label: "Today", summary: "The dashboard as it ships." },
    {
      id: "a",
      label: "A · Swapped screen",
      summary:
        "Settings › Appearance and Settings › Integrations wrapped by replacement modules, with one extra stylesheet rule.",
      modules: {
        "src/routes/settings/Appearance.tsx": "./variants/a/Appearance.tsx",
        "src/routes/settings/Integrations.tsx": "./variants/a/Integrations.tsx",
      },
      css: ["./variants/a/a.css"],
    },
    {
      id: "b",
      label: "B · Local uilet",
      summary: "The dashboard built against a local uilet checkout's packages.",
      // The throwaway uilet worktree beside this engine worktree; see the README.
      uilet: "../../../../../uilet",
    },
    {
      id: "c",
      label: "C · Replaced modules",
      summary:
        "The sidebar replaced without importing the shipped one, which holds the one engine-relative URL, and Settings › Integrations drawn by a screen of the topic's own.",
      modules: {
        "src/app/sidebar/Sidebar.tsx": "./variants/c/Sidebar.tsx",
        "~/routes/settings/Integrations.tsx": "./variants/c/Integrations.tsx",
      },
    },
  ],
});
