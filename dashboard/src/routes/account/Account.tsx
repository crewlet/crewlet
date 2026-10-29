/**
 * `#/account` — the signed-in reader's own page: who the engine holds them as,
 * how they prove it, where they are signed in, and the tokens that act as them.
 *
 * # Everything here is the reader's own, and the routes say so
 *
 * Each read and write names the CALLER, never somebody picked: `GET
 * /auth/session` says who this browser is, `GET /iam/people/{own id}` and its
 * sessions are the self arm of the directory's read, `GET /iam/credentials`
 * with no `?person=` is the caller's own, a token is minted with no
 * `?person=` — the one way a person's token is ever minted, since whoever
 * mints one is shown a value that acts as its owner — and a session is ended
 * by its lineage, which the engine decides on the session's owner. So the page
 * holds no id it could be talked into swapping for somebody else's.
 *
 * # What an administrator changes is shown, never edited
 *
 * A login, a name, an address, a seat and access are the directory's, and a
 * person changing their own would be the escalation the directory exists to
 * close — the engine refuses it. The profile reads them and says who changes
 * them.
 *
 * # Changing the password keeps this browser, and ends everything else
 *
 * `POST /auth/password` takes the current password as its proof — and, from
 * somebody who holds a second factor, a code, exactly as a step-up asks it;
 * no step-up is asked first — moves the person's revocation epoch, which ends
 * every other session and every personal token, and answers a fresh session
 * for this browser. Where
 * this node has not applied the change yet it answers no session (`202`): the
 * cookie is cleared, so the tab goes to the sign-in at once, a toast saying
 * why — every read here would be refused, and the socket the engine closes
 * would send it there anyway, with nothing said.
 *
 * # Every gesture re-reads who this browser is
 *
 * A gesture the engine asks a step-up for — a token's mint or revocation, the
 * authenticator, new recovery codes — REPLACES this browser's session when
 * the confirmation is made, under a new lineage, and so does a password
 * change. So every gesture re-reads `/auth/session` before the lists: the
 * "This browser" mark read before it would leave this browser's new session
 * offered a named sign-out, which ends this browser.
 *
 * # What a person comes to check is on the first screen
 *
 * Security leads with two-step verification — its state in a line and its
 * button — and the password is a line of its own whose "Change password…"
 * opens the form in a dialog, as the authenticator's setup does. The form
 * drawn in place stood four fields tall above the second factor and pushed it
 * below the fold at 1280×720 and on a phone, so a person looking for two-step
 * verification found a password form and concluded there was none.
 *
 * # A credential that is not a person's sees what it is
 *
 * A Tier A token exchanged for a session is the deployment's credential, with
 * no password, second factor or token of its own — so the page names it and
 * draws none of the personal sections, each of which the engine would refuse.
 * Its note says so too: the page's one sentence is chosen by what the session
 * is, because the person's — "your password, your second factor…" — stood
 * directly above a card saying this credential has none of them.
 */

