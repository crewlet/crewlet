/**
 * The modal shell every dialog in the dashboard uses.
 *
 * Generalized from the token dialog rather than copied beside it: that one is
 * the only modal the dashboard had, and a second hand-rolled `.veil` plus
 * `role="dialog"` is how two dialogs start disagreeing about which one closes
 * on Escape and which one traps focus.
 *
 * What the shell owns, because every dialog needs it and none should restate
 * it: the veil and its click-outside, Escape, focus moved into the dialog on
 * open and returned to whatever opened it on close, `aria-modal` with a label,
 * and the head/body/foot structure the stylesheet already carries.
 *
 * What it deliberately does NOT own: what the dialog says, what its buttons
 * do, and whether closing is allowed. A dialog mid-write passes
 * `dismissable={false}` so a stray click cannot abandon a request whose
 * outcome the operator has not seen.
 *
 * # The behaviour and the chrome are two things
 *
 * [useModal] is the behaviour and [Dialog] is one presentation of it. The
 * split exists because the command palette is a modal whose header IS its
 * input: forced through `Dialog` it would grow a title bar it does not want,
 * so it hand-rolled the veil instead and got none of the behaviour — no focus
 * moved in, no focus returned to the row that opened it, and an Escape
 * handler on the input alone, which does nothing the moment focus is anywhere
 * else in the panel. A second presentation is fine; a second implementation
 * of "what a modal does" is how two of them start disagreeing.
 */

import { useEffect, useRef, type ReactNode } from "react";
import { Icon, type IconName } from "~/ui/Icon.tsx";

/**
 * What every modal in this dashboard does, whatever it looks like.
 *
 * Returns the props for the veil and for the panel inside it. Focus moves to
 * the panel's first control on open and returns to whatever opened it on
 * close; Escape closes; a mousedown on the veil closes and one inside the
 * panel does not.
 *
 * ON `document` RATHER THAN THE PANEL, which is the whole reason Escape is
 * here and not on a caller's own handler: a key listener bound to one input
 * stops working the instant the reader tabs into the list beside it.
 */
export function useModal({
  label,
  onClose,
  dismissable = true,
}: {
  label: string;
  onClose: () => void;
  dismissable?: boolean;
}) {
  // HTMLElement, because a caller's shell may be a <form> or a <div> and one
  // ref has to reach whichever was rendered.
  const ref = useRef<HTMLElement | null>(null);
  // WHERE FOCUS CAME FROM, so it can go back. A modal that returns focus to
  // the document body leaves a keyboard reader at the top of the page, which
  // on this dashboard means the rail rather than the row they opened.
  const opener = useRef<Element | null>(null);

  useEffect(() => {
    opener.current = document.activeElement;
    // The first control, or the panel itself when it has none, so a screen
    // reader announces the label rather than continuing behind the veil.
    const focusable = ref.current?.querySelector<HTMLElement>(
      "input, select, textarea, button, [href], [tabindex]:not([tabindex='-1'])",
    );
    (focusable ?? ref.current)?.focus();
    return () => {
      const back = opener.current;
      if (back instanceof HTMLElement && document.contains(back)) back.focus();
    };
  }, []);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape" && dismissable) {
        e.stopPropagation();
        onClose();
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [dismissable, onClose]);

  return {
    veil: {
      className: "veil",
      role: "presentation" as const,
      onMouseDown: () => dismissable && onClose(),
    },
    shell: {
      role: "dialog" as const,
      "aria-modal": true,
      "aria-label": label,
      tabIndex: -1,
      ref: (el: HTMLElement | null) => {
        ref.current = el;
      },
      onMouseDown: (e: React.MouseEvent) => e.stopPropagation(),
    },
  };
}

export function Dialog({
  title,
  icon,
  onClose,
  children,
  footer,
  width = 480,
  dismissable = true,
  onSubmit,
}: {
  title: string;
  icon?: IconName;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  width?: number;
  /** False while a request is in flight: the veil and Escape stop closing. */
  dismissable?: boolean;
  /** When given, the shell is a form and Enter submits it. */
  onSubmit?: () => void;
}) {
  const { veil, shell: modal } = useModal({ label: title, onClose, dismissable });

  const body = (
    <>
      <header className="dialog-head">
        {icon && <Icon name={icon} size="sm" />}
        <strong style={{ fontSize: "var(--fs-sm)" }}>{title}</strong>
      </header>
      <div className="dialog-body col gap-3">{children}</div>
      {footer && <footer className="dialog-foot">{footer}</footer>}
    </>
  );

  const shell = {
    ...modal,
    className: "dialog",
    style: { width: `min(${width}px, 100%)` },
  };

  return (
    <div {...veil}>
      {onSubmit ? (
        <form
          {...shell}
          onSubmit={(e) => {
            e.preventDefault();
            onSubmit();
          }}
        >
          {body}
        </form>
      ) : (
        <div {...shell}>{body}</div>
      )}
    </div>
  );
}
