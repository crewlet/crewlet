/**
 * The model the harness runs, in its own naming: the names the harness resolves
 * itself (its aliases) as choices, and "Another model" for any other id,
 * which suggests what the public catalogue lists and links the harness's own page
 * of model names.
 *
 * ONE CONTROL FOR ONE VALUE (design QA Minor 5). The free-text field is the
 * panel of the "Another model" choice and is empty unless that choice is
 * made, so an alias picked above is never echoed in a second field.
 *
 * AN ALIAS IS ONLY ITS OWN ID. A model on `claude-haiku-4-5` is not on the
 * `haiku` alias: the alias follows the harness's latest Haiku, and the full id
 * stays on that release. So a full id opens under "Another model", which
 * names it as the catalogue does and says it is a fixed version, rather than
 * under the alias it resembles, which would move the model on its next save.
 *
 * AN ALIAS NAMES THE VERSION IT FOLLOWS TODAY beside its name, as the engine's
 * harness profile records it for the vendor's own service; a cloud or unchanged
 * sign-in may route it elsewhere, and an unknown version shows nothing.
 *
 * WHERE THE SUGGESTIONS COME FROM (N6): `llm_models`, the models.dev
 * catalogue for the harness's provider, read by the engine with no credential
 * (D1). Whether a plan reaches a model is the test call's question. With no
 * catalogue the field still takes any id, and the harness's page is linked.
 */

import { useMemo, useState } from "react";
import {
  ChoiceList,
  Combobox,
  FormField,
  Link,
  Select,
  Text,
  type ComboboxOption,
} from "@crewlethq/ui";
import { useQuery } from "~/lib/useQuery.ts";
import type { ModelAlias } from "~/protocol/models.ts";
import { product } from "../../words.ts";
import { effortLevels, factsOf, modelId, namesProvider, type Draft } from "../draft.ts";

/** The model field's placeholder, in the harness's own naming where it has a familiar one. */
const MODEL_PLACEHOLDER: Readonly<Record<string, string>> = {
  "claude-code": "claude-opus-5-5",
  codex: "gpt-5.5",
  opencode: "claude-sonnet-5-5",
  pi: "claude-sonnet-5-5",
};

/** An alias's name, with the version it resolves to when the sign-in reaches the vendor's own service. */
function aliasLabel(alias: ModelAlias, draft: Draft) {
  const known = alias.version && draft.method !== "cloud" && draft.method !== "unchanged";
  if (!known) return alias.name;
  return (
    <>
      {alias.name}{" "}
      <Text as="span" variant="caption" tone="secondary">
        {alias.version}
      </Text>
    </>
  );
}

/** The value the effort control holds for the harness's default, which writes no `effort`. */
const DEFAULT_EFFORT = "";

/** How hard the model reasons: only the levels the harness takes on this model, or why it has
 * none. The harness default writes nothing. */
export function EffortField({
  draft,
  error,
  onChange,
}: {
  draft: Draft;
  error: string | undefined;
  onChange: (effort: string) => void;
}) {
  const facts = factsOf(draft.harness);
  const levels = effortLevels(draft);
  const name = product(draft.harness);
  if (facts.efforts.length === 0) {
    return (
      <Text as="p" variant="caption" tone="secondary">
        No reasoning effort setting here. {facts.effortNote ?? ""}.
      </Text>
    );
  }
  // A LEVEL WRITTEN IN CONFIGURATION that the list lacks stays chosen, and is said.
  const kept = draft.effort && !levels.includes(draft.effort) ? [draft.effort] : [];
  const helper =
    levels.length === 0
      ? `${name} runs ${modelId(draft)} with no reasoning effort setting.`
      : `How hard the model reasons, in ${name}'s own levels for this model. Harness default sends none, so ${name} uses its own.`;
  return (
    <FormField label="Reasoning effort" optional helper={helper} error={error}>
      {(field) => (
        <Select
          id={field.id}
          aria-describedby={field.describedBy}
          aria-invalid={field.invalid || undefined}
          value={draft.effort || DEFAULT_EFFORT}
          options={[
            { value: DEFAULT_EFFORT, label: "Harness default" },
            ...[...kept, ...levels].map((l) => ({ value: l, label: l })),
          ]}
          onChange={(v) => onChange(String(v))}
        />
      )}
    </FormField>
  );
}

