/**
 * The model's name and the coding CLI it runs.
 *
 * BOTH ARE THE MODEL'S IDENTITY ON AN EDIT (decision D4). Seats and the
 * assistant name the model by its name, and the engine refuses a rename; a
 * model on another CLI is another model. So an edit shows both, the name read
 * only and the other CLIs unavailable with the way to get there.
 *
 * THE CARDS SAY WHAT EACH CLI CAN DO, as "Jobs" (design QA M7): whether it can
 * run a seat's coding is the one difference that decides the rest.
 */

import { ChoiceCards, FormField, Input } from "@crewlethq/ui";
import { CliMark, MARK_SIZE } from "../../CliMark.tsx";
import { product } from "../../words.ts";
import { AGENTS, KEY_MAX, canExecute, type Agent, type Draft } from "../draft.ts";

const CLI_WORDS: Record<Agent, { description: string; methods: string }> = {
  "claude-code": {
    description: "Anthropic's coding CLI.",
    methods: "Key, token, login or cloud",
  },
  codex: {
    description: "OpenAI's coding CLI.",
    methods: "API key or ChatGPT",
  },
  opencode: {
    description: "An open CLI for many providers.",
    methods: "The provider's API key",
  },
};

export function CliStep({
  draft,
  editing,
  error,
  onName,
  onAgent,
}: {
  draft: Draft;
  editing: boolean;
  error: string | undefined;
  onName: (key: string) => void;
  onAgent: (agent: Agent) => void;
}) {
  const agents: readonly string[] = (AGENTS as readonly string[]).includes(draft.agent)
    ? AGENTS
    : [...AGENTS, draft.agent];
  return (
    <>
      <FormField
        label="Name"
        helper={
          editing
            ? "The name is this model's identity: seats name it. To use another name, add a new model."
            : "Lowercase letters, digits and hyphens. Seats and the assistant name the model by it."
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
        label="Coding CLI"
        value={draft.agent}
        onChange={(v) => onAgent(v as Agent)}
        options={agents.map((a) => {
          const words = CLI_WORDS[a as Agent];
          const other = editing && a !== draft.agent;
          return {
            value: a,
            label: product(a),
            media: <CliMark agent={a} size={MARK_SIZE} />,
            ...(words ? { description: words.description } : {}),
            facts: [
              { label: "Jobs", value: canExecute({ agent: a }) ? "All jobs" : "All but coding" },
              ...(words ? [{ label: "Method", value: words.methods }] : []),
            ],
            ...(other
              ? {
                  disabled: true,
                  disabledReason: `A model keeps its CLI. To run ${product(a)}, add a model on it, then remove this one with its jobs moved.`,
                }
              : {}),
          };
        })}
      />
    </>
  );
}
