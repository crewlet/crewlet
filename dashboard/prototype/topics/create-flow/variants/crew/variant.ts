import type { Variant } from "../../../../harness/defineTopic.ts";

const crew: Variant = {
  id: "crew",
  label: "3 · Crew",
  summary:
    "Crew. The Make row is the crew itself: six Crewlet characters standing on the org chart's dotted ground, one for each kind, with a line on who makes it. Pressing one slides in a panel beside the gallery that already holds the work: a doc's page with its title focused, a sheet's grid, the deck's cover, or a brief to the agent best suited. The gallery stays in view behind it, so making one is a step aside rather than a trip away.",
  modules: {
    "~/routes/artifacts/Gallery.tsx": "./variants/crew/Gallery.tsx",
    "~/routes/artifacts/parts.tsx": "./variants/crew/parts.tsx",
    "~/routes/artifacts/NewArtifact.tsx": "./variants/crew/NewArtifact.tsx",
  },
  css: ["./shared/shared.css", "./variants/crew/crew.css"],
};

export default crew;
