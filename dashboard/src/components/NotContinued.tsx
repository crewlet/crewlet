/**
 * What a launch, or a turn's launches, did not continue: each verdict in the
 * engine's sentence (`executorkeep.NotContinued.Sentence`), and the engine's
 * detail, which quotes what refused, behind a disclosure. Drawn on a collected
 * launch's card, on a running executor and at the head of its turn, so a run
 * that started fresh says so wherever a person reads it. Nothing where no
 * session was lost.
 */

import { Callout, Disclosure } from "@crewlethq/ui";
import type { SessionNotContinued } from "~/contract/sessions.ts";
import { notContinuedSentence } from "~/lib/sessionLoss.ts";

export function NotContinuedCallout({ verdicts }: { verdicts: readonly SessionNotContinued[] }) {
  if (verdicts.length === 0) return null;
  return (
    <Callout variant="warning">
      {verdicts.map((v, i) => (
        <div key={i} className="col gap-1">
          <p>{notContinuedSentence(v)}</p>
          {v.detail && (
            <Disclosure title="Engine detail">
              <p className="t-caption">{v.detail}</p>
            </Disclosure>
          )}
        </div>
      ))}
    </Callout>
  );
}
