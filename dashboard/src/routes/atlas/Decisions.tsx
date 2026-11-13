/**
 * Deciding what the atlas's sources disagree on, wherever it is drawn: the
 * value shown with every other value beside it, and the decisions a person
 * may take, each one write in their own name. Nothing settles in silence, so
 * a decision is always a press, and what the engine refuses is said under the
 * buttons in its own words.
 */

import { useState } from "react";
import { Tag } from "@crewlethq/ui";
import { SetByLine } from "~/app/frame/ObjectHeader.tsx";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import type { AtlasClaimed, AtlasFactView } from "~/contract/atlas.ts";
import { useAct, type Act } from "~/lib/useAct.ts";
import { FactValue, sourceSetBy } from "./parts.tsx";
import type { Decision, DecisionTool } from "./writes.ts";

/** A claim as a list of claims draws it: the one shown, or another beside it. */
export interface ShownClaim extends Pick<AtlasFactView, "source" | "word" | "observed_at"> {
  value?: AtlasFactView["value"];
}

/** The value the atlas shows, marked as shown, then every other source's beside it. */
export function ClaimList({
  shown,
  others,
  seatName,
  now,
}: {
  shown: ShownClaim;
  others: readonly AtlasClaimed[];
  seatName: (handle: string) => string;
  now: number;
}) {
  return (
    <ul className="atlas-claims">
      <li>
        <span className="atlas-claims__value">
          {shown.value ? <FactValue value={shown.value} /> : "Holds"}
          <Tag variant="neutral" size="sm">
            Shown
          </Tag>
        </span>
        <SetByLine setBy={sourceSetBy(shown, seatName, now)} className="t-caption muted" />
      </li>
      {others.map((claim) => (
        <li key={`${claim.source.kind}:${claim.source.name ?? ""}:${claim.observed_at}`}>
          <span className="atlas-claims__value">
            {claim.denied ? (
              "Says it does not hold"
            ) : claim.value ? (
              <FactValue value={claim.value} />
            ) : (
              "Holds"
            )}
          </span>
          <SetByLine setBy={sourceSetBy(claim, seatName, now)} className="t-caption muted" />
        </li>
      ))}
    </ul>
  );
}

/** The writes a decision is sent through, one hook each so each keeps its own refusal. */
export type DecisionWrites = { readonly [T in DecisionTool]: Act<T> };

export function useDecisionWrites(): DecisionWrites {
  return {
    atlas_confirm: useAct("atlas_confirm"),
    atlas_correct: useAct("atlas_correct"),
    atlas_unrelate: useAct("atlas_unrelate"),
    atlas_relate: useAct("atlas_relate"),
    atlas_set_owner: useAct("atlas_set_owner"),
    atlas_retire: useAct("atlas_retire"),
  };
}

/** Send one decision through its own write. */
export function decide(writes: DecisionWrites, decision: Decision, quiet = false) {
  const label = { done: decision.done, quiet };
  switch (decision.tool) {
    case "atlas_confirm":
      return writes.atlas_confirm.run(decision.args, label);
    case "atlas_correct":
      return writes.atlas_correct.run(decision.args, label);
    case "atlas_unrelate":
      return writes.atlas_unrelate.run(decision.args, label);
    case "atlas_relate":
      return writes.atlas_relate.run(decision.args, label);
    case "atlas_set_owner":
      return writes.atlas_set_owner.run(decision.args, label);
    case "atlas_retire":
      return writes.atlas_retire.run(decision.args, label);
  }
}

/** The decisions one waiting thing allows, as buttons: the one that keeps what is shown first. */
export function DecisionButtons({ decisions }: { decisions: readonly Decision[] }) {
  const writes = useDecisionWrites();
  const [pressed, setPressed] = useState<string | null>(null);
  const tools = [...new Set(decisions.map((decision) => decision.tool))];
  const busy = tools.some((tool) => writes[tool].busy);
  if (decisions.length === 0) return null;
  return (
    <div className="atlas-decide">
      <div className="atlas-decide__buttons">
        {decisions.map((decision) => (
          <WriteButton
            key={decision.key}
            write={writes[decision.tool] as Act<DecisionTool>}
            size="small"
            variant={decision.accepts ? "primary" : "secondary"}
            showRefusal={false}
            pressing={busy && pressed === decision.key}
            blocked={busy ? "Waiting for the engine to answer" : undefined}
            onPress={() => {
              setPressed(decision.key);
              void decide(writes, decision);
            }}
          >
            {decision.label}
          </WriteButton>
        ))}
      </div>
      {tools.map((tool) => (
        <RefusalNote key={tool} write={writes[tool] as Act<DecisionTool>} />
      ))}
    </div>
  );
}
