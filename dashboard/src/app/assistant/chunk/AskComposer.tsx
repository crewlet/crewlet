/**
 * The drawer's composer, and the one part of the drawer a keystroke renders.
 *
 * # Why the draft is not the drawer's state
 *
 * The draft used to be the drawer's own `useState`, so every keystroke
 * rendered the whole drawer: the thread, and every card of every earlier
 * turn. Each card's work on that render (a design-system card registering its
 * title in an effect, for one) left an update pending, and under keystrokes
 * arriving back to back (fast typing, or a main thread busy drawing a long
 * conversation) React counted those towards its nested-update limit until it
 * threw a keystroke away (error #185): "FAIR-3" was sent as "FAR-3".
 *
 * So the draft lives in a [DraftBox] the drawer owns but does not render on.
 * Only [AskComposer] subscribes to it, so a keystroke renders the field and
 * its Send button and nothing else, whatever the conversation holds. The
 * drawer reads and writes the box from its gestures (ask, fill, Up for the
 * last question) without ever rendering on its text.
 */

import { useState, useSyncExternalStore, type ReactNode, type Ref } from "react";
import { Composer, type ComposerProps } from "@crewlethq/ui";

/** The text in the composer, held outside React's state so a keystroke renders only the composer. */
export interface DraftBox {
  /** The text now. */
  read(): string;
  /** Replace the text; subscribers are told only when it changes. */
  write(text: string): void;
  /** Hear each change; returns the unsubscribe. */
  subscribe(listener: () => void): () => void;
}

/** An empty box. */
export function createDraftBox(): DraftBox {
  let text = "";
  const listeners = new Set<() => void>();
  return {
    read: () => text,
    write: (next) => {
      if (next === text) return;
      text = next;
      for (const listener of listeners) listener();
    },
    subscribe: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** One box for the life of the component that owns it. */
export function useDraftBox(): DraftBox {
  const [box] = useState(createDraftBox);
  return box;
}

export interface AskComposerProps extends Omit<
  ComposerProps,
  "value" | "onChange" | "onSubmit" | "trailing"
> {
  /** Where the draft is kept. */
  box: DraftBox;
  /** The send key was pressed: ask the text in the box. */
  onSubmit: (text: string) => void;
  /** The control at the end of the field (Send or Stop), drawn for the text in the box. */
  trailing: (text: string) => ReactNode;
  ref?: Ref<HTMLTextAreaElement>;
}

/** The kit's composer, bound to a [DraftBox]: the only part of the drawer a keystroke renders. */
export function AskComposer({ box, onSubmit, trailing, ref, ...rest }: AskComposerProps) {
  const text = useSyncExternalStore(box.subscribe, box.read, box.read);
  return (
    <Composer
      {...rest}
      ref={ref}
      value={text}
      onChange={box.write}
      onSubmit={() => onSubmit(text)}
      trailing={trailing(text)}
    />
  );
}