/** The value of the choice that opens the free-text field. */
const ANOTHER = "__another__";

export function ModelStep({
  draft,
  error,
  effortError,
  onChange,
  onEffort,
}: {
  draft: Draft;
  error: string | undefined;
  effortError: string | undefined;
  onChange: (model: string) => void;
  onEffort: (effort: string) => void;
}) {
  const params = useMemo(
    () => ({
      harness: draft.harness,
      ...(namesProvider(draft.harness) ? { provider: draft.provider } : {}),
    }),
    [draft.harness, draft.provider],
  );
  const listing = useQuery("llm_models", params);
  const answer = listing.data;
  const aliases = answer?.aliases ?? [];
  const isAlias = aliases.some((a) => a.id === draft.model);
  // THE READER'S OWN CHOICE, once made: "Another model" stays chosen while its
  // field is emptied, so clearing it to type a new id does not throw the
  // reader back to the list. UNTIL THEN IT IS READ FROM THE ALIASES AS THEY
  // STAND, never decided once on the first render: the harness's aliases arrive
  // after it, and an edit of a model on `haiku` opened as "Another model"
  // with `haiku` typed in its field.
  const [chosen, setChosen] = useState<"alias" | "another" | null>(null);
  const another = chosen === "another" || (chosen === null && draft.model !== "" && !isAlias);
  const [open, setOpen] = useState(false);
  const catalogued = another ? answer?.models.find((m) => m.id === draft.model.trim()) : undefined;

  const options = useMemo<ComboboxOption[]>(() => {
    const q = draft.model.trim().toLowerCase();
    return (answer?.models ?? [])
      .filter((m) => !q || m.id.toLowerCase().includes(q) || m.name.toLowerCase().includes(q))
      .slice(0, 50)
      .map((m) => ({ value: m.id, label: m.id, hint: m.name }));
  }, [answer, draft.model]);

  const naming = namesProvider(draft.harness)
    ? `Sent as ${modelId({ ...draft, model: draft.model || "<model>" })}.`
    : "In the harness's own naming.";
  const source = listing.loading
    ? "Reading the catalogue."
    : answer?.source === "models.dev"
      ? "Suggestions from models.dev."
      : answer?.reason
        ? `No suggestions: ${answer.reason.replace(/\.?$/, ".")}`
        : listing.error
          ? "No suggestions: the catalogue could not be read."
          : "";

  const field = (
    <FormField
      label={aliases.length > 0 ? "Model id" : "Model"}
      {...(answer?.docs_url
        ? {
            labelAction: (
              <Link href={answer.docs_url} external size="caption">
                Model names
              </Link>
            ),
          }
        : {})}
      helper={source ? `${naming} ${source}` : naming}
      error={error}
    >
      {(f) => (
        <Combobox
          id={f.id}
          aria-describedby={f.describedBy}
          aria-invalid={f.invalid || undefined}
          aria-required
          error={f.invalid}
          label="Suggested models"
          mono
          value={draft.model}
          placeholder={MODEL_PLACEHOLDER[draft.harness] ?? "The model's id"}
          onValueChange={(next) => {
            onChange(next);
            setOpen(options.length > 0);
          }}
          onFocus={() => setOpen(options.length > 0)}
          open={open && options.length > 0}
          onOpenChange={setOpen}
          options={options}
        />
      )}
    </FormField>
  );

  const effort = <EffortField draft={draft} error={effortError} onChange={onEffort} />;
  if (aliases.length === 0)
    return (
      <>
        {field}
        {effort}
      </>
    );
  return (
    <>
      <ChoiceList
        label="Model"
        value={another ? ANOTHER : isAlias ? draft.model : null}
        onChange={(v) => {
          if (v === ANOTHER) {
            setChosen("another");
            if (isAlias) onChange("");
            return;
          }
          setChosen("alias");
          onChange(v);
        }}
        options={[
          ...aliases.map((a) => ({ value: a.id, label: aliasLabel(a, draft), detail: a.detail })),
          {
            value: ANOTHER,
            label: "Another model",
            detail: catalogued
              ? `${catalogued.name}, a fixed version.`
              : "Any model id the harness takes.",
            separated: true,
            panel: field,
          },
        ]}
      />
      {error && !another ? (
        <Text as="p" variant="caption" className="models-field-error" role="alert">
          {error}
        </Text>
      ) : null}
      {effort}
    </>
  );
}
