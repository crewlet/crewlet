/**
 * The room the toast stack keeps clear of the frame's own panels.
 *
 * The one toast host wraps the whole app (`App.tsx`), above the frame, so a
 * toast survives the navigation the write that raised it causes; but the
 * frame is what knows whether a panel stands at the window's edge, where the
 * stack would otherwise land on it. While the assistant is a column or over
 * the page, the stack stands beside it rather than over it; while it is the
 * whole window on a phone, the stack stands above its foot (the composer).
 *
 * A REQUEST PER ASKER, merged: the frame knows the width a panel takes and
 * the panel knows its own foot's height, and two writers of one value would
 * each undo the other's. Each asker (one mounted hook) says only its own part
 * and withdraws it when it goes; the host keeps the largest of each, so two
 * askers of one kind (a room's foot and a thread's reply box) never undo
 * each other either.
 *
 * THE STACK STANDS IN ONE PLACE ON EVERY SCREEN, the window's foot at its end
 * edge. Chatlet once stood it at the top right, under its bar, because its
 * composers sit at the foot and grow as they are typed in; there it lay over
 * an open pane (a room's details), and a toast was found in a different
 * corner than everywhere else. What grows is measured instead: a bar docked
 * at the foot keeps the stack above it however tall it grows
 * (`useToastClearOf`), and a panel standing at the end edge keeps it beside
 * the panel (`useToastBeside`), so it never covers an open one.
 *
 * A BAR DOCKED AT THE WINDOW'S FOOT ASKS (`useToastClearOf`): on a phone a
 * selection's bar stands where the stack does, and a toast raised by a press
 * on it lay over the controls the next press needs; Chatlet's room foot and
 * its reply boxes ask the same. Only a bar that reaches the foot asks, and
 * one with no box (a column folded away) asks nothing.
 *
 * THE PAGE KEEPS ROOM FOR THE STACK AT ITS FOOT (`useToastRoom`, `ToastRoom`).
 * A toast stands over the page, so after a press on a table's last row its
 * toast covered that row's controls for as long as it showed. THE ROOM IS
 * ALWAYS THERE, one toast's height over whatever a bar holds at the foot: made
 * as a toast came, it moved the page up under the pointer and back as it went.
 * Only a stack taller than that grows the page's end, keeping a reader at the
 * end there. Each stack reads its height off itself, the kit's toaster in its
 * own layer: it is always mounted, and is as tall as the toasts it holds. The
 * page keeps the largest.
 *
 * A SURFACE WITH A LAYER OF ITS OWN HOSTS ITS STACK THROUGH THE APP'S
 * (`SurfaceToastProvider`). A fullscreen surface such as the org builder
 * mounts a provider inside its `LayerHost`, since a toast drawn outside a
 * fullscreen element is not painted; but a bare provider there kept clear of
 * nothing the app's host is asked for, so on Structure a toast lay over the
 * assistant's panel, and its stack was one the page made no room for. It
 * takes the app's edge and inset, and says its room to the page.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useId,
  useState,
  type ReactNode,
} from "react";
import { ToastProvider, useLayerContainer, type ToasterInset } from "@crewlethq/ui";
import { screenScroller } from "~/lib/scroller.ts";

/**
 * Who keeps room: the frame (a panel's width), a panel itself (its foot), a bar
 * docked at the foot, or a screen's own pane standing at the end edge.
 */
export type InsetOwner = "frame" | "panel" | "bar" | "pane";

/** One asker's part, under its own key; null withdraws it. */
type Request = (key: string, inset: ToasterInset | null) => void;

const InsetRequest = createContext<Request>(() => {});

/** A stack's room at the window's foot, said under its id; 0 withdraws it. */
type RoomReport = (id: string, room: number) => void;

/** What the app's host hands every stack: what it keeps clear, and where its room goes. */
interface Host {
  readonly inset: ToasterInset | undefined;
  readonly report: RoomReport;
}

const HostContext = createContext<Host>({
  inset: undefined,
  report: () => {},
});
const Room = createContext(0);

/**
 * The height a stack at the window's foot covers, from its top to the
 * window's bottom, in px: none while it holds no toast.
 */
export function roomOf(stack: DOMRect, viewport: number): number {
  return stack.height > 0 ? Math.max(0, Math.ceil(viewport - stack.top)) : 0;
}

/** The kit's toaster drawn into `layer`: the one directly in it, never a nested surface's. */
function stackIn(layer: HTMLElement): HTMLElement | null {
  return layer.querySelector<HTMLElement>(":scope > .crewlet-toaster");
}

