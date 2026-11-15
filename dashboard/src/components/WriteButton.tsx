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
 * AND IT SAYS WHAT BECAME OF THE LAST PRESS: a refusal stays until the next
 * press, with a Retry where the node, not the request, was the problem
 * (`lib/useAct.ts`).
 *
 * # Where the refusal is drawn
 *
 * Under the control, where the control stands alone (a form's submit, a
 * button in a card's body). But a control that is ONE OF A ROW (the page
 * bar, a header's actions, a dialog's foot) drew its refusal there too, in
 * its own box: the box took the refusal's width and height, and the row
 * reflowed around it. The page bar grew a second line with Run now above the
 * trail and Edit and Delete shoved aside; a dialog's foot left Cancel alone
 * at its end and dropped the primary action and the refusal onto lines of
 * their own under it.
 *
 * So a row of controls keeps a PLACE for their refusals, outside the row
 * (under the page's head, under an object's header, above a dialog's foot),
 * and hands it to the controls in it ([RefusalsTo], [useRefusalPlace]); a
 * control in such a row draws its refusal there and its row never moves.
 */

import {
  createContext,
  useContext,
  useEffect,
  useState,
  type ReactElement,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import { Button, RefusalStrip, type ButtonProps } from "@crewlethq/ui";
import type { ActionTool } from "~/protocol/act.ts";
import type { Act } from "~/lib/useAct.ts";
import { marked } from "~/ui/Problems.tsx";
import { accessToAsk } from "~/lib/authorityRemedy.ts";
import { LimitRemedy } from "./LimitRemedy.tsx";

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
   * Draw the refusal: under the button, or in the place the row it stands in
   * keeps for refusals ([RefusalsTo]). Off where the caller draws it itself,
   * beside the field it names.
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

/**
 * Where the refusals of the write controls under it are drawn: the element a
 * row of controls keeps for them, `null` while the row has not drawn it yet,
 * `undefined` where no row keeps one, and a control draws its refusal under
 * itself.
 */
const RefusalPlace = createContext<HTMLElement | null | undefined>(undefined);

/**
 * Sends the refusals of the write controls in `children` to `place`: what a
 * row of controls wraps them in. `undefined` sends them nowhere, so a row
 * that has no place yet to give (a frame that is not there) leaves each
 * under its control.
 */
export function RefusalsTo({
  place,
  children,
}: {
  place: HTMLElement | null | undefined;
  children: ReactNode;
}) {
  return <RefusalPlace.Provider value={place}>{children}</RefusalPlace.Provider>;
}

/**
 * A row's place for its controls' refusals: draw `place` where they belong,
 * outside the row, and wrap the row in [RefusalsTo] with `element`. Empty, it
 * takes no room (`.refusal-place:empty`).
 */
export function useRefusalPlace(className?: string): {
  element: HTMLElement | null;
  place: ReactElement;
} {
  const [element, setElement] = useState<HTMLElement | null>(null);
  return {
    element,
    place: (
      <div
        ref={setElement}
        className={className ? `refusal-place ${className}` : "refusal-place"}
      />
    ),
  };
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
  const place = useContext(RefusalPlace);
  const refusal = showRefusal ? <RefusalNote write={write} /> : null;
  // A REFUSAL DRAWN AWAY FROM ITS CONTROL IS BROUGHT INTO VIEW: the place
  // above a dialog's foot ends a body that may be scrolled away from it, and
  // a press that seems to do nothing is a press made again.
  const shown = showRefusal && place ? write.refusal : null;
  useEffect(() => {
    if (shown && place) place.scrollIntoView?.({ block: "nearest" });
  }, [shown, place]);
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
      {/* IN THE ROW'S PLACE where it keeps one (nothing until it is drawn),
          under the control where it does not. */}
      {place === undefined ? refusal : place && refusal && createPortal(refusal, place)}
    </span>
  );
}

/**
 * A refusal, drawn where the control that caused it is.
 *
 * THE SENTENCE IS A PERSON'S (ADR-0060): the engine answers a refusal in words
 * written for whoever pressed, or in its code's own sentence, and neither
 * carries a backtick. An older node during a rolling upgrade still answers a
 * tool's sentence, which marks a value in backticks, so a backtick is drawn
 * as a value in the refusal's own ink rather than as punctuation around a
 * word.
 *
 * A REFUSAL ON AUTHORITY SAYS WHAT WOULD CHANGE IT: the access the deciding
 * rule would have admitted this person on ("needs Worklet access"), from the
 * answer itself and never written here — a permission named in this file is
 * a second statement of the rule, and the one that goes stale the day the
 * rule moves. Only where that access IS the way past the rule ([accessToAsk]):
 * a proof gone stale names the access that admitted the person, and an
 * artifact's role is cleared by its owner's share, so each says its own
 * sentence.
 */
export function RefusalNote<T extends ActionTool>({ write }: { write: Act<T> }) {
  const { refusal, busy } = write;
  if (!refusal) return null;
  return (
    // THE KIT'S ONE DRAWING OF A REFUSAL, inline under the control that
    // caused it. ABOVE A GRID ROW'S LINK (`data-above-row`), so its sentence
    // can be selected and its link followed rather than opening the row.
    <RefusalStrip
      data-above-row=""
      hint={refusal.hint || undefined}
      onRetry={refusal.retryable ? () => void write.retry() : undefined}
      retrying={busy}
      onDismiss={write.dismiss}
    >
      {/* ONE SENTENCE where access would admit: the refusal and its remedy
          said the same thing twice. */}
      {accessToAsk(refusal) ?? marked(refusal.sentence, "inherit")}
      {/* WHERE THE LIMIT THAT REFUSED IT IS RAISED, for a refusal a limit
          caused, behind the gate of the editor it opens. */}
      <LimitRemedy remedy={refusal.remedy} />
    </RefusalStrip>
  );
}
