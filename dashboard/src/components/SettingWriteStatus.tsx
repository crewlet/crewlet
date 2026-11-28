/**
 * What a change to a setting came to, under the field that made it: checking,
 * what the change would introduce, why it was refused, and a save this node
 * refused, each with the one control that answers it (`lib/useSettingWrite.ts`).
 * A save itself is confirmed by its toast (`lib/savedToast.ts`), not here.
 *
 * A LIVE REGION, because each of these arrives after the press with nothing
 * else on screen moving: a reader who pressed Save and heard nothing would
 * press it again.
 */

import { Button, Callout } from "@crewlethq/ui";
import type { SettingWrite } from "~/lib/useSettingWrite.ts";
import { brings, ConfigWarnings } from "~/components/ConfigWarnings.tsx";

export function SettingWriteStatus({
  write,
  onReload,
  unchanged = "Already set to that.",
  answersWarning = false,
  refused,
}: {
  write: SettingWrite;
  /** Re-read what the page draws: offered where a fresh read is the remedy. */
  onReload: () => void;
  /** What "nothing to save" reads as for this field. */
  unchanged?: string;
  /**
   * Whether the warning is answered here, with Save anyway and Cancel: for a
   * control that has no Save button of its own to say it on (a switch).
   */
  answersWarning?: boolean;
  /** A refusal in the form's own words, where it placed the engine's problems under its fields. */
  refused?: string | undefined;
}) {
  const { state } = write;
  return (
    <div className="ceiling-status" role="status" aria-live="polite">
      {state.kind === "checking" && <span className="t-caption">Checking with the engine…</span>}
      {state.kind === "saving" && <span className="t-caption">Saving…</span>}
      {state.kind === "unchanged" && <span className="t-caption">{unchanged}</span>}
      {state.kind === "warned" && (
        <ConfigWarnings
          warnings={state.warnings}
          title={brings}
          action={
            answersWarning && (
              <>
                <Button size="small" variant="primary" onClick={write.confirm}>
                  Save anyway
                </Button>
                <Button size="small" variant="ghost" onClick={write.reset}>
                  Cancel
                </Button>
              </>
            )
          }
        />
      )}
      {state.kind === "saved" && state.standing === "refused" && (
        <Callout variant="danger" title="Saved, but this node refused it">
          It goes on running the setting before.
        </Callout>
      )}
      {state.kind === "refused" && (
        <Callout
          variant="danger"
          title="Nothing was saved"
          action={
            state.reload && (
              <Button size="small" variant="secondary" onClick={onReload}>
                Reload
              </Button>
            )
          }
        >
          {refused ?? state.message}
        </Callout>
      )}
    </div>
  );
}

/** Whether [SettingWriteStatus] draws anything for `state`, for a caller that frames it. */
export function saysSomething(state: SettingWrite["state"]): boolean {
  return state.kind !== "idle" && (state.kind !== "saved" || state.standing === "refused");
}

/** A saved value this node refused, as the word beside the value it goes on running. */
export const REFUSED_HERE = "refused by this node";
