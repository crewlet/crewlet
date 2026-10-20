import type { Variant } from "../../../../harness/defineTopic.ts";

const studio: Variant = {
  id: "studio",
  label: "1 · Studio",
  summary:
    "Studio. Each kind keeps its glyph and colour, and its tile shows the Crewlet character that makes it presenting the kind on the org chart's dotted canvas. A tile opens a full studio for that kind: a doc opens as a blank page with its title focused, a sheet as its live grid with starting points beside it, slides on the deck's cover ready to type, and the three kinds agents build as a brief to the agent best suited, with who builds it and what happens next.",
  modules: {
    "~/routes/artifacts/Gallery.tsx": "./variants/studio/Gallery.tsx",
    "~/routes/artifacts/parts.tsx": "./variants/studio/parts.tsx",
    "~/routes/artifacts/NewArtifact.tsx": "./variants/studio/NewArtifact.tsx",
  },
  css: ["./shared/shared.css", "./variants/studio/studio.css"],
};

export default studio;
