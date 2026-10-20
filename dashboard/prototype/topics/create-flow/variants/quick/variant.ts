import type { Variant } from "../../../../harness/defineTopic.ts";

const quick: Variant = {
  id: "quick",
  label: "5 · Quick",
  summary:
    "Quick. The Make row shrinks to one line of kinds, each with its character and a key (D, S, T, G, B, H) that works anywhere on the gallery. A kind opens straight into a quiet, full-width editor with nothing around it but the draft's state and Create: a doc's title takes the caret, a sheet's grid is ready to fill, the deck's cover heading is selected. For what agents build it is one centred ask, its builder named in the same line. Command and Enter makes it.",
  modules: {
    "~/routes/artifacts/Gallery.tsx": "./variants/quick/Gallery.tsx",
    "~/routes/artifacts/parts.tsx": "./variants/quick/parts.tsx",
    "~/routes/artifacts/NewArtifact.tsx": "./variants/quick/NewArtifact.tsx",
  },
  css: ["./shared/shared.css", "./variants/quick/quick.css"],
};

export default quick;
