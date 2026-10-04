/**
 * Who this browser is, at the very foot of the sidebar — what they can do
 * about their own session — and the reader's own preferences.
 *
 * # Three states, and the block says which
 *
 * The answer is the PRINCIPAL the engine resolved for this browser and the
 * seat the identity directory binds it to (`lib/viewer.ts`). BOUND: a person
 * the directory binds to a seat — their name, their login and their seat, and
 * the block links to the seat's page. UNBOUND: somebody resolved with no seat,
 * which is ORDINARY (an operator outside the chart, a pipeline), so the block
 * names the login and says it is not bound rather than reporting a fault.
 * NOBODY: the block says so and offers the sign-in. Each is a sentence, never
 * a blank avatar, because "who does the engine think I am" is the question
 * every write this dashboard makes turns on — and the GRANTS ride beside it,
 * since they are what decides which of those writes and screens the reader
 * reaches.
 *
 * # The popover holds the session's own gestures
 *
 * The reader's second factor and recovery codes — only for a PERSON signed in
 * with a session, which `GET /auth/session` says: a machine credential holds
 * no second factor, and a dialog the engine refuses would be a control that
 * lies. And signing out, here or everywhere: the two ways a session ends by
 * its owner's hand, both of which end in a reload at the sign-in
 * (`lib/session.ts`).
 *
 * SIGNING OUT DOES NOT WAIT FOR THE SOCKET. The viewer is a socket question,
 * and a person the socket refuses — a seat taken out of the chart (`403
 * seat_unavailable`), a session without `state:read` — never has it answered:
 * the socket stops dialling on a refusal. So `GET /auth/session` is asked
 * whatever the viewer says, and while the viewer has not answered, a session
 * it names is offered the two sign-outs under its own login — nothing about
 * a seat or a factor, which that answer does not settle.
 *
 * # Preferences are per-browser, and they are here
 *
 * Theme, density, the zone timestamps are drawn in, and how a date is
 * written: none of them is the company's, none has a page of its own, and all
 * of them are about the person at this browser — so they sit behind the one
 * control beside that person's name. The theme also has a one-press flip
 * beside it, the kit's `ThemeToggle`, because it is the preference a reader
 * changes most; "match the system" is a choice in the popover's
 * `ThemeSwitcher`, where there is room to name it.
 *
 * BOTH ARE CONTROLLED by `lib/prefs.ts`. The kit's controls can keep their own
 * copy under a storage key, and a second copy is a second opinion about which
 * theme is on — the command palette sets it too.
 */

