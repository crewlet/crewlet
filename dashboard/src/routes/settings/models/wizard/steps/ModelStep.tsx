/**
 * The model the CLI runs, in its own naming: the names the CLI resolves
 * itself (its aliases) as choices, and "Another model" for any other id,
 * which suggests what the public catalogue lists and links the CLI's own page
 * of model names.
 *
 * ONE CONTROL FOR ONE VALUE (design QA Minor 5). The free-text field is the
 * panel of the "Another model" choice and is empty unless that choice is
 * made, so an alias picked above is never echoed in a second field.
 *
 * WHERE THE SUGGESTIONS COME FROM (N6): `llm_models`, the models.dev
 * catalogue for the CLI's provider, read by the engine with no credential
 * (D1). Whether a plan reaches a model is the test call's question. With no
 * catalogue the field still takes any id, and the CLI's page is linked.
 */

import { useMemo, useState } from "react";
import { ChoiceList, Combobox, FormField, Link, Text, type ComboboxOption } from "@crewlethq/ui";
import { useQuery } from "~/lib/useQuery.ts";
import { modelId, type Draft } from "../draft.ts";

/** The value of the choice that opens the free-text field. */
const ANOTHER = "__another__";

export function ModelStep({
  draft,
  error,
  onChange,
}: {
  draft: Draft;
  error: string | undefined;
  onChange: (model: string) => void;
}) {
  const params = useMemo(
    () => ({
      agent: draft.agent,
      ...(draft.agent === "opencode" ? { provider: draft.provider } : {}),
    }),
    [draft.agent, draft.provider],
  );
  const listing = useQuery("llm_models", params);
  const answer = listing.data;
  const aliases = answer?.aliases ?? [];
  const isAlias = aliases.some((a) => a.id === draft.model);
  // "ANOTHER MODEL" STAYS CHOSEN while its field is emptied, so clearing the
  // field to type a new id does not throw the reader back to the list.
  const [another, setAnother] = useState(() => draft.model !== "" && !isAlias);
  const [open, setOpen] = useState(false);

  const options = useMemo<ComboboxOption[]>(() => {
    const q = draft.model.trim().toLowerCase();
    return (answer?.models ?? [])
      .filter((m) => !q || m.id.toLowerCase().includes(q) || m.name.toLowerCase().includes(q))
      .slice(0, 50)
      .map((m) => ({ value: m.id, label: m.id, hint: m.name }));
  }, [answer, draft.model]);

  const naming =
    draft.agent === "opencode"
      ? `Sent as ${modelId({ ...draft, model: draft.model || "<model>" })}.`
      : "In the CLI's own naming.";
  const source = listing.loading
    ? "Reading the catalogue."
    : answer?.source === "models.dev"
      ? "Suggestions from models.dev."
      : answer?.reason
        ? `No suggestions: ${answer.reason}`
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
          placeholder={draft.agent === "opencode" ? "claude-sonnet-5-5" : "claude-opus-5-5"}
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

  if (aliases.length === 0) return field;
  return (
    <>
      <ChoiceList
        label="Model"
        value={another ? ANOTHER : isAlias ? draft.model : null}
        onChange={(v) => {
          if (v === ANOTHER) {
            setAnother(true);
            if (isAlias) onChange("");
            return;
          }
          setAnother(false);
          onChange(v);
        }}
        options={[
          ...aliases.map((a) => ({ value: a.id, label: a.name, detail: a.detail })),
          {
            value: ANOTHER,
            label: "Another model",
            detail: "Any model id the CLI takes.",
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
    </>
  );
}
