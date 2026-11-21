/**
 * A model's name from its id, through the engine's `llm_models` answer: an
 * alias by the model it follows (or its own name and version), a catalogue id
 * by the catalogue's name, and an id neither names by nothing, so the page says
 * the id itself. The question asked is the harness's, with the provider a
 * provider-named harness writes in front of the id.
 */

import { expect, test } from "vitest";
import type { ModelsAnswer } from "~/protocol/models.ts";
import { catalogueOf, nameIn, nameKey } from "./modelNames.ts";
import { modelEntry, modelsData } from "./fixtures.ts";

const ANSWER: Pick<ModelsAnswer, "aliases" | "models"> = {
  aliases: [
    {
      id: "opus",
      name: "Opus",
      detail: "The harness's latest Opus",
      model: "claude-opus-5-5",
      version: "5.5",
    },
    { id: "haiku", name: "Haiku", detail: "The harness's latest Haiku", version: "5.5" },
    { id: "best", name: "Best", detail: "Whatever the harness thinks best" },
  ],
  models: [
    { id: "claude-opus-5-5", name: "Claude Opus 5.5" },
    { id: "claude-haiku-5-5", name: "Claude Haiku 5.5" },
  ],
};

test("an id is named by its alias's model, the catalogue, or not at all", () => {
  // AN ALIAS BY THE MODEL IT FOLLOWS, as the catalogue names it.
  expect(nameIn(ANSWER, "opus")).toBe("Claude Opus 5.5");
  // AN ALIAS THE CATALOGUE CANNOT NAME: its own name and version, or its name.
  expect(nameIn(ANSWER, "haiku")).toBe("Haiku 5.5");
  expect(nameIn(ANSWER, "best")).toBe("Best");
  // A FULL ID by the catalogue's name.
  expect(nameIn(ANSWER, "claude-haiku-5-5")).toBe("Claude Haiku 5.5");
  // AN ID NOTHING NAMES, or no answer yet: no name, and the page says the id.
  expect(nameIn(ANSWER, "my-finetune-7")).toBeNull();
  expect(nameIn(undefined, "opus")).toBeNull();
});

test("the question asked is the harness's, with a provider-named harness's provider", () => {
  expect(catalogueOf("claude-code", "opus")).toEqual({ harness: "claude-code" });
  expect(catalogueOf("opencode", "anthropic/claude-sonnet-5-5")).toEqual({
    harness: "opencode",
    provider: "anthropic",
  });
  // NO PROVIDER IN THE ID, a custom harness or no id: nothing to ask.
  expect(catalogueOf("opencode", "claude-sonnet-5-5")).toBeNull();
  expect(catalogueOf("custom", "x")).toBeNull();
  expect(catalogueOf("codex", "")).toBeNull();
});

test("a model is said by its name where one is known, and by its id where none is", () => {
  const data = modelsData({
    entries: [
      modelEntry({ key: "sub-4", model: "opus" }),
      modelEntry({ key: "tuned", model: "my-finetune-7" }),
    ],
    names: new Map([[nameKey("claude-code", "opus"), "Claude Opus 5.5"]]),
  });
  expect(data.models.map((m) => m.modelName)).toEqual(["Claude Opus 5.5", "my-finetune-7"]);
});
