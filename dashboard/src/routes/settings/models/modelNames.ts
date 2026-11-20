/**
 * A model's name as a person reads it ("Claude Opus 5.5"), from the id its
 * entry runs, through the one place the dashboard reads model names: the
 * engine's `llm_models` answer for the entry's harness, which the wizard's
 * Model step reads too. It holds the harness's own aliases, each with the
 * model and version it follows on the vendor's own service, and the public
 * catalogue's models. An id it names neither way is said as itself.
 *
 * READ ONCE PER HARNESS, for an admin: `llm_models` is an admin's question,
 * and the engine serves it from its catalogue copy, fetched at most once a day.
 * A member, and anybody while the answers arrive, reads each model by its id.
 */

import { useEffect, useMemo, useState } from "react";
import { useClient } from "~/lib/store-hooks.ts";
import type { ModelsAnswer } from "~/protocol/models.ts";

/** The harnesses that name their provider in the model id (`<provider>/<model>`), which `llm_models` asks for. */
const NAMED_PROVIDER: ReadonlySet<string> = new Set(["opencode", "pi"]);

/** The `llm_models` question that names a model on a harness, or null where none can. */
export function catalogueOf(
  harness: string,
  id: string,
): { harness: string; provider?: string } | null {
  if (!harness || harness === "custom" || !id) return null;
  if (!NAMED_PROVIDER.has(harness)) return { harness };
  const slash = id.indexOf("/");
  return slash > 0 ? { harness, provider: id.slice(0, slash) } : null;
}

/**
 * The name an answer gives an id: an alias by the model it follows (or its own
 * name and version), else the catalogue's own name; null where it names none.
 */
export function nameIn(
  answer: Pick<ModelsAnswer, "aliases" | "models"> | null | undefined,
  id: string,
): string | null {
  if (!answer || !id) return null;
  const listed = (x: string) => answer.models.find((m) => m.id === x)?.name ?? null;
  const alias = answer.aliases.find((a) => a.id === id);
  if (!alias) return listed(id);
  return (
    (alias.model ? listed(alias.model) : null) ??
    (alias.version ? `${alias.name} ${alias.version}` : alias.name)
  );
}

/** A model's place in the names map: the harness and the id a name is read for. */
export const nameKey = (harness: string, id: string) => `${harness} ${id}`;

/**
 * The names of the models given, by [nameKey], read once per question for an
 * admin; empty for a member and until the answers arrive.
 */
export function useModelNames(
  models: readonly { harness: string; model: string }[],
  enabled: boolean,
): ReadonlyMap<string, string> {
  const { socket } = useClient();
  const questions = useMemo(() => {
    const out = new Map<string, { harness: string; provider?: string }>();
    for (const m of models) {
      const q = catalogueOf(m.harness, m.model);
      if (q) out.set(JSON.stringify(q), q);
    }
    return out;
  }, [models]);
  const asked = [...questions.keys()].sort().join("|");
  const [answers, setAnswers] = useState<ReadonlyMap<string, ModelsAnswer>>(new Map());
  useEffect(() => {
    if (!enabled) return;
    let live = true;
    for (const [id, q] of questions) {
      socket.query("llm_models", q).then(
        (answer) => {
          if (live) setAnswers((had) => new Map(had).set(id, answer));
        },
        () => {
          // A question refused or unanswered leaves the model said by its id.
        },
      );
    }
    return () => {
      live = false;
    };
    // The questions' content is the dependency, not the map's identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [asked, enabled, socket]);
  return useMemo(() => {
    const out = new Map<string, string>();
    for (const m of models) {
      const q = catalogueOf(m.harness, m.model);
      const name = q ? nameIn(answers.get(JSON.stringify(q)), m.model) : null;
      if (name) out.set(nameKey(m.harness, m.model), name);
    }
    return out;
  }, [models, answers]);
}
