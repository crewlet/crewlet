/**
 * A reader view's head: its own tools, drawn in the conversation's top bar
 * rather than as a row of their own under it, so the view's list starts one
 * bar's height higher. The view's name is the page trail's last crumb, the
 * page's one `h1`, so the bar does not repeat it.
 *
 * THE BAR OWNS A SLOT and the view portals into it, before Search. A portal
 * keeps the tools the view's own (its state, its handlers, its context) while
 * their elements stand in the bar.
 *
 * THE BAR IS NEVER REMOUNTED between places (`Chat.tsx`'s layout), so its
 * slot is the same element for the screen's life: a move from one view to
 * another swaps the tools in the commit that swaps the view, with no frame
 * between them holding the old ones. A room draws no head, so its bar is
 * drawn as it always was.
 */

import {
  createContext,
  useContext,
  useLayoutEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";

/** Where a view's tools are drawn in the bar, and the bar's shape. */
interface ViewBar {
  /** The bar's slot for the view's tools, before Search; null until the bar is drawn. */
  tools: HTMLElement | null;
  /** The bar is under its tight width: a tool drops its words for its glyph. */
  tight: boolean;
}

/** What the bar hands the host: its slot and its shape. */
interface ViewBarWriter {
  toolsSlot: (el: HTMLElement | null) => void;
  setTight: (tight: boolean) => void;
}

const NO_BAR: ViewBar = { tools: null, tight: false };

const ViewBarContext = createContext<ViewBar>(NO_BAR);
const ViewBarWriterContext = createContext<ViewBarWriter | null>(null);

/**
 * Holds the bar's slot for the views under it. The writer is made once, so
 * the bar's slot ref never changes and a render never detaches it.
 */
export function ViewBarHost({ children }: { children: ReactNode }) {
  const [bar, setBar] = useState<ViewBar>(NO_BAR);
  const writer = useMemo<ViewBarWriter>(() => {
    const put =
      <K extends keyof ViewBar>(key: K) =>
      (value: ViewBar[K]) =>
        setBar((was) => (was[key] === value ? was : { ...was, [key]: value }));
    return { toolsSlot: put("tools"), setTight: put("tight") };
  }, []);
  return (
    <ViewBarWriterContext.Provider value={writer}>
      <ViewBarContext.Provider value={bar}>{children}</ViewBarContext.Provider>
    </ViewBarWriterContext.Provider>
  );
}

/**
 * The bar's side: the ref for its tools slot, with its tight shape reported
 * to the views. Outside a host (the bar alone in a suite) the slot goes
 * nowhere.
 */
export function useViewBarSlot(tight: boolean): ((el: HTMLElement | null) => void) | undefined {
  const writer = useContext(ViewBarWriterContext);
  useLayoutEffect(() => writer?.setTight(tight), [writer, tight]);
  return writer?.toolsSlot;
}

/** Whether the bar the view's tools stand in is under its tight width. */
export function useViewBarTight(): boolean {
  return useContext(ViewBarContext).tight;
}

/** A view's head: its tools in the bar's tools slot. */
export function ViewHead({ children }: { children: ReactNode }) {
  const bar = useContext(ViewBarContext);
  return bar.tools ? createPortal(children, bar.tools) : null;
}
