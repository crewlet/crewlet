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

import { ChoiceCards, FormField, Input, Text } from "@crewlethq/ui";
import { EXECUTOR_HARNESSES } from "~/contract/models.ts";
import { HarnessMark, MARK_SIZE } from "../../HarnessMark.tsx";
import { product } from "../../words.ts";
import { HARNESSES, KEY_MAX, canExecute, factsOf, type Harness, type Draft } from "../draft.ts";

/** Each harness in a line, and how it signs in, in the words the Method step uses. */
const HARNESS_WORDS: Readonly<Record<string, { description: string; methods: string }>> = {
  "claude-code": { description: "Anthropic's harness.", methods: "Key, token, login or cloud" },
  codex: { description: "OpenAI's harness.", methods: "API key or ChatGPT" },
  opencode: {
    description: "An open harness for many providers.",
    methods: "A provider's key or login",
  },
  copilot: { description: "GitHub's harness.", methods: "GitHub token or login" },
  "cursor-agent": { description: "Cursor's harness.", methods: "API key or login" },
  "gemini-cli": { description: "Google's harness.", methods: "API key or login" },
  grok: { description: "xAI's harness.", methods: "API key or login" },
  hermes: {
    description: "Nous Research's harness for many providers.",
    methods: "A provider's key or login",
  },
  "kimi-code": { description: "Moonshot AI's harness.", methods: "Login" },
  "muse-code": { description: "Meta's harness.", methods: "API key or login" },
  pi: { description: "An open harness for many providers.", methods: "A provider's key or login" },
  "qwen-code": {
    description: "Qwen's harness, on any OpenAI-compatible endpoint.",
    methods: "API key",
  },
};

/** "A or B", "A, B or C". */
const either = (parts: readonly string[]) =>
  parts.length < 3 ? parts.join(" or ") : `${parts.slice(0, -1).join(", ")} or ${parts.at(-1)}`;

/** What a harness with no box runner does: its turns on the host, and Ask Crewlet where the engine allows it. */
export function hostOnly(harness: string): string {
  const name = product(harness);
  const coding = either(EXECUTOR_HARNESSES.map(product));
  if (factsOf(harness).assistant)
    return `${name} runs a seat's turns on the engine host and Ask Crewlet. A seat's coding runs in a box on ${coding}.`;
  const assistant = either(HARNESSES.filter((h) => factsOf(h).assistant).map(product));
  return `${name} runs a seat's turns on the engine host. A seat's coding runs in a box on ${coding}, and Ask Crewlet on ${assistant}.`;
}

/** The value of the custom harness's card, which is never chosen here. */
const CUSTOM = "custom";

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
  const harnesses: readonly string[] = HARNESSES.includes(draft.harness)
    ? [...HARNESSES, CUSTOM]
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
          const words = HARNESS_WORDS[a];
          const other = editing && a !== draft.harness;
          if (a === CUSTOM && !editing) {
            return {
              value: a,
              label: "Custom",
              media: <HarnessMark harness={a} size={MARK_SIZE} />,
              description: "A harness of your own.",
              facts: [{ label: "Jobs", value: "All but coding" }],
              disabled: true,
              disabledReason:
                "A custom harness needs its own profile: its binary, flags and output. Write it in Settings, Configuration.",
            };
          }
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
      {canExecute(draft) ? null : (
        <Text as="p" variant="caption" tone="secondary">
          {hostOnly(draft.harness)}
        </Text>
      )}
    </>
  );
}
