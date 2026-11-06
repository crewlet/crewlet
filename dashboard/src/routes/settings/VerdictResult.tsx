/**
 * What a credential check found, wherever one reports back: a connect form's
 * Test control and an agent's pasted token. One renderer, so every vendor's
 * answer reads the same way.
 */
import { Callout, DescriptionList, StatusDot } from "@crewlethq/ui";
import type { SetupVerdict, VerifyOutcome } from "~/protocol/index.ts";
import { marked } from "~/ui/Problems.tsx";

/** The outcome's tone. A refusal is never drawn as success, nor an outage as a refusal. */
const VERDICT_TONE: Record<VerifyOutcome, "success" | "danger" | "warning" | "neutral"> = {
  works: "success",
  refused: "danger",
  unreachable: "warning",
  unchecked: "neutral",
};

/**
 * Its answer, what it read as pairs, and each thing to act on as its own
 * callout. None of it waits behind a disclosure, because a risk that has to be
 * opened to be read is one that goes unread.
 *
 * TWO SIBLINGS OF WHATEVER HOLDS IT, in `.int-key-check`: the answer beside a
 * control if there is one, and the rest on a line of its own, so a long result
 * never moves the control.
 */
export function VerdictResult({ verdict }: { verdict: SetupVerdict }) {
  const facts = verdict.facts ?? [];
  const warnings = verdict.warnings ?? [];
  return (
    <>
      {/* THE ANSWER WRAPS rather than being cut: on a narrow pane it is the
          one line under the button that has to be read whole. */}
      <span className="int-verdict-status">
        <StatusDot tone={VERDICT_TONE[verdict.outcome] ?? "neutral"} wrap>
          {verdict.summary}
        </StatusDot>
      </span>
      {(facts.length > 0 || warnings.length > 0) && (
        <div className="int-verdict">
          {/* A VALUE IS DRAWN AS IT CAME, because it is not a sentence the engine
              wrote: an address must not be read as a config path, and a vendor's
              own words must not become a link in an administrator's console. */}
          {facts.length > 0 && (
            <DescriptionList dense items={facts.map((f) => [f.label, f.value] as const)} />
          )}
          {warnings.map((w, i) => (
            <Callout
              key={i}
              variant={verdict.outcome === "refused" ? "danger" : "warning"}
              title={w.title ? marked(w.title, "inherit") : undefined}
              live="polite"
            >
              <span className="int-verdict-action">{marked(w.action, "inherit")}</span>
            </Callout>
          ))}
        </div>
      )}
    </>
  );
}
