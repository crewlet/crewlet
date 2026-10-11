/**
 * Who this browser is, at the very foot of the sidebar, and the reader's own
 * preferences.
 *
 * # Three states, and only one of them is a person
 *
 * BOUND: a token whose operator id a human seat names in
 * `contact.crewlet_operator_id`. The block is that person — their name, their
 * seat — and it links to their seat's page. UNBOUND: a token no seat claims,
 * which is ordinary (an operator's automation token is one), so the block
 * names the token and says what would bind it rather than reporting a fault.
 * ANONYMOUS: no token at all; the block says so and offers to set one. Each
 * state is a sentence, never a blank avatar, because "who does the engine
 * think I am" is the question every write this dashboard makes will turn on.
 *
 * # Preferences are per-browser, and they are here
 *
 * Mode, density, theme and the token: none of them is the company's, and all
 * of them are about the person at this browser, so they sit behind the one
 * control beside that person's name. Settings › Appearance offers the same
 * three with room to show every theme. The mode also has a one-press flip
 * beside the name, the kit's `ThemeToggle`, because it is the preference a
 * reader changes most; "match the system" is a choice in the popover's
 * `ThemeSwitcher`, where there is room to name it.
 *
 * EVERY CONTROL IS CONTROLLED by `lib/prefs.ts`. The kit's controls can keep
 * their own copy under a storage key, and a second copy is a second opinion
 * about which mode is on; the command palette sets them too.
 *
 * THE THEME ROW PREVIEWS. Its list repaints the whole frame in the theme the
 * highlight rests on, and puts back the theme in effect when the list closes
 * or the pointer leaves it; Enter or a click applies (see `app/appearance.tsx`).
 */

import {
  Button,
  DensitySwitcher,
  IconButton,
  Popover,
  Select,
  ThemeSwitcher,
  ThemeToggle,
} from "@crewlethq/ui";
import { isThemeId } from "@crewlethq/tokens";
import { KeyGlyph, Settings2Glyph } from "@crewlethq/icons/glyphs";
import { THEME_OPTIONS, useApplyTheme } from "~/app/appearance.tsx";
import { href } from "~/app/router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { previewPalette, useViewerPrefs } from "~/lib/prefs.ts";
import type { ViewerState } from "~/lib/viewer.ts";

/** What the block says about who this browser is. */
export function whoLine(viewer: ViewerState, seatName: string): { name: string; detail: string } {
  if (viewer.loading) return { name: "Checking who you are", detail: "Asking the engine" };
  if (viewer.anonymous) return { name: "Anonymous", detail: "No API token, reading only" };
  if (viewer.unbound) {
    return { name: `Token ${viewer.operatorID}`, detail: "Not bound to a seat" };
  }
  const name = viewer.name || viewer.handle;
  // THE SEAT'S ROLE, never the handle with a bare `@`: the handle is an
  // address, and the address is where the block links. A person is not
  // labelled a "human seat", which says what the block already shows; a
  // token bound to an agent's seat is named as one, because acting as an
  // agent is the fact a reader must not miss.
  const role = seatName && seatName !== name ? seatName : "";
  if (viewer.kind === "agent")
    return { name, detail: role ? `${role} · agent seat` : "agent seat" };
  return { name, detail: role };
}

export function UserBlock({
  viewer,
  seatName,
  onSetToken,
}: {
  viewer: ViewerState;
  /** The name of the seat this token is bound to, from the org chart. */
  seatName: string;
  onSetToken: () => void;
}) {
  const prefs = useViewerPrefs();
  const who = whoLine(viewer, seatName);
  const person = !viewer.loading && !viewer.anonymous && !viewer.unbound;

  const identity = (
    <>
      <SeatAvatar
        name={person ? who.name : viewer.anonymous ? "?" : viewer.operatorID || "?"}
        size="sm"
        kind="human"
        decorative
      />
      <span className="user-block-text">
        <span className="user-block-name truncate">{who.name}</span>
        {who.detail ? <span className="user-block-detail truncate">{who.detail}</span> : null}
      </span>
    </>
  );

  return (
    <div className="user-block">
      {person ? (
        <a className="user-block-who" href={href(["agents", "seats", viewer.handle])}>
          {identity}
        </a>
      ) : (
        <span className="user-block-who">{identity}</span>
      )}
      <ThemeToggle size="sm" value={prefs.theme} onChange={prefs.setTheme} />
      {/* START-ALIGNED, OPENING INTO THE ROOM TO ITS RIGHT. The trigger sits at
          the sidebar's inner edge, a couple of hundred pixels from the left of
          the window, so an end-aligned panel would have to be slid back
          across the window edge to fit at all.

          And the kit cannot draw it end-aligned from here: @crewlethq/ui
          0.5.0's Popover (and Menu) asks whether the anchor left the layer
          using the END-ALIGNED PANEL'S left edge with the ANCHOR'S width, so
          any panel wider than the anchor's right edge plus its width reads as
          an anchor carried off screen and closes on the frame it opened. That
          is a kit defect, raised against uilet; the test beside this block
          lays the sidebar out at its real coordinates so a return to `end`
          goes red while the kit still has it. */}
      <Popover
        label="Preferences"
        side="top"
        align="start"
        trigger={(open, toggle) => (
          <IconButton
            label="Preferences"
            icon={<Settings2Glyph size="sm" />}
            size="sm"
            variant="ghost"
            aria-expanded={open}
            onClick={toggle}
          />
        )}
      >
        <Preferences onSetToken={onSetToken} tokenPresented={!viewer.anonymous} />
      </Popover>
    </div>
  );
}

/**
 * The per-browser preferences. Exported for its suite, which drives each
 * control and reads `lib/prefs.ts` back.
 */
export function Preferences({
  onSetToken,
  tokenPresented,
}: {
  onSetToken: () => void;
  tokenPresented: boolean;
}) {
  const prefs = useViewerPrefs();
  const applyTheme = useApplyTheme();
  return (
    <div className="preferences">
      {/* EVERY CONTROL NAMES ITSELF: each switcher is a radio group carrying
          its own label and the select is named Theme, so a second label round
          one would be read twice. The two switchers FILL one box of one width,
          so a row of glyphs and a row of letters draw the same cells. */}
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Mode
        </span>
        <div className="preferences-switch">
          <ThemeSwitcher
            label="Mode"
            value={prefs.theme}
            onChange={prefs.setTheme}
            size="sm"
            fill
          />
        </div>
      </div>
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Density
        </span>
        <div className="preferences-switch">
          <DensitySwitcher
            label="Density"
            value={prefs.density}
            onChange={prefs.setDensity}
            size="sm"
            fill
          />
        </div>
      </div>
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Theme
        </span>
        <Select
          ariaLabel="Theme"
          size="sm"
          menuClassName="preferences-theme-menu"
          options={THEME_OPTIONS}
          value={prefs.palette}
          onChange={(next) => {
            if (isThemeId(next)) applyTheme(next);
          }}
          onHighlight={(option) =>
            previewPalette(option && isThemeId(option.value) ? option.value : null)
          }
        />
      </div>
      <Button
        size="small"
        variant="secondary"
        leadingIcon={<KeyGlyph size="sm" />}
        onClick={onSetToken}
      >
        {tokenPresented ? "Change the API token" : "Set an API token"}
      </Button>
    </div>
  );
}
