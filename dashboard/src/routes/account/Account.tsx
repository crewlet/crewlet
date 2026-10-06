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
 * A login, a name, an address, a seat and grants are the directory's, and a
 * person changing their own would be the escalation the directory exists to
 * close — the engine refuses it. The profile reads them and says who changes
 * them.
 *
 * # Changing the password keeps this browser, and ends everything else
 *
 * `POST /auth/password` takes the current password as its proof — no step-up
 * — moves the person's revocation epoch, which ends every other session and
 * every personal token, and answers a fresh session for this browser. Where
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
 * # A credential that is not a person's sees what it is
 *
 * A Tier A token exchanged for a session is the deployment's credential, with
 * no password, second factor or token of its own — so the page names it and
 * draws none of the personal sections, each of which the engine would refuse.
 */

import { useCallback, useState } from "react";
import {
  Button,
  Callout,
  Card,
  EmptyValue,
  FormField,
  Input,
  Tag,
  Text,
  useToast,
} from "@crewlethq/ui";
import { KeyGlyph, MonitorGlyph, ShieldUserGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import {
  ConfirmDialog,
  endedWord,
  ExpiresCell,
  GrantTags,
  MintTokenDialog,
} from "~/components/people.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, TextCell } from "~/app/frame/cells.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { LayerBoundary } from "~/app/boundaries.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { useIamGesture } from "~/lib/iamWrite.ts";
import { refusalText } from "~/lib/refusal.ts";
import { goSignIn } from "~/lib/session.ts";
import { SignOutEverywhereDialog } from "~/components/SignOutEverywhere.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { auth, rest, RestError, type SessionAnswer } from "~/protocol/index.ts";
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

/** The Tier A token id a session exchanged from one acts under, or "". */
export function tierATokenOf(login: string): string {
  return login.startsWith("token:") ? login.slice("token:".length) : "";
}

export function Account() {
  const session = useRest("/auth/session", (signal) => auth.session(signal), READ);
  const { reload } = session;
  const sessionMoved = useCallback(() => reload({ quiet: true }), [reload]);
  const answer = session.data;
  return (
    <>
      <PageNote>
        Who you are signed in as, and what you can change about it yourself: your password, your
        second factor, where you are signed in and the tokens that act as you.
      </PageNote>
      {answer ? (
        answer.kind === "person" ? (
          <PersonAccount session={answer} onSessionMoved={sessionMoved} />
        ) : (
          <NotAPerson session={answer} />
        )
      ) : (
        <QueryState
          error={session.code}
          refusal={session.refusal}
          detail={session.error?.detail || undefined}
          loading={session.loading}
        />
      )}
    </>
  );
}

/** A session that is not a person's: what it is, and nothing to manage. */
function NotAPerson({ session }: { session: SessionAnswer }) {
  const token = tierATokenOf(session.login);
  return (
    <Card>
      <Card.Header icon={<KeyGlyph size="sm" />}>
        <Card.Title>Not a person&rsquo;s account</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        <Text as="p" variant="body">
          {token ? (
            <>
              You are signed in with this deployment&rsquo;s API token{" "}
              <code className="inline">{token}</code>. It is the deployment&rsquo;s own credential
              rather than a person, so it has no password, second factor, sessions list or personal
              tokens to manage here; its session ends within the hour.
            </>
          ) : (
            <>
              You are signed in as <code className="inline">{session.login}</code>, which is not a
              person, so there is no password, second factor or personal token to manage here.
            </>
          )}
        </Text>
        <div className="row gap-2" style={{ flexWrap: "wrap", alignItems: "center" }}>
          <span className="t-label">Holds</span>
          <GrantTags grants={session.grants} />
        </div>
      </div>
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
  const id = encodeURIComponent(session.person);
  const profile = useRest(
    `/iam/people/${id}`,
    async (signal) => (await rest.get(`/iam/people/${id}`, signal)) as DirectoryRow,
    READ,
  );
  const credentials = useRest(
    "/iam/credentials",
    async (signal) =>
      listOf<CredentialRow>(await rest.get("/iam/credentials", signal), "credentials"),
    READ,
  );
  const sessions = useRest(
    `/iam/people/${id}/sessions`,
    async (signal) =>
      listOf<SessionRow>(await rest.get(`/iam/people/${id}/sessions`, signal), "sessions"),
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
    <div className="col gap-4">
      <Profile session={session} profile={profile} />
      <Security credentials={credentials} onChanged={moved} />
      <Sessions session={session} sessions={sessions} onChanged={moved} />
      <Tokens
        owner={{ id: session.person, login: session.login, grants: profile.data?.grants ?? grants }}
        held={grants}
        credentials={credentials}
        onChanged={moved}
      />
    </div>
  );
}

/** What the directory holds about the reader, read-only. */
function Profile({
  session,
  profile,
}: {
  session: SessionAnswer;
  profile: RestResult<DirectoryRow>;
}) {
  const org = useOrg();
  const row = profile.data;
  const seat = row?.seat ?? session.seat ?? "";
  const seatName = seat ? (indexOrg(org).byHandle.get(seat)?.name ?? seat) : "";
  const sealed = <EmptyValue label="Sealed: this node cannot open it" />;
  return (
    <Card padding="none">
      <Card.Header icon={<UserGlyph size="sm" />}>
        <Card.Title>Profile</Card.Title>
      </Card.Header>
      <QueryState
        error={profile.code}
        refusal={profile.refusal}
        detail={profile.error?.detail || undefined}
        loading={profile.loading && !row}
      >
        <dl className="prof-facts">
          <dt>Login</dt>
          <dd className="mono">{row?.login || session.login}</dd>
          <dt>Name</dt>
          <dd>{row?.sealed ? sealed : row?.name || <EmptyValue label="No name" />}</dd>
          <dt>Email</dt>
          <dd>{row?.sealed ? sealed : row?.email || <EmptyValue label="No address" />}</dd>
          <dt>Kind</dt>
          <dd>Person</dd>
          <dt>Seat</dt>
          <dd>
            {seat ? (
              <a className="t-link" href={href(["agents", "seats", seat])}>
                {seatName} <span className="mono muted">{seat}</span>
              </a>
            ) : (
              <EmptyValue label="Bound to no seat" />
            )}
          </dd>
          <dt>Grants</dt>
          <dd>
            <GrantTags grants={row?.grants ?? session.grants} />
          </dd>
        </dl>
      </QueryState>
      <p
        className="t-caption muted"
        style={{ margin: 0, padding: "0 var(--spacing-4) var(--spacing-4)" }}
      >
        An administrator changes these, from Settings › People &amp; access.
      </p>
    </Card>
  );
}

/** The password, the second factor and the recovery codes. */
function Security({
  credentials,
  onChanged,
}: {
  credentials: RestResult<CredentialRow[]>;
  onChanged: () => void;
}) {
  const [dialog, setDialog] = useState<"factor" | "codes" | null>(null);
  const live = (credentials.data ?? []).filter((c) => !c.revoked);
  const app = live.some((c) => c.method === "totp");
  const codes = live.some((c) => c.method === "recovery");
  const close = () => {
    setDialog(null);
    onChanged();
  };
  return (
    <Card>
      <Card.Header icon={<ShieldUserGlyph size="sm" />}>
        <Card.Title>Security</Card.Title>
      </Card.Header>
      <div className="col gap-4">
        <ChangePassword onChanged={onChanged} />
        <section className="col gap-2" aria-label="Two-step verification">
          <span className="t-label">Two-step verification</span>
          {credentials.data === null ? (
            <QueryState
              error={credentials.code}
              refusal={credentials.refusal}
              detail={credentials.error?.detail || undefined}
              loading={credentials.loading}
            />
          ) : (
            <>
              <Text as="p" variant="body" tone="secondary">
                {app
                  ? "An authenticator app is set up: signing in asks for its code as well as your password."
                  : "No authenticator app is set up: signing in asks for your password alone."}
              </Text>
              <div className="row gap-2" style={{ flexWrap: "wrap" }}>
                <Button size="small" variant="secondary" onClick={() => setDialog("factor")}>
                  {app ? "Replace authenticator…" : "Set up an authenticator…"}
                </Button>
              </div>
              {/* ONLY BESIDE AN APP: held alone, recovery codes are a second
                  factor of their own, and the engine refuses to issue them. */}
              {app && (
                <>
                  <Text as="p" variant="body" tone="secondary">
                    {codes
                      ? "You hold recovery codes: each signs you in once when your app is not to hand. A new set retires the one you hold."
                      : "You hold no recovery codes: issue a set for the day your app is not to hand."}
                  </Text>
                  <div className="row gap-2" style={{ flexWrap: "wrap" }}>
                    <Button size="small" variant="secondary" onClick={() => setDialog("codes")}>
                      New recovery codes…
                    </Button>
                  </div>
                </>
              )}
            </>
          )}
        </section>
      </div>
      {dialog === "factor" && (
        <LayerBoundary title="Two-step verification" onClose={close}>
          <AuthenticatorDialog onClose={close} />
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

/** What changing the password came to, while this browser stays signed in. */
type Changed = { kind: "kept" } | { kind: "refused"; text: string };

/**
 * Change your own password: the current one, and the new one twice. The
 * current one IS the proof, so no step-up is asked.
 */
function ChangePassword({ onChanged }: { onChanged: () => void }) {
  const toast = useToast();
  const config = useRest("/auth/config", (signal) => auth.config(signal));
  const floor = config.data?.min_password_length ?? null;
  const [current, setCurrent] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Changed | null>(null);

  async function submit() {
    setTried(true);
    if (busy || current === "" || !newPasswordReady(password, confirm, floor)) return;
    setBusy(true);
    setOutcome(null);
    try {
      const answer = await auth.changePassword({
        current_password: current,
        new_password: password,
      });
      setCurrent("");
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
          title: "Your password is changed",
          message:
            "Every session you held has ended, this one included: sign in with the new password.",
        });
        goSignIn();
        return;
      }
      setOutcome({ kind: "kept" });
      onChanged();
    } catch (err) {
      // NOBODY KNOWS whether a change nothing confirmed landed: the next
      // sign-in with the new password is what says.
      const unknown = err instanceof RestError && (err.unanswered || err.status === 503);
      setOutcome({
        kind: "refused",
        text: unknown
          ? `${refusalText(err)} It is not known whether your password changed: if the new one signs you in, it did.`
          : refusalText(err),
      });
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      className="signin-form"
      aria-label="Change password"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <span className="t-label">Password</span>
      <CurrentPassword value={current} onChange={setCurrent} missing={tried && current === ""} />
      <NewPasswordFields
        floor={floor}
        password={password}
        confirm={confirm}
        onPassword={setPassword}
        onConfirm={setConfirm}
        tried={tried}
      />
      {outcome?.kind === "kept" && (
        <Callout variant="success" role="status">
          Your password is changed. Every other session and every personal access token you held has
          ended; this browser stays signed in.
        </Callout>
      )}
      {outcome?.kind === "refused" && (
        <Callout variant="danger" role="alert">
          {outcome.text}
        </Callout>
      )}
      <Button type="submit" variant="primary" disabled={busy}>
        {busy ? "Changing" : "Change password"}
      </Button>
    </form>
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
      error={
        missing ? "Type your current password: it is what proves the change is yours." : undefined
      }
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

/** Where the reader is signed in: the live sessions, this browser marked. */
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
      toast.failed(`Signing that session out did not go through. ${refusalText(err)}`);
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
        actions={
          <Button size="small" variant="secondary" onClick={() => setEverywhere(true)}>
            Sign out everywhere
          </Button>
        }
      >
        <Card.Title>Where you are signed in</Card.Title>
      </Card.Header>
      <p
        className="t-caption muted"
        style={{ margin: 0, padding: "0 var(--spacing-4) var(--spacing-3)" }}
      >
        Signing out everywhere ends every session you hold, this one included, and every personal
        access token you minted.
      </p>
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
              header: "Signed in",
              sortValue: (s) => s.created_at ?? "",
              cell: (s) => <DateCell at={s.created_at} now={now} />,
            },
            {
              key: "ends",
              header: "Ends",
              shrink: true,
              sortValue: (s) => s.expires_at ?? "",
              cell: (s) => <DateCell at={s.expires_at} now={now} />,
            },
            {
              key: "end",
              header: "",
              label: "Sign out",
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
                    aria-label={`Sign out the session started ${s.created_at ?? ""}`}
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
  const tokens = (credentials.data ?? []).filter((c) => c.method === "token");
  return (
    <Card padding="none">
      <Card.Header
        icon={<KeyGlyph size="sm" />}
        count={credentials.data ? tokens.filter((t) => !t.revoked).length : undefined}
        actions={
          <Button size="small" variant="secondary" onClick={() => setMinting(true)}>
            New token
          </Button>
        }
      >
        <Card.Title>Personal access tokens</Card.Title>
      </Card.Header>
      <p
        className="t-caption muted"
        style={{ margin: 0, padding: "0 var(--spacing-4) var(--spacing-3)" }}
      >
        A token acts as you — for your assistant or a script — carrying what you chose of your
        grants, never more than you hold now.
      </p>
      <QueryState
        error={credentials.code}
        refusal={credentials.refusal}
        detail={credentials.error?.detail || undefined}
        loading={credentials.loading && !credentials.data}
      >
        <DataGrid<CredentialRow>
          rows={tokens}
          rowKey={(c) => c.id}
          defaultSort="created"
          empty={{ title: "No tokens" }}
          columns={[
            {
              key: "label",
              header: "Token",
              sortValue: (c) => c.label ?? "",
              cell: (c) => <TextCell>{c.label || c.id}</TextCell>,
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
              header: "Created",
              shrink: true,
              sortValue: (c) => c.created_at ?? "",
              cell: (c) => <DateCell at={c.created_at} now={now} />,
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
              cell: (c) => <GrantTags grants={c.grants} />,
            },
            {
              key: "revoke",
              header: "",
              label: "Revoke",
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
          path: `/iam/credentials/${encodeURIComponent(token.id)}`,
        });
        if (!answer) return;
        onDone();
        if (answer.kind === "done" && !answer.pending) onClose();
      }}
    >
      Anything presenting it is refused from now on
      {token.label ? ` — “${token.label}”` : ""}.
    </ConfirmDialog>
  );
}
