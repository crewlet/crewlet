/**
 * The model's name and the harness it runs.
 *
 * BOTH ARE THE MODEL'S IDENTITY ON AN EDIT (decision D4). Seats and the
 * assistant name the model by its name, and the engine refuses a rename; a
 * model on another harness is another model. So an edit shows both, the name read
 * only and the other harnesses unavailable with the way to get there.
 *
 * THE CARDS SAY WHAT EACH HARNESS CAN DO, as "Jobs" (design QA M7): whether it can
 * run a seat's coding is the one difference that decides the rest.
 */

import { ChoiceCards, FormField, Input } from "@crewlethq/ui";
import { HarnessMark, MARK_SIZE } from "../../HarnessMark.tsx";
import { product } from "../../words.ts";
import { HARNESSES, KEY_MAX, canExecute, type Harness, type Draft } from "../draft.ts";

const HARNESS_WORDS: Record<Harness, { description: string; methods: string }> = {
  "claude-code": {
    description: "Anthropic's harness.",
    methods: "Key, token, login or cloud",
  },
  codex: {
    description: "OpenAI's harness.",
    methods: "API key or ChatGPT",
  },
  opencode: {
    description: "An open harness for many providers.",
    methods: "The provider's API key",
  },
};

export function HarnessStep({
  draft,
  editing,
  error,
  onName,
  onHarness,
}: {
  draft: Draft;
  editing: boolean;
  error: string | undefined;
  onName: (key: string) => void;
  onHarness: (harness: Harness) => void;
}) {
  const harnesses: readonly string[] = (HARNESSES as readonly string[]).includes(draft.harness)
    ? HARNESSES
    : [...HARNESSES, draft.harness];
  return (
    <>
      <FormField
        label="Name"
        helper={
          editing
            ? "The name is this model's identity: seats name it. To use another name, add a new model."
            : "Lowercase letters, digits and hyphens. Seats and Ask Crewlet name the model by it."
        }
        error={error}
        {...(editing ? {} : { limit: { length: draft.key.length, max: KEY_MAX } })}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            aria-required
            error={field.invalid}
            appearance="reference"
            value={draft.key}
            placeholder="sonnet-key"
            readOnly={editing}
            autoComplete="off"
            spellCheck={false}
            onChange={(e) => onName(e.target.value)}
          />
        )}
      </FormField>
      <ChoiceCards
        label="Harness"
        minCardWidth="sm"
        density="compact"
        value={draft.harness}
        onChange={(v) => onHarness(v as Harness)}
        options={harnesses.map((a) => {
          const words = HARNESS_WORDS[a as Harness];
          const other = editing && a !== draft.harness;
          return {
            value: a,
            label: product(a),
            media: <HarnessMark harness={a} size={MARK_SIZE} />,
            ...(words ? { description: words.description } : {}),
            facts: [
              { label: "Jobs", value: canExecute({ harness: a }) ? "All jobs" : "All but coding" },
              ...(words ? [{ label: "Method", value: words.methods }] : []),
            ],
            ...(other
              ? {
                  disabled: true,
                  disabledReason: `A model keeps its harness. To run ${product(a)}, add a model on it, then remove this one with its jobs moved.`,
                }
              : {}),
          };
        })}
      />
    </>
  );
}
