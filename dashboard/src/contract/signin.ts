/**
 * Whether a second factor is required of a person signing in with a password.
 * COPIES of `iam.SecondFactors`, held by `internal/api`'s
 * `TestTheDashboardReadsExactlyTheSignInPolicyTheEngineSends`.
 */
type SecondFactorRule = "required" | "optional";

/**
 * How a person signs in on the node that answered: `queries.SignInPolicy` in
 * Go, member for member. Every value is the one the engine ENFORCES (a
 * setting the node's file leaves out arrives as its default), and every
 * duration is in whole seconds.
 */
interface SignInPolicy {
  second_factor: SecondFactorRule;
  /** The floor every password is held to. */
  min_password_length: number;
  /** The longest a session lives from sign-in, however it is used. */
  session_absolute_seconds: number;
  /** How long a session lives unused. An engine constant, not a setting. */
  session_idle_seconds: number;
  /** How long a proof of identity authorises a sensitive gesture. */
  step_up_seconds: number;
}

/**
 * The `sign_in_policy` answer, an administrator's: `queries.SignInPolicyAnswer`
 * in Go. TIER A, so it is THIS NODE'S posture and `node` names which node
 * that is; nodes of one fleet may legally disagree, so a screen names the
 * node rather than presenting the reading as the company's. Read only: it is
 * changed in the node's own file and takes a restart.
 */
export interface SignInPolicyAnswer {
  node: string;
  policy: SignInPolicy;
}