/** The app's toast host, keeping clear whatever its owners asked for. */
export function InsetToastProvider({ children }: { children: ReactNode }) {
  const [asked, setAsked] = useState<Readonly<Record<string, ToasterInset>>>({});
  const [rooms, setRooms] = useState<Readonly<Record<string, number>>>({});
  const request = useCallback<Request>((key, inset) => {
    setAsked((now) => {
      const same = now[key]?.inline === inset?.inline && now[key]?.block === inset?.block;
      if (same && (inset === null) === (now[key] === undefined)) return now;
      const next = { ...now };
      if (inset === null) delete next[key];
      else next[key] = inset;
      return next;
    });
  }, []);
  const report = useCallback<RoomReport>((id, room) => {
    setRooms((now) => {
      if ((now[id] ?? 0) === room) return now;
      const next = { ...now };
      if (room === 0) delete next[id];
      else next[id] = room;
      return next;
    });
  }, []);
  const inset = useMemo<ToasterInset | undefined>(() => {
    const all = Object.values(asked);
    if (all.length === 0) return undefined;
    return {
      inline: Math.max(0, ...all.map((i) => i.inline ?? 0)),
      block: Math.max(0, ...all.map((i) => i.block ?? 0)),
    };
  }, [asked]);
  const host = useMemo<Host>(() => ({ inset, report }), [inset, report]);
  return (
    <InsetRequest.Provider value={request}>
      <HostContext.Provider value={host}>
        <Room.Provider value={Math.max(0, ...Object.values(rooms))}>
          <HostedStack>{children}</HostedStack>
        </Room.Provider>
      </HostContext.Provider>
    </InsetRequest.Provider>
  );
}

/**
 * The toast host for a surface that draws its own layer (`LayerHost`), such
 * as the org builder, whose fullscreen paints nothing outside itself: its
 * stack stands where the app's does, keeps clear of what the app's keeps
 * clear of, and its room is the page's too.
 */
export function SurfaceToastProvider({ children }: { children: ReactNode }) {
  return <HostedStack>{children}</HostedStack>;
}

/** A provider drawing its stack at the window's foot with the app host's inset, its room said to the page. */
function HostedStack({ children }: { children: ReactNode }) {
  const { inset } = useContext(HostContext);
  return (
    <ToastProvider position="bottom-right" inset={inset}>
      <StackRoom />
      {children}
    </ToastProvider>
  );
}

/** Says the room its layer's stack covers at the foot as it changes, and withdraws it when it goes. */
function StackRoom() {
  const id = useId();
  const { report } = useContext(HostContext);
  const layer = useLayerContainer();
  useLayoutEffect(() => {
    const stack = layer ? stackIn(layer) : null;
    if (!stack) return;
    const measure = () => report(id, roomOf(stack.getBoundingClientRect(), window.innerHeight));
    measure();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    observer?.observe(stack);
    window.addEventListener("resize", measure);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
      report(id, 0);
    };
  }, [layer, id, report]);
  return null;
}

/** The room the largest stack at the window's foot covers, now ([roomOf]). */
export function useToastRoom(): number {
  return useContext(Room);
}

/**
 * Whether a reader whose scroller now ends `distance` px below its view was
 * at its end before the page's end grew by `grew` px.
 */
export function wasAtEnd(distance: number, grew: number): boolean {
  return grew > 0 && distance <= grew + 1;
}

/**
 * The room the page's end always keeps: a toast naming a long seat, which wraps
 * to three lines (87px measured, at 1440 and at a phone's width), over the kit's
 * 24px window inset, so one toast at the foot lies over the room and moves nothing.
 */
export const TOAST_REST_ROOM = 112;

/**
 * The room at the page's end: drawn last in a scrolling screen, so the last
 * row can stand above the stack, and above a bar docked at the foot.
 */
export function ToastRoom() {
  const room = useToastRoom();
  const { inset } = useContext(HostContext);
  return <RoomAtEnd room={Math.max(room, TOAST_REST_ROOM + (inset?.block ?? 0))} />;
}

function RoomAtEnd({ room }: { room: number }) {
  const ref = useRef<HTMLDivElement>(null);
  // WHAT THIS ROOM ADDED TO THE PAGE, its gap in the column included. Null
  // until it is drawn: the room a page opens with is part of the page, and a
  // reader on a short page is not at an end it took them to.
  const added = useRef<number | null>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    const scroller = screenScroller();
    if (!el || !scroller) return;
    const column = el.parentElement;
    const gap = column ? parseFloat(getComputedStyle(column).rowGap) || 0 : 0;
    const now = el.offsetHeight + gap;
    const grew = added.current === null ? 0 : now - added.current;
    added.current = now;
    if (wasAtEnd(scroller.scrollHeight - scroller.clientHeight - scroller.scrollTop, grew)) {
      scroller.scrollTop = scroller.scrollHeight;
    }
  }, [room]);
  return <div ref={ref} aria-hidden="true" style={{ height: room, flex: "none" }} />;
}

/**
 * Keep `inset` clear of toasts while this component is mounted, as `owner`;
 * null asks for nothing. Withdrawn when the component goes.
 */
