/**
 * How a screen tells the assistant what it shows and which actions it takes.
 *
 * A screen calls this with what it already holds: the records in view, what
 * is selected, the version of the data it drew, and its page actions
 * (`context.ts` says what each is for). It is registered while the screen is
 * mounted and withdrawn when it goes, so the next screen never inherits the
 * last one's actions.
 *
 * KEYED ON CONTENT, NOT IDENTITY: a screen rebuilds its arrays on every push,
 * and a registration that moved with every render would tell the drawer the
 * page changed several times a second. The actions are read through the
 * latest render's object, so a handler that closes over newer state is the one
 * that runs.
 */

import { useEffect, useLayoutEffect, useRef } from "react";
import { registerPage, type AssistantPageRegistration, type PageActionSpec } from "./context.ts";

export function useAssistantContext(registration: AssistantPageRegistration): void {
  const latest = useRef(registration);
  useLayoutEffect(() => {
    latest.current = registration;
  });
  const key = JSON.stringify([
    registration.resources ?? [],
    registration.selection ?? null,
    registration.dataVersion ?? "",
    Object.keys(registration.actions ?? {}).sort(),
  ]);
  useEffect(() => {
    const actions: Record<string, PageActionSpec<never>> = {};
    for (const id of Object.keys(latest.current.actions ?? {})) {
      // EVERY HALF THROUGH THE LATEST RENDER, so a setter that closes over
      // newer state is the one that applies.
      const at = () => latest.current.actions?.[id] as PageActionSpec<unknown> | undefined;
      const first = at()!;
      actions[id] = {
        scope: first.scope,
        ...(first.confirm === undefined ? {} : { confirm: first.confirm }),
        parse: (raw) => at()?.parse(raw) ?? { ok: false, reason: "The page no longer takes it." },
        preview: (value) => at()?.preview(value) ?? [],
        blocked: (value) => (at() ? at()!.blocked(value) : "The page no longer takes it."),
        apply: (value) => at()?.apply(value),
      } as PageActionSpec<never>;
    }
    return registerPage({ ...latest.current, actions });
    // The dependency is the CONTENT; see the module doc.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
}
