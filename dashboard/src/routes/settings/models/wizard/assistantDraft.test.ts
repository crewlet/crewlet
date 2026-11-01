import { describe, expect, it } from "vitest";
import { isSecretDraftPath } from "~/app/assistant/draft.ts";
import { emptyDraft } from "./draft.ts";
import { modelDraftFields } from "./assistantDraft.ts";

describe("the model wizard's draft, as the assistant reads it", () => {
  // A PASTED KEY NEVER LEAVES THE PAGE: no field reads the credential or the
  // secret it is stored under, and none names a path the engine treats as one.
  it("carries no credential and no secret's name", () => {
    const draft = {
      ...emptyDraft(),
      key: "opus",
      credential: "sk-ant-api03-SECRET-VALUE",
      variable: "ANTHROPIC_API_KEY_OPUS",
    };
    const fields = modelDraftFields(["pm"]);
    for (const field of fields) {
      expect(isSecretDraftPath(field.path), field.path).toBe(false);
      const value = String(field.read(draft));
      expect(value, field.path).not.toContain("SECRET-VALUE");
      expect(value, field.path).not.toContain("ANTHROPIC_API_KEY_OPUS");
    }
  });

  // READ, NEVER SET: a change goes through the wizard's own step, where a
  // sign-in is tested before anything is stored.
  it("offers the assistant nothing to set", () => {
    expect(modelDraftFields([]).every((f) => f.write === undefined && !f.file)).toBe(true);
  });
});