export function useToastInset(owner: InsetOwner, inset: ToasterInset | null): void {
  const request = useContext(InsetRequest);
  // THIS ASKER'S OWN KEY: two of one kind are two parts, the largest kept.
  const key = `${owner}${useId()}`;
  const inline = inset?.inline;
  const block = inset?.block;
  const none = inset === null;
  useEffect(() => {
    request(key, none ? null : { inline, block });
  }, [request, key, none, inline, block]);
  useEffect(() => () => request(key, null), [request, key]);
}

/**
 * How near the window's foot a bar's bottom must stand to count as docked
 * there, in px: the frame's own inset under a screen that fills it, with room
 * to spare. A reply box that follows a short thread, mid-pane, is not docked.
 */
export const DOCKED_SLACK = 32;

/**
 * What a bar docked at the foot keeps clear, from its box: as high as its top
 * is off the foot, or nothing for a bar with no box (a column folded away) or
 * one whose bottom does not reach the foot.
 */
export function clearOf(box: DOMRect, viewport: number): number | null {
  if (box.width <= 0 || box.height <= 0) return null;
  if (viewport - box.bottom > DOCKED_SLACK) return null;
  return Math.max(0, Math.ceil(viewport - box.top));
}

/**
 * What a panel standing at the end edge keeps clear, from its box: its width
 * from the window's end edge, or nothing for a panel with no box.
 */
export function besideOf(box: DOMRect, viewport: number): number | null {
  if (box.width <= 0 || box.height <= 0) return null;
  return Math.max(0, Math.ceil(viewport - box.left));
}

/**
 * Measure `find()`'s box while `on`: now, whenever it or any box around it
 * changes size, and whenever the window does; `watch` names what it finds, so
 * a different element under a new name is measured afresh.
 *
 * WHERE A BAR STANDS IS ITS SURROUNDINGS' AS MUCH AS ITS OWN. A room's foot
 * first stands under the room's loading rows, mid-screen, and reaches the
 * foot as the transcript above it fills, without changing size itself:
 * watched alone, it was never measured again. So every box that can move it
 * is watched as well: each box around it and, at each level, the boxes laid
 * out before it, and a box added or taken away at any of those levels (the
 * loading rows giving way to the transcript) measures again. And an element
 * drawn after the asker mounts is found when it is added to the page.
 */
function useMeasured(
  find: () => HTMLElement | null,
  on: boolean,
  watch: string,
  read: (box: DOMRect) => number | null,
): number | null {
  const [value, setValue] = useState<number | null>(null);
  const finder = useRef(find);
  finder.current = find;
  const reader = useRef(read);
  reader.current = read;
  useLayoutEffect(() => {
    if (!on) {
      setValue(null);
      return;
    }
    let el: HTMLElement | null = null;
    const measure = () => setValue(el ? reader.current(el.getBoundingClientRect()) : null);
    const sizes = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    const MO = typeof MutationObserver === "undefined" ? null : MutationObserver;
    // Boxes added or taken away beside the bar, at any level: watch them too.
    const moved = MO ? new MO(() => attach()) : null;
    const attach = (): boolean => {
      el = finder.current();
      if (!el) return false;
      for (let box: Element | null = el; box && box !== document.body; box = box.parentElement) {
        sizes?.observe(box);
        if (box.parentElement) moved?.observe(box.parentElement, { childList: true });
        for (
          let before = box.previousElementSibling;
          before;
          before = before.previousElementSibling
        )
          sizes?.observe(before);
      }
      measure();
      return true;
    };
    // Not drawn yet: found when it is added to the page.
    const added = MO
      ? new MO(() => {
          if (attach()) added?.disconnect();
        })
      : null;
    if (!attach()) {
      setValue(null);
      added?.observe(document.body, { childList: true, subtree: true });
    }
    window.addEventListener("resize", measure);
    return () => {
      sizes?.disconnect();
      moved?.disconnect();
      added?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, [on, watch]);
  return value;
}

/**
 * Keep the toast stack above a bar docked at the window's foot while `on`, as
 * high as the bar's top is off the foot ([clearOf]): measured now, whenever
 * the bar changes size (a composer growing as it is typed in), and whenever
 * the window does. Withdrawn when it goes.
 */
export function useToastClearOf(bar: () => HTMLElement | null, on: boolean, watch = ""): void {
  const block = useMeasured(bar, on, watch, (box) => clearOf(box, window.innerHeight));
  useToastInset("bar", block === null ? null : { block });
}

/**
 * Keep the toast stack beside a panel standing at the window's end edge while
 * `on`, as far in as the panel's start edge ([besideOf]), so a toast never
 * lies over an open panel (Chatlet's thread, a room's details). `watch` names
 * the panel, which a different pane replaces. Withdrawn when it goes.
 */
export function useToastBeside(panel: () => HTMLElement | null, on: boolean, watch = ""): void {
  const inline = useMeasured(panel, on, watch, (box) => besideOf(box, window.innerWidth));
  useToastInset("pane", inline === null ? null : { inline });
}
