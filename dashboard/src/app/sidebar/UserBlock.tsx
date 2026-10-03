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
 * Theme, density, the zone timestamps are drawn in, how a date is written,
 * and the token: none of them is the company's, none has a page of its own,
 * and all of them are about the person at this browser — so they sit behind
 * the one control beside that person's name. The theme also has a one-press
 * flip beside it, the kit's `ThemeToggle`, because it is the preference a
 * reader changes most; "match the system" is a choice in the popover's
 * `ThemeSwitcher`, where there is room to name it.
 *
 * BOTH ARE CONTROLLED by `lib/prefs.ts`. The kit's controls can keep their own
 * copy under a storage key, and a second copy is a second opinion about which
 * theme is on — the command palette sets it too.
 */

import { useMemo } from "react";
import {
  Button,
  DensitySwitcher,
  FormField,
  IconButton,
  Popover,
  Select,
  ThemeSwitcher,
  ThemeToggle,
  type SelectOption,
} from "@crewlethq/ui";
import { KeyGlyph, Settings2Glyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { browserZone, useViewerPrefs, type DateFormat } from "~/lib/prefs.ts";
import type { ViewerState } from "~/lib/viewer.ts";

/** What the block says about who this browser is. */
export function whoLine(viewer: ViewerState, seatName: string): { name: string; detail: string } {
  if (viewer.loading) return { name: "Checking who you are", detail: "Asking the engine" };
  if (viewer.anonymous) return { name: "Anonymous", detail: "No API token — reading only" };
  if (viewer.unbound) {
    return { name: `Token ${viewer.operatorID}`, detail: "Not bound to a seat" };
  }
  const name = viewer.name || viewer.handle;
  // THE SEAT'S ROLE AND KIND, never the handle with a bare `@`: the handle is
  // an address, and the address is where the block links.
  const kind = viewer.kind === "agent" ? "agent seat" : "human seat";
  return { name, detail: seatName && seatName !== name ? `${seatName} · ${kind}` : kind };
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
        <span className="user-block-detail truncate">{who.detail}</span>
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

const DATE_LABELS: Record<DateFormat, string> = {
  auto: "This browser's own",
  iso: "2026-09-22",
  long: "September 22, 2026",
};

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
  const zones = useMemo<SelectOption[]>(() => {
    const own = browserZone();
    return [
      // EMPTY IS "THE BROWSER'S", which is a real choice rather than an
      // absent one: a reader who travels wants the zone to follow them.
      { value: "", label: `This browser's (${own})` },
      ...supportedZones().map((z) => ({ value: z, label: z.replace(/_/g, " ") })),
    ];
  }, []);
  return (
    <div className="preferences">
      {/* THE TWO SWITCHERS NAME THEMSELVES: each is a radio group carrying
          its own label, so a second label round it would be read twice. */}
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Theme
        </span>
        <ThemeSwitcher label="Theme" value={prefs.theme} onChange={prefs.setTheme} size="sm" />
      </div>
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Density
        </span>
        <DensitySwitcher
          label="Density"
          value={prefs.density}
          onChange={prefs.setDensity}
          size="sm"
        />
      </div>
      <FormField
        label="Time zone"
        helper="Every timestamp is drawn in it. A day is still the company's."
      >
        {(field) => (
          <Select
            id={field.id}
            value={prefs.timezoneChosen ? prefs.timezone : ""}
            onChange={(value) => prefs.setTimezone(String(value))}
            options={zones}
            searchable
            searchPlaceholder="Find a zone"
            size="sm"
            width="full"
          />
        )}
      </FormField>
      <FormField label="Dates">
        {(field) => (
          <Select
            id={field.id}
            value={prefs.dateFormat}
            onChange={(value) => prefs.setDateFormat(value as DateFormat)}
            options={(Object.keys(DATE_LABELS) as DateFormat[]).map((f) => ({
              value: f,
              label: DATE_LABELS[f],
            }))}
            size="sm"
            width="full"
          />
        )}
      </FormField>
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

/**
 * Every IANA zone this runtime can format in.
 *
 * `Intl.supportedValuesOf` is the runtime's own list, so every entry offered
 * is one `setZone` will accept. A runtime without it (an older engine's
 * embedded browser, a test environment) offers the browser's own zone and
 * UTC, which is every zone it can promise.
 *
 * UTC IS ADDED, NOT ASSUMED. The runtime's list is its CANONICAL zones, and
 * V8's does not name `UTC` among them — it lists `Etc/…` and the cities — so
 * the one zone an operator reading logs most often wants was the one a reader
 * could not find. It formats everywhere, so offering it costs nothing.
 */
export function supportedZones(): string[] {
  const intl = Intl as unknown as { supportedValuesOf?: (key: string) => string[] };
  try {
    const all = intl.supportedValuesOf?.("timeZone");
    if (all && all.length > 0) return all.includes("UTC") ? all : ["UTC", ...all];
  } catch {
    // fall through to the two zones every runtime has
  }
  const own = browserZone();
  return own === "UTC" ? ["UTC"] : [own, "UTC"];
}
