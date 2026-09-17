/**
 * An object's tabs, bound to the URL and RESOLVED against the tabs it has.
 *
 * # Why this is a hook and not a component
 *
 * There were two tab widgets in this tree before there was a design system:
 * `ui/primitives.tsx`'s `Tabs`, which four screens rendered, and a second one
 * here, complete down to its own roving focus and imported by nobody. Both are
 * gone — `@crewlethq/ui`'s `Tabs` mints the `aria-controls`/`aria-labelledby`
 * pair and moves focus with the arrow keys, which is every part of the gesture
 * a component can own.
 *
 * What it cannot own is the part below, and that is why this file is a HOOK
 * and not a widget: no tab component knows which tabs an object HAS.
 *
 * # What was genuinely missing
 *
 * WHICH TAB IS REAL. `tab=` is a string off a URL and the tab set is a
 * property of the OBJECT: a human seat has two tabs and an agent seat has
 * five, a company has two lenses, the configuration has three. Every one of
 * those screens cast the parameter straight to its own union and rendered
 * `{tab === "overview" && …}` down the page, so a value naming no tab
 * selected nothing and matched no branch: the header, the strip and then
 * nothing at all — a blank page from a bookmark, a hand-typed URL, or a link
 * made before the seat's kind changed.
 *
 * The seat screen had HALF of the check, for the human case only, with a
 * comment explaining exactly why it was needed; the agent case beside it was
 * a bare cast. That is the tell that this belongs in one place rather than in
 * a convention each screen follows as far as it happened to think about it.
 *
 * So the resolution is the hook's, the tab a caller renders is the RESOLVED
 * one, and a caller cannot get at the raw parameter to render it by mistake.
 *
 * # The digits
 *
 * `1`–`9` select a tab, which is the shortcut every product of this shape has
 * and the first one a reader tries. It is bound here rather than in the
 * widget because the binding belongs to the PARAMETER, not to the strip: a
 * screen may draw its tabs in two places (a page and the peek over it) and
 * both would bind the same digits to the same setter.
 *
 * Only a `section` binds them. That is the grammar this product already has —
 * a section is the page you are on, a filter narrows what is on it — so an
 * object's own tabs take the digits and a thread picker or a lens inside a
 * peek leaves them for whatever else wants them.
 */

import { useParam } from "../router.tsx";
import { useKeyChords } from "~/lib/keys.ts";

export function useTab<T extends string>(
  key: string,
  tabs: readonly T[],
  kind: "section" | "filter" = "section",
): [T, (value: T) => void] {
  // THE FIRST TAB IS THE FALLBACK, which is also what makes the URL clean:
  // `useParam` drops a parameter equal to its fallback, so landing on an
  // object writes no `tab=` at all and only a reader who moved carries one.
  const first = tabs[0] as T;
  const [asked, set] = useParam(key, first, kind);
  const shown = (tabs as readonly string[]).includes(asked) ? (asked as T) : first;

  // ONE BINDING PER TAB rather than a range check, because the chord list IS
  // the shortcut table: a digit past the end of this object's tabs has no
  // binding and falls through to whatever else wants it, instead of being
  // swallowed by a strip that could not have used it.
  useKeyChords(
    tabs.slice(0, 9).map((tab, i) => ({
      key: String(i + 1),
      run: () => set(tab),
      when: kind === "section",
    })),
  );

  return [shown, set as (value: T) => void];
}
