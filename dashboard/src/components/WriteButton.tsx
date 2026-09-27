/**
 * The one control that changes the company.
 *
 * NEVER HIDDEN. It is drawn for every reader and DISABLED with the reason when
 * this browser cannot make the change (`lib/useWriteAccess.ts`): the kit's
 * `disabledReason` keeps it focusable and hoverable, so the sentence is
 * reachable by exactly the reader who needs it — a natively disabled button
 * takes no focus and no hover, and its explanation with it.
 *
 * AND IT SAYS WHAT BECAME OF THE LAST PRESS, beside itself: a refusal stays
 * under the control until the next press, with a Retry where the node, not
 * the request, was the problem (`lib/useAct.ts`).
 */

import type { ReactNode } from "react";
import { Button, type ButtonProps } from "@crewlethq/ui";
import type { ActionTool } from "~/protocol/act.ts";
import type { Act } from "~/lib/useAct.ts";

export interface WriteButtonProps<T extends ActionTool> extends Omit<
  ButtonProps,
  "onClick" | "disabled" | "disabledReason" | "loading" | "children"
> {
  /** The change this button makes, from `useAct`. */
  write: Act<T>;
  /** Press it. Called only when the change can be made. */
  onPress: () => void;
  children: ReactNode;
  /**
   * Why this press would change nothing even though the change could be
   * made — nothing chosen yet, the same value as now. Disabled with this
   * sentence, the same way as a reader who cannot act: a press that silently
   * does nothing is a button somebody presses twice.
   */
  blocked?: string;
  /**
   * Draw the refusal beside the button. Off where the caller draws it
   * itself, beside the field it names.
   */
  showRefusal?: boolean;
}

export function WriteButton<T extends ActionTool>({
  write,
  onPress,
  children,
  showRefusal = true,
  blocked,
  ...rest
}: WriteButtonProps<T>) {
  const { access, busy } = write;
  const reason = access.can ? blocked : access.reason;
  return (
    <span className="write-button">
      <Button
        {...rest}
        loading={busy}
        disabledReason={reason}
        onClick={(event) => {
          // A ROW OR A CARD IS OFTEN AN ANCHOR: the press is this button's,
          // never a navigation to whatever the row links to.
          event.preventDefault();
          event.stopPropagation();
          if (reason === undefined && !busy) onPress();
        }}
      >
        {children}
      </Button>
      {showRefusal && <RefusalNote write={write} />}
    </span>
  );
}

/** A refusal, drawn where the control that caused it is. */
export function RefusalNote<T extends ActionTool>({ write }: { write: Act<T> }) {
  const { refusal, busy } = write;
  if (!refusal) return null;
  return (
    <span className="write-refusal" role="alert">
      <span>{refusal.sentence}</span>
      {refusal.hint && <span className="t-caption">{refusal.hint}</span>}
      {refusal.retryable && (
        <Button size="small" variant="ghost" loading={busy} onClick={() => void write.retry()}>
          Try again
        </Button>
      )}
      <Button size="small" variant="ghost" onClick={write.dismiss}>
        Dismiss
      </Button>
    </span>
  );
}