import { useCallback, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Card,
  Checkbox,
  EmptyValue,
  FormField,
  Input,
  Link,
  Modal,
  Skeleton,
  Tag,
  Text,
  useToast,
  DescriptionList,
  Inline,
  InlineCode,
  Stack,
  TableDateCell,
  TableEntityCell,
  TableTextCell,
  PageNote,
} from "@crewlethq/ui";
import { KeyGlyph, MonitorGlyph, ShieldUserGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import {
  ConfirmDialog,
  endedWord,
  ExpiresCell,
  AccessTags,
  MintTokenDialog,
} from "~/components/people.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { linkedSeat, plainSeat } from "~/app/frame/cells.tsx";
import { LayerBoundary } from "~/app/boundaries.tsx";
import { href } from "~/app/router.tsx";
import { peekHref } from "~/app/frame/peekAddress.ts";
import { ADMIN, holds } from "~/lib/access.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useIamGesture } from "~/lib/iamWrite.ts";
import { refusalText } from "~/lib/refusal.ts";
import { goSignIn } from "~/lib/session.ts";
import { ownsAccount, tierATokenOf } from "~/lib/login.ts";
import { SignOutEverywhereDialog } from "~/components/SignOutEverywhere.tsx";
import { handleLabel, indexOrg } from "~/lib/seats.ts";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { useWaiting } from "~/lib/waiting.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { auth, pathSegment, rest, RestError, type SessionAnswer } from "~/protocol/index.ts";
import type { CredentialRow, DirectoryRow, SessionRow } from "~/routes/settings/Access.tsx";
import { AuthenticatorDialog, RecoveryCodesDialog } from "~/routes/signin/SecondFactor.tsx";
import { NewPasswordFields, newPasswordReady } from "~/routes/signin/NewPassword.tsx";

/** How the page's reads are kept current: the tab coming back asks again. */
const READ = { refetchOnFocus: true };

/** The list an `/iam` answer carries under `field`, or none. */
function listOf<T>(answer: unknown, field: string): T[] {
  const list = (answer as Record<string, unknown> | null)?.[field];
  return Array.isArray(list) ? (list as T[]) : [];
}

export function Account() {
  // THE FRAME'S READ of who this is (`lib/frameSession.ts`), re-read through
  // it after every gesture: a read of its own was a second answer to the one
  // question, asked again on every focus beside the frame's.
  const session = useFrameSession();
  const sessionMoved = session.reload;
  const answer = session.answer;
  // NOTHING TO SAY UNTIL THE ENGINE HAS ANSWERED, and the page says that much
  // rather than nothing: the note is chosen by what the session is, so it
  // waits too, and `QueryState` draws nothing for a read still out — which
  // left the reader's own page blank for as long as the read took.
  if (!answer && session.code === null) {
    return <Skeleton variant="text" rows={4} label="Reading who you are signed in as" />;
  }
  if (!answer) {
    return (
      <QueryState
        error={session.code}
        refusal={session.refusal}
        detail={session.error?.detail || undefined}
        loading={session.loading}
      />
    );
  }
  return (
    <>
      <PageNote>{noteFor(answer)}</PageNote>
      {ownsAccount(answer) ? (
        <PersonAccount session={answer} onSessionMoved={sessionMoved} />
      ) : (
        <NotAPerson session={answer} />
      )}
    </>
  );
}

/**
 * The page's one sentence, for the session it shows.
 *
 * CHOSEN BY WHAT THE SESSION IS, and so drawn only once `/auth/session` has
 * said: the person's sentence promises a password, a second factor, sessions
 * and tokens, and drawn for every session it stood directly above the card
 * telling a Tier A token's session that it holds none of them.
 */
function noteFor(session: SessionAnswer): string {
  if (ownsAccount(session)) {
    return "Your profile, security, sessions and tokens.";
  }
  return tierATokenOf(session.login)
    ? "Signed in with an API token. Ends within an hour."
    : "Who this browser is signed in as. A credential that isn’t a person has no password, two-step verification or personal tokens to change here.";
}

/** A session that is not a person's: what it is and what it holds, and nothing to manage. */
function NotAPerson({ session }: { session: SessionAnswer }) {
  const token = tierATokenOf(session.login);
  return (
    <Card>
      <Card.Header icon={<KeyGlyph size="sm" />}>
        <Card.Title>{token ? "API token" : "Not a person’s account"}</Card.Title>
      </Card.Header>
      <Stack gap={3}>
        {/* WHICH CREDENTIAL, and nothing about what it lacks: the note above
            says that once, for the page. */}
        <Text as="p" variant="body">
          {token ? (
            <>
              You’re signed in with the API token <InlineCode>{token}</InlineCode>.
            </>
          ) : (
            <>
              You’re signed in as <InlineCode>{session.login}</InlineCode>, which isn’t a person.
            </>
          )}
        </Text>
        <Inline gap={2} wrap>
          <Text variant="label">Access</Text>
          <AccessTags grants={session.grants} />
        </Inline>
      </Stack>
    </Card>
  );
}

/** A person's own account: every section. */
function PersonAccount({
  session,
  onSessionMoved,
}: {
  session: SessionAnswer;
  /** Read who this browser is again; resolves once that read has settled. */
  onSessionMoved: () => Promise<void>;
}) {
  const profile = useRest(
    `/iam/people/${pathSegment(session.person)}`,
    async (signal) =>
      (await rest.get(`/iam/people/${pathSegment(session.person)}`, signal)) as DirectoryRow,
    READ,
  );
  const credentials = useRest(
    "/iam/credentials",
    async (signal) =>
      listOf<CredentialRow>(await rest.get("/iam/credentials", signal), "credentials"),
    READ,
  );
  const sessions = useRest(
    `/iam/people/${pathSegment(session.person)}/sessions`,
    async (signal) =>
      listOf<SessionRow>(
        await rest.get(`/iam/people/${pathSegment(session.person)}/sessions`, signal),
        "sessions",
      ),
    READ,
  );
  const { reload: reloadCredentials } = credentials;
  const { reload: reloadSessions } = sessions;
  // ONE RE-READ AFTER EVERY GESTURE, who this browser is FIRST: a step-up or a
  // password change gave it a new session, and the list compared against the
  // old lineage would offer this browser a named sign-out.
  const moved = useCallback(() => {
    void onSessionMoved().then(() => {
      void reloadCredentials({ quiet: true });
      void reloadSessions({ quiet: true });
    });
  }, [onSessionMoved, reloadCredentials, reloadSessions]);
  const grants = session.grants ?? [];

  return (
    <Stack gap={4}>
      <Profile session={session} profile={profile} />
      <Security credentials={credentials} onChanged={moved} />
      <Sessions session={session} sessions={sessions} onChanged={moved} />
      <Tokens
        owner={{ id: session.person, login: session.login, grants: profile.data?.grants ?? grants }}
        held={grants}
        credentials={credentials}
        onChanged={moved}
      />
    </Stack>
  );
}

/**
 * What the directory holds about the reader, read-only.
 *
 * AT THE CARD'S OWN INSET: `padding="none"` is for a card whose body is a
 * flush grid, and around a list of facts it left the first of them, Login,
 * touching the header's rule.
 */
function Profile({
  session,
  profile,
}: {
  session: SessionAnswer;
  profile: RestResult<DirectoryRow>;
}) {
  const row = profile.data;
  const seat = row?.seat ?? session.seat ?? "";
  const sealed = <EmptyValue label="Locked: this node can’t decrypt it" />;
  return (
    <Card>
      <Card.Header icon={<UserGlyph size="sm" />}>
        <Card.Title>Profile</Card.Title>
      </Card.Header>
      <QueryState
        error={profile.code}
        refusal={profile.refusal}
        detail={profile.error?.detail || undefined}
        loading={profile.loading && !row}
      >
        <DescriptionList>
          <DescriptionList.Item term="Login">
            <Text variant="cell" mono>
              {row?.login || session.login}
            </Text>
          </DescriptionList.Item>
          <DescriptionList.Item term="Name">
            {row?.sealed ? sealed : row?.name || <EmptyValue label="No name" />}
          </DescriptionList.Item>
          <DescriptionList.Item term="Email">
            {row?.sealed ? sealed : row?.email || <EmptyValue label="No address" />}
          </DescriptionList.Item>
          <DescriptionList.Item term="Seat">
            {seat ? (
              <SeatFact handle={seat} />
            ) : (
              // A PERSON ALWAYS HOLDS ONE (ADR-0032), so this is a session
              // that acts as its own credential rather than as a seat.
              <EmptyValue label="No seat" />
            )}
          </DescriptionList.Item>
          <DescriptionList.Item term="Access">
            <AccessTags grants={row?.grants ?? session.grants} />
          </DescriptionList.Item>
        </DescriptionList>
      </QueryState>
      <Text as="p" variant="caption">
        {/* AN ADMIN IS SENT TO THEIR OWN ROW, where these are changed. */}
        {holds(row?.grants ?? session.grants ?? [], ADMIN) ? (
          <>
            Change these in{" "}
            <Link href={peekHref({ kind: "person", id: session.person })}>People</Link>.
          </>
        ) : (
          "Only an admin can change these."
        )}
      </Text>
    </Card>
  );
}

/**
 * The reader's seat, written as the product writes a seat everywhere: its
 * badge and name (the grid's seat cell), then its `@handle`.
 *
 * IT WAS THE NAME AND THE BARE HANDLE, and where the chart could not be read
 * the name fell back to the handle — so it read "Bob bob", and before the
 * chart had arrived "cara cara". A seat the chart does not name here is its
 * handle ALONE, since there is no name to put beside it.
 *
 * A LINK ONLY WHERE THE SEAT'S PAGE IS SERVED: every screen but this one reads
 * the live socket, which serves every signed-in reader the overview — but not
 * a reader the engine refused it to (a seat taken out of the chart), for whom
 * the seat would link to the panel refusing them the company.
 */
function SeatFact({ handle }: { handle: string }) {
  const linked = useConnection().accessRefused === null;
  const org = useOrg();
  const held = indexOrg(org).byHandle.get(handle);
  const at = handleLabel(handle);
  if (!held) {
    return linked ? (
      <Link href={href(["company", "org", "seats", handle])} size="inherit">
        <Text variant="cell" mono>
          {at}
        </Text>
      </Link>
    ) : (
      <Text variant="cell" mono>
        {at}
      </Text>
    );
  }
  return (
    <Inline as="span" gap={2} wrap>
      {linked ? (
        <TableEntityCell
          {...linkedSeat({ handle: handle, name: held.name, kind: held.kind, avatar: held.avatar })}
        />
      ) : (
        <TableEntityCell
          {...plainSeat({ name: held.name, kind: held.kind, avatar: held.avatar })}
        />
      )}
      <Text variant="cell" mono tone="tertiary">
        {at}
      </Text>
    </Inline>
  );
}

/**
 * The second factor, the recovery codes and the password: a line each, with
 * the gesture beside it.
 *
 * TWO-STEP VERIFICATION FIRST, because it is what a person comes to this card
 * to check — and the password a line rather than its form. The form drawn in
 * place stood above the second factor and pushed it below the fold at
 * 1280×720 and on a phone; it opens in a dialog now, as the authenticator's
 * setup does.
 */
function Security({
  credentials,
  onChanged,
}: {
  credentials: RestResult<CredentialRow[]>;
  onChanged: () => void;
}) {
  const [dialog, setDialog] = useState<"factor" | "codes" | "password" | null>(null);
  const live = (credentials.data ?? []).filter((c) => !c.revoked);
  const app = live.some((c) => c.method === "totp");
  const codes = live.some((c) => c.method === "recovery");
  // WHAT THE ENGINE ASKS A CODE OF: any second factor held, the app or the
  // recovery codes — its own reading — and unknown while the list is.
  const holdsFactor = credentials.data === null ? null : app || codes;
  const close = () => {
    setDialog(null);
    onChanged();
  };
  return (
    <Card>
      <Card.Header icon={<ShieldUserGlyph size="sm" />}>
        <Card.Title>Security</Card.Title>
      </Card.Header>
      <Stack gap={4}>
        {credentials.data === null ? (
          <Stack as="section" gap={1} aria-label="Two-step verification">
            <Text variant="label">Two-step verification</Text>
            <QueryState
              error={credentials.code}
              refusal={credentials.refusal}
              detail={credentials.error?.detail || undefined}
              loading={credentials.loading}
            />
          </Stack>
        ) : (
          <>
            <SecurityLine
              label="Two-step verification"
              action={
                <Button size="small" variant="secondary" onClick={() => setDialog("factor")}>
                  {app ? "Replace app…" : "Set up…"}
                </Button>
              }
            >
              {app ? "On." : "Off. Password only."}
            </SecurityLine>
            {/* ONLY BESIDE AN APP: held alone, recovery codes are a second
                factor of their own, and the engine refuses to issue them. */}
            {app && (
              <SecurityLine
                label="Recovery codes"
                action={
                  <Button size="small" variant="secondary" onClick={() => setDialog("codes")}>
                    Create new codes…
                  </Button>
                }
              >
                {codes
                  ? "For when you don’t have your app."
                  : "None yet. Create a set for when you don’t have your app."}
              </SecurityLine>
            )}
          </>
        )}
        <SecurityLine
          label="Password"
          action={
            <Button size="small" variant="secondary" onClick={() => setDialog("password")}>
              Change password…
            </Button>
          }
        >
          Signs you out elsewhere and stops your tokens.
        </SecurityLine>
      </Stack>
      {dialog === "password" && (
        <LayerBoundary title="Change password" onClose={() => setDialog(null)}>
          <ChangePasswordDialog
            holdsFactor={holdsFactor}
            onChanged={onChanged}
            onClose={() => setDialog(null)}
          />
        </LayerBoundary>
      )}
      {dialog === "factor" && (
        <LayerBoundary title="Two-step verification" onClose={close}>
          <AuthenticatorDialog codes={codes} onClose={close} />
        </LayerBoundary>
      )}
      {dialog === "codes" && (
        <LayerBoundary title="Recovery codes" onClose={close}>
          <RecoveryCodesDialog held={codes} onClose={close} />
        </LayerBoundary>
      )}
    </Card>
  );
}

/**
 * One thing a person proves themselves with: its name, what is true of it now
 * in a line, and the gesture that changes it — beside the line where there is
 * room, under it on a phone.
 */
function SecurityLine({
  label,
  action,
  children,
}: {
  label: string;
  action?: ReactNode;
  /** What is true of it now: one sentence. */
  children: string;
}) {
  return (
    <Inline as="section" gap={3} justify="between" wrap aria-label={label}>
      <Stack gap={1}>
        <Text variant="label">{label}</Text>
        <Text as="p" variant="body" tone="secondary">
          {children}
        </Text>
      </Stack>
      {action}
    </Inline>
  );
}

/** What changing the password came to, while this browser stays signed in. */
type Changed = { kind: "kept" } | { kind: "refused"; text: string };

/**
 * Change your own password, in the dialog the Security card's "Change
 * password…" opens: the current one, a code where a second factor is held,
 * and the new one twice. Those ARE the proof — a step-up's, taken in the one
 * request — so no step-up is asked.
 *
 * # The code is asked of whoever holds a second factor
 *
 * The change ends every other session the person holds, so on the password
 * alone somebody holding their cookie and their password, but not their
 * authenticator, could sign them out everywhere and lock them out. The engine
 * asks a code of anybody who holds one, as a step-up does; the field is drawn
 * when the credential list says one is held, and — where that list is unknown
 * or behind — once the engine has answered `second_factor_required`.
 */
function ChangePasswordDialog({
  holdsFactor,
  onChanged,
  onClose,
}: {
  /** Whether this person holds a second factor, or null while unknown. */
  holdsFactor: boolean | null;
  onChanged: () => void;
  onClose: () => void;
}) {
  const toast = useToast();
  const waiting = useWaiting();
  const config = useRest("/auth/config", (signal) => auth.config(signal));
  const floor = config.data?.min_password_length ?? null;
  const [current, setCurrent] = useState("");
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Changed | null>(null);
  // THE ENGINE ASKED FOR A CODE: the field stays drawn whatever the list
  // said, because the engine is what decides whether one is held.
  const [asked, setAsked] = useState(false);
  const needsCode = holdsFactor === true || asked;

  async function submit() {
    setTried(true);
    if (
      busy ||
      current === "" ||
      (needsCode && code.trim() === "") ||
      !newPasswordReady(password, confirm, floor)
    ) {
      return;
    }
    const sent = needsCode ? code.trim() : "";
    setBusy(true);
    setOutcome(null);
    try {
      const answer = await auth.changePassword({
        current_password: current,
        new_password: password,
        ...(sent ? { code: sent } : {}),
      });
      setCurrent("");
      setCode("");
      setPassword("");
      setConfirm("");
      setTried(false);
      if (answer.status === "password_changed") {
        // THIS BROWSER'S SESSION ENDED WITH THE REST and its cookie is
        // cleared: nothing here is read again — every read would be refused
        // and send the tab to sign in with nothing said — so it goes now,
        // saying why.
        toast.show({
          variant: "success",
          title: "Password changed",
          message: "Sign in again.",
        });
        goSignIn();
        return;
      }
      setOutcome({ kind: "kept" });
      onChanged();
    } catch (err) {
      // A CODE IS SINGLE-USE, and one the engine refused is no use again:
      // cleared, as the step-up dialog clears it — and the form is not
      // "tried" again until it is sent again, or the emptied field would
      // say it is missing under the refusal that emptied it.
      setCode("");
      setTried(false);
      setOutcome({ kind: "refused", text: changeRefusal(err, sent !== "") });
      if (err instanceof RestError && err.code === "second_factor_required") setAsked(true);
    } finally {
      setBusy(false);
    }
  }

  const kept = outcome?.kind === "kept";
  return (
    <Modal
      open
      size="md"
      title="Change password"
      icon={<KeyGlyph />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason={waiting.reason}
      stackBody
      onSubmit={() => void submit()}
      footer={
        kept ? (
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" disabled={busy}>
              {busy ? "Changing" : "Change password"}
            </Button>
          </>
        )
      }
    >
      {kept ? (
        <Callout variant="success" role="status">
          Password changed. Signed out everywhere else.
        </Callout>
      ) : (
        <>
          <CurrentPassword
            value={current}
            onChange={setCurrent}
            missing={tried && current === ""}
          />
          {needsCode && (
            <SecondFactorCode
              value={code}
              onChange={setCode}
              missing={tried && code.trim() === ""}
            />
          )}
          <NewPasswordFields
            floor={floor}
            password={password}
            confirm={confirm}
            onPassword={setPassword}
            onConfirm={setConfirm}
            tried={tried}
          />
          {outcome?.kind === "refused" && (
            <Callout variant="danger" role="alert">
              {outcome.text}
            </Callout>
          )}
        </>
      )}
    </Modal>
  );
}

/**
 * What a refused change says, `coded` where it presented a code.
 *
 * THE STEP-UP DIALOG'S WORDS for a refusal that may be the code: the engine
 * answers a wrong code with the sign-in's one refusal, worded for a form this
 * person is not on, and names `current_password` only where it was the
 * password — which it then says itself.
 */
function changeRefusal(err: unknown, coded: boolean): string {
  if (err instanceof RestError) {
    if (err.code === "second_factor_required") {
      return "Enter a code from your app or a recovery code too.";
    }
    if (err.code === "sign_in_refused" && coded && err.body.field !== "current_password") {
      return "Wrong password or code. Try again.";
    }
    // NOBODY KNOWS whether a change nothing confirmed landed: the next
    // sign-in with the new password is what says.
    if (err.unanswered || err.status === 503) {
      return `${refusalText(err)} Couldn’t confirm the change. Try your new password.`;
    }
  }
  return refusalText(err);
}

/** The second factor's code: asked beside the password of whoever holds one. */
function SecondFactorCode({
  value,
  onChange,
  missing,
}: {
  value: string;
  onChange: (next: string) => void;
  missing: boolean;
}) {
  return (
    <FormField
      label="Code"
      helper="The current code from your app, or a recovery code."
      error={missing ? "Enter a code from your app or a recovery code." : undefined}
    >
      {(field) => (
        <Input
          id={field.id}
          aria-describedby={field.describedBy}
          aria-invalid={field.invalid || undefined}
          autoComplete="one-time-code"
          spellCheck={false}
          width="full"
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </FormField>
  );
}

/** The current password: the proof the change takes. */
function CurrentPassword({
  value,
  onChange,
  missing,
}: {
  value: string;
  onChange: (next: string) => void;
  missing: boolean;
}) {
  return (
    <FormField
      label="Current password"
      error={missing ? "Enter your current password." : undefined}
    >
      {(field) => (
        <Input
          id={field.id}
          aria-describedby={field.describedBy}
          aria-invalid={field.invalid || undefined}
          type="password"
          autoComplete="current-password"
          width="full"
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </FormField>
  );
}

/**
 * Where the reader is signed in: the live sessions, this browser marked.
 *
 * "SIGN OUT EVERYWHERE" STANDS BESIDE THE SENTENCE SAYING WHAT IT ENDS, in the
 * card's body rather than its header: in the header it took the title's width,
 * and on a phone the title read "Where you are sign…".
 */
function Sessions({
  session,
  sessions,
  onChanged,
}: {
  session: SessionAnswer;
  sessions: RestResult<SessionRow[]>;
  onChanged: () => void;
}) {
  const now = useNow();
  const toast = useToast();
  const [ending, setEnding] = useState<string | null>(null);
  const [everywhere, setEverywhere] = useState(false);
  const live = (sessions.data ?? []).filter((s) => s.live);

  async function end(lineage: string) {
    setEnding(lineage);
    try {
      await auth.logoutOne(lineage);
    } catch (err) {
      toast.failed(`Signing that session out didn’t go through. ${refusalText(err)}`);
    } finally {
      setEnding(null);
      onChanged();
    }
  }

  return (
    <Card padding="none">
      <Card.Header
        icon={<MonitorGlyph size="sm" />}
        count={sessions.data ? live.length : undefined}
      >
        <Card.Title>Where you are signed in</Card.Title>
      </Card.Header>
      <Card.Body padding="sm">
        <Inline gap={3} justify="between" wrap>
          <Text as="p" variant="caption">
            Signs you out on every device and stops your tokens.
          </Text>
          <Button size="small" variant="secondary" onClick={() => setEverywhere(true)}>
            Sign out everywhere
          </Button>
        </Inline>
      </Card.Body>
      <QueryState
        error={sessions.code}
        refusal={sessions.refusal}
        detail={sessions.error?.detail || undefined}
        loading={sessions.loading && !sessions.data}
      >
        <DataGrid<SessionRow>
          rows={live}
          rowKey={(s) => s.lineage}
          defaultSort="started"
          empty={{ title: "Signed in nowhere" }}
          columns={[
            {
              key: "started",
              align: "right",
              header: "Signed in",
              sortValue: (s) => s.created_at ?? "",
              cell: (s) => <TableDateCell value={s.created_at} now={now} />,
            },
            {
              key: "ends",
              align: "right",
              header: "Ends",
              shrink: true,
              sortValue: (s) => s.expires_at ?? "",
              cell: (s) => <TableDateCell value={s.expires_at} now={now} />,
            },
            {
              key: "end",
              header: "",
              action: true,
              shrink: true,
              cell: (s) =>
                s.lineage === session.lineage ? (
                  <Tag size="sm" variant="success">
                    This browser
                  </Tag>
                ) : (
                  <Button
                    size="small"
                    variant="ghost"
                    disabled={ending === s.lineage}
                    // A TIME A PERSON READS, in the zone and format the page
                    // draws every other in: it was the raw ISO instant, so a
                    // screen reader spelled out "2026-10-06T06:48:15.219Z".
                    aria-label={`Sign out the session started ${fmtDateTime(s.created_at)}`}
                    onClick={() => void end(s.lineage)}
                  >
                    {ending === s.lineage ? "Signing out" : "Sign out"}
                  </Button>
                ),
            },
          ]}
        />
      </QueryState>
      {everywhere && <SignOutEverywhereDialog onClose={() => setEverywhere(false)} />}
    </Card>
  );
}

/** The personal access tokens that act as the reader: minted, listed, revoked. */
function Tokens({
  owner,
  held,
  credentials,
  onChanged,
}: {
  owner: { id: string; login: string; grants: string[] | null };
  held: readonly string[];
  credentials: RestResult<CredentialRow[]>;
  onChanged: () => void;
}) {
  const now = useNow();
  const [minting, setMinting] = useState(false);
  const [revoking, setRevoking] = useState<CredentialRow | null>(null);
  // ONLY WORKING TOKENS UNLESS ASKED: revoked and expired rows stay in the
  // directory for good, and listed by default they buried the live ones.
  const [all, setAll] = useState(false);
  const tokens = (credentials.data ?? []).filter((c) => c.method === "token");
  const working = tokens.filter((t) => !t.revoked);
  return (
    <Card padding="none">
      <Card.Header
        icon={<KeyGlyph size="sm" />}
        count={credentials.data ? working.length : undefined}
        actions={
          <>
            <Checkbox label="Show revoked and expired" checked={all} onCheckedChange={setAll} />
            <Button size="small" variant="secondary" onClick={() => setMinting(true)}>
              New token
            </Button>
          </>
        }
      >
        <Card.Title>Personal access tokens</Card.Title>
      </Card.Header>
      {/* IN A BODY, at the card's inset: drawn bare under a flush header it
          sat against the header's rule. */}
      <Card.Body padding="sm">
        <Text as="p" variant="caption">
          Tokens let a script or assistant act as you.
        </Text>
      </Card.Body>
      <QueryState
        error={credentials.code}
        refusal={credentials.refusal}
        detail={credentials.error?.detail || undefined}
        loading={credentials.loading && !credentials.data}
      >
        <DataGrid<CredentialRow>
          rows={all ? tokens : working}
          rowKey={(c) => c.id}
          defaultSort="created"
          empty={{ title: "No tokens" }}
          columns={[
            {
              key: "label",
              header: "Token",
              sortValue: (c) => c.label ?? "",
              cell: (c) => <TableTextCell>{c.label || c.id}</TableTextCell>,
            },
            {
              key: "state",
              header: "State",
              shrink: true,
              sortValue: (c) => (c.revoked ? 1 : 0),
              cell: (c) =>
                c.revoked ? (
                  <Tag size="sm" variant="neutral">
                    {endedWord(c, now)}
                  </Tag>
                ) : (
                  <Tag size="sm" variant="success">
                    In use
                  </Tag>
                ),
            },
            {
              key: "created",
              align: "right",
              header: "Created",
              shrink: true,
              sortValue: (c) => c.created_at ?? "",
              cell: (c) => <TableDateCell value={c.created_at} now={now} />,
            },
            {
              key: "expires",
              header: "Expires",
              shrink: true,
              drop: 1,
              sortValue: (c) => c.expires_at ?? "",
              cell: (c) => <ExpiresCell c={c} now={now} />,
            },
            {
              key: "grants",
              header: "Carries",
              drop: 2,
              cell: (c) => <AccessTags grants={c.grants} />,
            },
            {
              key: "revoke",
              header: "",
              action: true,
              shrink: true,
              cell: (c) =>
                c.revoked ? null : (
                  <Button
                    size="small"
                    variant="ghost"
                    aria-label={`Revoke ${c.label || c.id}`}
                    onClick={() => setRevoking(c)}
                  >
                    Revoke
                  </Button>
                ),
            },
          ]}
        />
      </QueryState>
      {minting && (
        <MintTokenDialog
          self
          owner={owner}
          held={held}
          onClose={() => setMinting(false)}
          onDone={onChanged}
        />
      )}
      {revoking && (
        <RevokeToken token={revoking} onClose={() => setRevoking(null)} onDone={onChanged} />
      )}
    </Card>
  );
}

/** Revoke one of your own tokens: anything presenting it is refused from then on. */
function RevokeToken({
  token,
  onClose,
  onDone,
}: {
  token: CredentialRow;
  onClose: () => void;
  onDone: () => void;
}) {
  const write = useIamGesture();
  return (
    <ConfirmDialog
      title="Revoke this token?"
      confirm="Revoke"
      danger
      write={write}
      onClose={onClose}
      onConfirm={async () => {
        const answer = await write.run({
          method: "DELETE",
          path: `/iam/credentials/${pathSegment(token.id)}`,
        });
        if (!answer) return;
        onDone();
        if (answer.kind === "done" && !answer.pending) onClose();
      }}
    >
      Anything using {token.label ? `“${token.label}”` : "it"} stops working.
    </ConfirmDialog>
  );
}
