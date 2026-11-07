/**
 * A reader view's head: its title and its own tools, drawn in the
 * conversation's top bar rather than as a row of their own under it, so the
 * view's list starts one bar's height higher.
 *
 * THE BAR OWNS TWO SLOTS and the view portals into them: the title as the
 * bar's current location after the switchers, and the tools before Search.
 * A portal keeps the tools the view's own (its state, its handlers, its
 * context) while their elements stand in the bar, and the title stays the
 * view's one `<h1>`.
 *
 * THE BAR IS NEVER REMOUNTED between places (`Chat.tsx`'s layout), so its
 * slots are the same elements for the screen's life: a move from one view to
 * another swaps the title and the tools in the commit that swaps the view,
 * with no frame between them holding the old ones. A room draws no head, so
 * its bar is drawn as it always was.
 */

import {
  createContext,
  useContext,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import { createPortal } from "react-dom";

import { focusQuietly } from "~/lib/wizardFocus.ts";

/** Where a view's head is drawn in the bar, and the bar's shape. */
interface ViewBar {
  /** The bar's slot for the title; null until the bar is drawn. */
  title: HTMLElement | null;
  /** The bar's slot for the view's tools, before Search. */
  tools: HTMLElement | null;
  /** The bar is under its tight width: a tool drops its words for its glyph. */
  tight: boolean;
}

/** What the bar hands the host: its slots and its shape, and the view's drawn title. */
interface ViewBarWriter {
  titleSlot: (el: HTMLElement | null) => void;
  toolsSlot: (el: HTMLElement | null) => void;
  setTight: (tight: boolean) => void;
  heading: RefObject<HTMLHeadingElement | null>;
}

const NO_BAR: ViewBar = { title: null, tools: null, tight: false };

const ViewBarContext = createContext<ViewBar>(NO_BAR);
const ViewBarWriterContext = createContext<ViewBarWriter | null>(null);

/**
 * Holds the bar's slots for the views under it. The writer is made once, so
 * the bar's slot refs never change and a render never detaches them.
 */
export function ViewBarHost({ children }: { children: ReactNode }) {
  const [bar, setBar] = useState<ViewBar>(NO_BAR);
  const heading = useRef<HTMLHeadingElement | null>(null);
  const writer = useMemo<ViewBarWriter>(() => {
    const put =
      <K extends keyof ViewBar>(key: K) =>
      (value: ViewBar[K]) =>
        setBar((was) => (was[key] === value ? was : { ...was, [key]: value }));
    return { titleSlot: put("title"), toolsSlot: put("tools"), setTight: put("tight"), heading };
  }, []);
  return (
    <ViewBarWriterContext.Provider value={writer}>
      <ViewBarContext.Provider value={bar}>{children}</ViewBarContext.Provider>
    </ViewBarWriterContext.Provider>
  );
}

/**
 * The bar's side: the refs for its two slots, with its tight shape reported
 * to the views. Outside a host (the bar alone in a suite) the slots go
 * nowhere.
 */
export function useViewBarSlots(tight: boolean): {
  title?: (el: HTMLElement | null) => void;
  tools?: (el: HTMLElement | null) => void;
} {
  const writer = useContext(ViewBarWriterContext);
  useLayoutEffect(() => writer?.setTight(tight), [writer, tight]);
  return writer ? { title: writer.titleSlot, tools: writer.toolsSlot } : {};
}

/** Whether the bar the view's tools stand in is under its tight width. */
export function useViewBarTight(): boolean {
  return useContext(ViewBarContext).tight;
}

/**
 * Focuses the open view's title, where focus goes when the control that held
 * it is removed. A HEADING IS A READING POSITION, focused with no ring
 * (`focusQuietly`).
 */
export function useFocusViewTitle(): () => void {
  const writer = useContext(ViewBarWriterContext);
  return () => {
    const el = writer?.heading.current;
    if (el?.isConnected) focusQuietly(el);
  };
}

/** A view's head: its title in the bar's title slot and its tools in the bar's tools slot. */
export function ViewHead({ title, children }: { title: string; children?: ReactNode }) {
  const bar = useContext(ViewBarContext);
  const writer = useContext(ViewBarWriterContext);
  return (
    <>
      {bar.title &&
        createPortal(
          <h1 ref={writer?.heading} className="chat-topbar-title" tabIndex={-1}>
            {title}
          </h1>,
          bar.title,
        )}
      {children && bar.tools && createPortal(children, bar.tools)}
    </>
  );
}
