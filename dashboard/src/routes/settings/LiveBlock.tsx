/**
 * A block the engine redraws from what the form holds (`public.live`), and the
 * key such a block needs, made when the form opens (`mint_first`).
 *
 * The block holds public names and a public half alone, so it follows what is
 * typed without a save: the engine draws it (`POST .../draw`) from the typed
 * values of the fields it is drawn from, each held to its own rule first.
 */

import { useEffect, useRef, useState } from "react";
import { Callout, CodeBlock } from "@crewlethq/ui";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import type { SetupPublicView, SetupRequirement } from "~/contract/setup.ts";

/** How long typing rests before the block is redrawn. */
const REDRAW_MS = 250;

/** The fields of one form section that `mint_first` asks to make, not yet held. */
export function mintsFirst(reqs: SetupRequirement[]): string[] {
  return reqs.filter((r) => r.kind === "secret" && r.mint_first && !r.present).map((r) => r.field);
}

/**
 * Makes each `mint_first` value the form's sections lack, once per form, then
 * refreshes the form so its block is drawn. Answers what went wrong, if anything.
 */
export function useMintFirst(
  sections: { kind: string; seat?: string; requirements: SetupRequirement[] }[],
  refresh: () => Promise<void>,
): string | undefined {
  const asked = useRef(new Set<string>());
  const [failed, setFailed] = useState<string>();
  useEffect(() => {
    for (const { kind, seat, requirements } of sections) {
      const fields = mintsFirst(requirements);
      const key = `${kind}/${seat ?? ""}`;
      if (fields.length === 0 || asked.current.has(key)) continue;
      asked.current.add(key);
      rest
        .post(`/setup/integrations/${pathSegment(kind)}/inputs`, {
          generate: fields,
          summary: "Made the key for the setup block",
          ...(seat ? { seat } : {}),
        })
        .then(refresh)
        .catch((err: unknown) =>
          setFailed(err instanceof RestError ? err.detail || err.code : String(err)),
        );
    }
  }, [sections, refresh]);
  return failed;
}

/** What a redraw answered: the block, or why there is none (said at a field when one is named). */
type Drawn = { view: SetupPublicView } | { note: string; atField: boolean };

/**
 * A live block, drawn first as the engine listed it and then from what is
 * typed in `typed` (the fields it is drawn from), the latest answer winning.
 */
export function LiveBlock({
  kind,
  field,
  view,
  typed,
  onRefused,
}: {
  kind: string;
  field: string;
  view: SetupPublicView;
  /** The form's values for the fields the block is drawn from. */
  typed: Record<string, string>;
  /** A typed value the engine refused, said at its own field. */
  onRefused?: (field: string, said: string) => void;
}) {
  const refused = useRef(onRefused);
  useEffect(() => {
    refused.current = onRefused;
  });
  const [drawn, setDrawn] = useState<Drawn>({ view });
  const latest = useRef(0);
  const asked = JSON.stringify(typed);
  useEffect(() => {
    const seq = ++latest.current;
    const timer = setTimeout(() => {
      const values = JSON.parse(asked) as Record<string, string>;
      rest
        .post(`/setup/integrations/${pathSegment(kind)}/draw`, { field, values })
        .then((answer) => {
          if (seq !== latest.current) return;
          const { public: next, needs } = answer as { public?: SetupPublicView; needs?: string };
          setDrawn(
            next
              ? { view: next }
              : { note: needs ?? "This block cannot be drawn yet.", atField: false },
          );
        })
        .catch((err: unknown) => {
          if (seq !== latest.current) return;
          const note =
            err instanceof RestError
              ? err.detail || err.hint || "The block cannot be drawn from what is typed."
              : String(err);
          const at = err instanceof RestError ? String(err.body.field ?? "") : "";
          if (at) refused.current?.(at, note);
          setDrawn({ note, atField: at !== "" });
        });
    }, REDRAW_MS);
    return () => clearTimeout(timer);
  }, [kind, field, asked]);
  if ("note" in drawn) {
    return drawn.atField ? (
      <Callout variant="neutral" title="Fix the answer above to see the block" live="polite" />
    ) : (
      <Callout variant="neutral" title="This block cannot be drawn yet" live="polite">
        {drawn.note}
      </Callout>
    );
  }
  return (
    <CodeBlock
      code={drawn.view.text}
      language={drawn.view.language}
      wrap={false}
      maxHeight="24rem"
    />
  );
}
