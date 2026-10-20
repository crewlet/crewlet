import type { Variant } from "../../../../harness/defineTopic.ts";

const composer: Variant = {
  id: "composer",
  label: "2 · Composer",
  summary:
    "Composer. The top of the gallery is one card asking what to make, its kinds a row of chips, each led by the character that makes it. Choosing one opens its create experience in the card itself, above the work it will join: a doc's page with its title focused, a sheet's grid with its starting points as chips, the deck's cover on its stage, or a brief with suggested asks and the agent who builds it. Nothing leaves the gallery until it is made.",
  modules: {
    "~/routes/artifacts/Gallery.tsx": "./variants/composer/Gallery.tsx",
    "~/routes/artifacts/parts.tsx": "./variants/composer/parts.tsx",
    "~/routes/artifacts/NewArtifact.tsx": "./variants/composer/NewArtifact.tsx",
  },
  css: ["./shared/shared.css", "./variants/composer/composer.css"],
};

export default composer;