import { useMemo, useState } from "react";
import {
  Button,
  DensitySwitcher,
  FormField,
  IconButton,
  Popover,
  Select,
  ThemeSwitcher,
  ThemeToggle,
  useToast,
  type SelectOption,
} from "@crewlethq/ui";
import { Settings2Glyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { browserZone, useViewerPrefs, type DateFormat } from "~/lib/prefs.ts";
import { refusalText } from "~/lib/refusal.ts";
import { adoptReader } from "~/lib/reader.ts";
import { useRest } from "~/lib/useRest.ts";
import { goSignIn, signOut, signOutEverywhere } from "~/lib/session.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import { auth, type SessionAnswer } from "~/protocol/index.ts";
import { LayerBoundary } from "../boundaries.tsx";
import { lazyScreen } from "../lazyScreen.ts";

/**
 * The two dialogs a person manages their proof in, out of the sign-in chunk:
 * every signed-in reader carries this block, and few of them open either.
 */
const AuthenticatorDialog = lazyScreen("signin", (m) => m.AuthenticatorDialog);
const RecoveryCodesDialog = lazyScreen("signin", (m) => m.RecoveryCodesDialog);

/** The grants a principal carries, as one line. */
export function grantsLine(grants: readonly string[]): string {
  return grants.length > 0 ? `Holds ${grants.join(", ")}` : "Holds no grants";
}

/** What the block says about who this browser is. */
export function whoLine(
  viewer: ViewerState,
  seatName: string,
): { name: string; detail: string; grants: string } {
  if (viewer.loading) {
    return { name: "Checking who you are", detail: "Asking the engine", grants: "" };
  }
  if (viewer.anonymous) {
    return { name: "Not signed in", detail: "Sign in to read and act", grants: "" };
  }
  const grants = grantsLine(viewer.grants);
  // UNBOUND IS ORDINARY, so it is said as a fact: the login is who the engine
  // records them as, and their record is kept under it.
  if (viewer.unbound) return { name: viewer.login, detail: "Not bound to a seat", grants };
  const name = viewer.name || viewer.handle;
  // THE LOGIN AND THE SEAT, never the handle with a bare `@`: the login is
  // who signed in, the seat is where the block links, and the two differ.
  const kind = viewer.kind === "agent" ? "agent seat" : "human seat";
  const seat = seatName && seatName !== name ? `${seatName} · ${kind}` : kind;
  return { name, detail: `${viewer.login} · ${seat}`, grants };
}

/**
 * The session behind this browser, as the sign-in surface describes it, or
 * null until it has answered — or if it cannot, in which case nothing that
 * depends on it is offered.
 *
 * ASKED UNLESS THE VIEWER HAS ANSWERED NOBODY — before it has answered too,
 * see the file's note — and again when the viewer's login moves, which is a
 * different session, and when the tab comes back: for the people the socket
 * refuses this read is the ONLY way to the sign-outs, so a request lost on the
 * way must not leave them none until a reload.
 */
function useSessionAnswer(enabled: boolean, login: string): SessionAnswer | null {
  return useRest(
    // A DIFFERENT LOGIN IS A DIFFERENT SESSION, so it is a different question
    // and starts from nothing.
    enabled ? `/auth/session as ${login}` : null,
    async (signal) => {
      const session = await auth.session(signal);
      // A TAB OPENED WITH A SESSION ALREADY IN THE BROWSER learns who it is
      // read by here, since no sign-in in it ever said (`lib/reader.ts`).
      adoptReader(session.person);
      return session;
    },
    { refetchOnFocus: true },
  ).data;
}

export function UserBlock({
  viewer,
  seatName,
}: {
  viewer: ViewerState;
  /** The name of the seat this principal is bound to, from the org chart. */
  seatName: string;
}) {
  const prefs = useViewerPrefs();
  const session = useSessionAnswer(!viewer.anonymous, viewer.login);
  const [dialog, setDialog] = useState<"factor" | "codes" | null>(null);
  const who = whoLine(viewer, seatName);
  const resolved = !viewer.loading && !viewer.anonymous;
  const person = resolved && !viewer.unbound;
  const account = accountOf(viewer, session);

  const identity = (
    <>
      {/* RINGED IN THE ACCENT ONCE THE ENGINE HAS SAID WHO THIS IS: this one
          badge IS the reader, which is the single use `ring="brand"` exists
          for (`docs/reference/dashboard-design.md`). A badge standing in for
          nobody, or for an answer still out, is not the reader and is not
          ringed. */}
      <SeatAvatar
        name={person ? who.name : resolved ? viewer.login || "?" : "?"}
        size="sm"
        kind="human"
        ring={resolved ? "brand" : undefined}
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
        <a
          className="user-block-who"
          href={href(["agents", "seats", viewer.handle])}
          title={who.grants}
        >
          {identity}
        </a>
      ) : (
        <span className="user-block-who" title={who.grants || undefined}>
          {identity}
        </span>
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
        label="Account and preferences"
        side="top"
        align="start"
        trigger={(open, toggle) => (
          <IconButton
            label="Account and preferences"
            icon={<Settings2Glyph size="sm" />}
            size="sm"
            variant="ghost"
            aria-expanded={open}
            onClick={toggle}
          />
        )}
      >
        {account && (
          <AccountActions
            account={account}
            grants={who.grants}
            onFactor={() => setDialog("factor")}
            onCodes={() => setDialog("codes")}
          />
        )}
        <Preferences />
      </Popover>
      {/* OUTSIDE THE POPOVER, which closes as a dialog opens over it. */}
      {dialog === "factor" && (
        <LayerBoundary title="Two-step verification" onClose={() => setDialog(null)}>
          <AuthenticatorDialog onClose={() => setDialog(null)} />
        </LayerBoundary>
      )}
      {dialog === "codes" && (
        <LayerBoundary title="Recovery codes" onClose={() => setDialog(null)}>
          <RecoveryCodesDialog onClose={() => setDialog(null)} />
        </LayerBoundary>
      )}
    </div>
  );
}

/** What the popover may offer about the session behind this browser. */
export type Account = { kind: "nobody" } | { kind: "session"; login: string; proof: boolean };

/**
 * What the popover may offer, from the viewer and the session's own answer —
 * or null for nothing at all.
 *
 * A SESSION THE VIEWER HAS NOT ANSWERED FOR (it may never, for a person the
 * socket refuses) is offered its two sign-outs under its own login, and
 * nothing that needs a seat or a factor settled. A PERSON'S SESSION — the
 * session answer says `person` and carries a deadline — is offered its proof
 * besides; a machine's never, since it holds no second factor.
 */
export function accountOf(viewer: ViewerState, session: SessionAnswer | null): Account | null {
  if (viewer.anonymous) return { kind: "nobody" };
  if (viewer.loading)
    return session ? { kind: "session", login: session.login, proof: false } : null;
  return {
    kind: "session",
    login: viewer.login,
    proof: session?.kind === "person" && session.expires_at !== undefined,
  };
}

/**
 * The session's own gestures. Exported for its suite.
 *
 * `proof` is whether a person's second factor and recovery codes are
 * offered: a session's, and a person's.
 */
export function AccountActions({
  account,
  grants,
  onFactor,
  onCodes,
}: {
  account: Account;
  /** The grants line, said under the login — empty where nothing settles it. */
  grants: string;
  onFactor: () => void;
  onCodes: () => void;
}) {
  const toast = useToast();
  if (account.kind === "nobody") {
    return (
      <div className="preferences-account">
        <Button size="small" variant="primary" onClick={goSignIn}>
          Sign in
        </Button>
      </div>
    );
  }
  // A FAILED SIGN-OUT IS SAID, and the page stays: see `signOut` for why a
  // sign-out that nothing answered must not look like one that worked.
  const run = (gesture: () => Promise<void>, what: string) => () => {
    gesture().catch((err: unknown) =>
      toast.failed(`${what} did not go through. ${refusalText(err)}`),
    );
  };
  return (
    <div className="preferences-account">
      <span className="t-label">Signed in as</span>
      <span className="mono truncate">{account.login}</span>
      {grants && <span className="t-caption">{grants}</span>}
      {account.proof && (
        <>
          <Button size="small" variant="ghost" onClick={onFactor}>
            Two-step verification…
          </Button>
          <Button size="small" variant="ghost" onClick={onCodes}>
            New recovery codes…
          </Button>
        </>
      )}
      <Button size="small" variant="secondary" onClick={run(signOut, "Signing out")}>
        Sign out
      </Button>
      <Button
        size="small"
        variant="ghost"
        title="Ends every session you hold, on every device"
        onClick={run(signOutEverywhere, "Signing out everywhere")}
      >
        Sign out everywhere
      </Button>
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
export function Preferences() {
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
