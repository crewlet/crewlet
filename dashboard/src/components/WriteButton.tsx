/**
 * The one control that changes the company.
 *
 * NEVER HIDDEN. It is drawn for every reader and DISABLED with the reason when
 * this browser cannot make the change (`lib/useWriteAccess.ts`): the kit's
 * `disabledReason` keeps it focusable, and reads the sentence to a screen
 * reader as the button's description — a natively disabled button takes no
 * focus, and its explanation with it.
 *
 * THE KIT DRAWS THAT SENTENCE FOR NOBODY ELSE: it is visually hidden, so a
 * sighted reader hovering a dead button learned nothing. The same sentence is
 * therefore the button's `title` while it is held, which a pointer reveals on
 * hover and which a screen reader does not read a second time (an element's
 * `aria-describedby` outranks its title as its description). A tooltip the
 * pointer and the keyboard both open would have been read twice for exactly
 * that reason. A form whose primary action is held also writes the reason on
 * the page beside it (the New task sheet's footer), because neither reaches a
 * touch screen.
 *
 * AND IT SAYS WHAT BECAME OF THE LAST PRESS, beside itself: a refusal stays
 * under the control until the next press, with a Retry where the node, not
 * the request, was the problem (`lib/useAct.ts`).
 */

import type { ReactNode } from "react";
import { Button, type ButtonProps } from "@crewlethq/ui";
import type { ActionTool } from "~/protocol/act.ts";
import type { Act } from "~/lib/useAct.ts";
import { marked } from "~/ui/Problems.tsx";

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
  /**
   * Whether THIS button's press is the one in flight, where several buttons
   * send through one write (the options of one ask): only that one shows it
   * is sending. Defaults to the write's own `busy`, for a button that is the
   * write's only control. Every button of the write still refuses a press
   * while any press is out.
   */
  pressing?: boolean;
}

/**
 * Whether `write` may be pressed now: this browser may make the change,
 * nothing holds the press (`blocked`), and no press of it is already out.
 *
 * WHAT A WriteButton CHECKS ON ITS OWN CLICK — and every other way into the
 * same press checks it too, through this. A dialog is a form, so Enter
 * submits it without touching the button; a reply field sends on ⌘Enter; a
 * one-line form files on Enter. Each of those went straight to `run`, and a
 * second Enter while the first answer was out sent the change again, under a
 * NEW request id — which the engine rightly takes for a second change: two
 * sub-tasks filed, two comments posted, a target date written twice.
 */
export function pressable<T extends ActionTool>(write: Act<T>, blocked?: string): boolean {
  return write.access.can && blocked === undefined && !write.busy;
}

export function WriteButton<T extends ActionTool>({
  write,
  onPress,
  children,
  showRefusal = true,
  blocked,
  pressing,
  ...rest
}: WriteButtonProps<T>) {
  const { access, busy } = write;
  const reason = access.can ? blocked : access.reason;
  return (
    <span className="write-button">
      <Button
        {...rest}
        loading={pressing ?? busy}
        disabledReason={reason}
        title={reason ?? rest.title}
        onClick={(event) => {
          // A ROW OR A CARD IS OFTEN AN ANCHOR: the press is this button's,
          // never a navigation to whatever the row links to.
          event.preventDefault();
          event.stopPropagation();
          if (pressable(write, blocked)) onPress();
        }}
      >
        {children}
      </Button>
      {showRefusal && <RefusalNote write={write} />}
    </span>
  );
}

/**
 * A refusal, drawn where the control that caused it is.
 *
 * THE ENGINE'S SENTENCE NAMES ITS ARGUMENTS IN BACKTICKS ("`labels` …"), which
 * is the one mark a tool's sentence carries for a value — so it is drawn as
 * one, in the refusal's own ink, rather than as punctuation around a word.
 *
 * A REFUSAL ON AUTHORITY SAYS WHAT WOULD CHANGE IT: the grants the deciding
 * rule would have admitted this person on, from the answer itself and never
 * written here — a grant named in this file is a second statement of the
 * rule, and the one that goes stale the day the rule's grant moves.
 */
export function RefusalNote<T extends ActionTool>({ write }: { write: Act<T> }) {
  const { refusal, busy } = write;
  if (!refusal) return null;
  return (
    <span className="write-refusal" role="alert">
      <span>{marked(refusal.sentence, "inherit")}</span>
      {refusal.hint && <span className="t-caption">{refusal.hint}</span>}
      {refusal.grants.length > 0 && (
        <span className="t-caption">
          Needs{" "}
          {refusal.grants.map((grant, i) => (
            <span key={grant}>
              {i > 0 ? " or " : ""}
              <code className="inline">{grant}</code>
            </span>
          ))}
          : sign in as somebody who holds it.
        </span>
      )}
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
