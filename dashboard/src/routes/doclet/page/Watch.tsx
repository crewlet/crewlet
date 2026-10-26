/**
 * Watching a page: the eye in the page bar, and who else is told.
 *
 * # Two controls, because they are two questions
 *
 * The eye answers "am I told when this changes" and changes it in one press
 * (`save_page` with `watch`, as the person signed in). The count
 * beside it answers "who is told" and opens the list. A hover panel over the
 * eye would carry a list no keyboard, touch or screen reader reaches, which is
 * what `Tooltip` refuses to be; a `Popover` is a list a reader can keep open.
 *
 * # Who is told
 *
 * A watcher is a seat in `watchers` that is not in `muted`: an unwatch MUTES
 * rather than deletes, so a seat that stopped following is not quietly
 * subscribed again by a later mention. The engine's own rule
 * (`internal/doclet` `recipientsOf`), read the same way here.
 *
 * # Which name is "you"
 *
 * A watch is filed under the SEAT the writer acts as (`ViewerState.handle`),
 * because a wake reaches a seat and nothing else. A credential the directory
 * binds to no seat subscribes nobody (`internal/doclet` `Actor.subscriber`):
 * its login is no seat a change could be told to, so the eye is drawn
 * disabled for it with that reason rather than offering a press the engine
 * would answer by moving nothing.
 */

import { Button, IconButton, Popover, useToast } from "@crewlethq/ui";
import { EyeGlyph } from "@crewlethq/icons/glyphs";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { Page } from "~/protocol/index.ts";
import { Face, usePeople } from "../parts.tsx";

/** The seats told when a page changes: its watchers, less the ones who muted it. */
export function followersOf(page: Pick<Page, "watchers" | "muted">): string[] {
  const muted = new Set(page.muted ?? []);
  return (page.watchers ?? []).filter((handle) => !muted.has(handle));
}

export function WatchControl({ page }: { page: Page }) {
  const viewer = useViewer();
  const who = usePeople();
  const toast = useToast();
  const save = useAct("save_page");
  const followers = followersOf(page);
  const me = viewer.handle;
  const watching = me !== "" && followers.includes(me);
  // NO SEAT, NO SUBSCRIPTION: see "Which name is you" above.
  const held = !save.access.can
    ? save.access.reason
    : viewer.unbound
      ? "A watch is kept under a seat, and this credential is bound to none, so nothing could tell it about a change."
      : undefined;
  const names = followers.map((h) => (h === me ? "you" : who(h).name));
  const said = names.length > 0 ? `Watching: ${names.join(", ")}` : "Nobody watches this page";

  const press = async () => {
    const result = await save.run(
      { page: page.id, base_version: page.version, watch: !watching },
      {
        done: watching ? `You stopped watching “${page.title}”.` : `Watching “${page.title}”.`,
      },
    );
    // A REFUSAL IS SAID WHERE IT CAN BE SEEN: the bar has no room for a note
    // under the eye, and the page answering a newer version than the one the
    // press was against is the usual cause.
    if (result?.kind === "refused") {
      toast.show({
        variant: "warning",
        title: "The watch did not change",
        message: [result.sentence, result.hint].filter(Boolean).join(" "),
      });
    }
  };

  return (
    <span className="knowledge-watch">
      <IconButton
        icon={<EyeGlyph size="sm" />}
        label={watching ? "Stop watching this page" : "Watch this page"}
        variant="ghost"
        pressed={watching}
        disabledReason={held}
        onClick={() => {
          if (!save.busy) void press();
        }}
      />
      <Popover
        label="Who watches this page"
        align="end"
        trigger={(open, toggle) => (
          <Button
            size="small"
            variant="ghost"
            aria-expanded={open}
            aria-label={`${said}. Show who watches this page`}
            onClick={toggle}
          >
            {followers.length.toLocaleString()}
          </Button>
        )}
      >
        <div className="knowledge-watchers">
          <p className="knowledge-watchers-head">Watching · {followers.length.toLocaleString()}</p>
          {followers.length === 0 ? (
            <p className="knowledge-note">Nobody watches this page yet.</p>
          ) : (
            <ul className="knowledge-watchers-list" role="list">
              {followers.map((handle) => (
                <li key={handle}>
                  <Face handle={handle} who={who} />
                  <span>
                    {who(handle).name}
                    {handle === me && <span className="knowledge-note"> (you)</span>}
                  </span>
                </li>
              ))}
            </ul>
          )}
          <p className="knowledge-note">
            {held !== undefined
              ? held
              : watching
                ? "Press the eye to stop watching."
                : "Press the eye to watch."}
          </p>
        </div>
      </Popover>
    </span>
  );
}
