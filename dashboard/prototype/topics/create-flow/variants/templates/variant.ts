import type { Variant } from "../../../../harness/defineTopic.ts";

const templates: Variant = {
  id: "templates",
  label: "4 · Templates",
  summary:
    "Templates. The Make row is a shelf of real starting points, each drawn live by the renderer it will open in: a doc's first page, a deck's cover, a sheet's first rows, and for what agents build, something an agent here already made from a brief like it. A kind filter narrows the shelf. Choosing one lands in its editor already filled, with the other starting points one row above to swap, or for an agent's kind, in a brief written for you with the example beside it.",
  modules: {
    "~/routes/artifacts/Gallery.tsx": "./variants/templates/Gallery.tsx",
    "~/routes/artifacts/parts.tsx": "./variants/templates/parts.tsx",
    "~/routes/artifacts/NewArtifact.tsx": "./variants/templates/NewArtifact.tsx",
  },
  css: ["./shared/shared.css", "./variants/templates/templates.css"],
};

export default templates;
