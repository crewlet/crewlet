/**
 * `#/enrol?next=` — the second factor a deployment requires before anything
 * else.
 *
 * # Where a browser lands, and why it cannot be anywhere else
 *
 * On a deployment whose `api.auth.local.totp` is `required`, a sign-in that
 * proved a password and no second factor opens a session that may do exactly
 * three things: say who it is, enrol a factor, and re-confirm the password.
 * Every other route answers `403 second_factor_enrolment_required` — the
 * socket's handshake included — so a sign-in that answers that status comes
 * here, and so does a browser that meets the refusal anywhere later (a reload,
 * a session opened in another tab).
 *
 * # Enrolling replaces the session
 *
 * The engine answers the enrolment with a WHOLE session in place of the
 * restricted one, its cookie on the response. So this screen issues the first
 * recovery codes on that session, and only then carries the person on to
 * `next` — re-dialling the socket, which was refused under the old one.
 */

import { useState } from "react";
import { useRoute } from "~/app/router.tsx";
import { useSignedIn, type SignedInAs } from "~/lib/session.ts";
import { FirstRecoveryCodes, SecondFactorSetup } from "./SecondFactor.tsx";
import { SignInPage } from "./SignInPage.tsx";

export function Enrol() {
  const next = useRoute().query.get("next");
  const signedIn = useSignedIn();
  // THE WHOLE SESSION THE ENROLMENT OPENED, which says who it is: the sign-in
  // that led here recorded the same person, and what carries on is decided
  // against it like any other sign-in's answer.
  const [enrolled, setEnrolled] = useState<SignedInAs | null>(null);

  return (
    <SignInPage
      title={enrolled ? "Save your recovery codes" : "Set up two-step verification"}
      lede={
        enrolled
          ? undefined
          : "This company asks everybody who signs in with a password to confirm it with an authenticator app as well. Until you have added one, nothing else opens."
      }
    >
      {enrolled ? (
        <FirstRecoveryCodes onDone={() => signedIn(enrolled, next)} />
      ) : (
        <SecondFactorSetup
          onEnrolled={(answer) => setEnrolled(answer.session ?? { status: "signed_in" })}
        />
      )}
    </SignInPage>
  );
}
