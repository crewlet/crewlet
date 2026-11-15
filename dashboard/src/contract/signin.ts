import type { LimitsAnswer } from "./limits.ts";
import type { SettingsAnswer } from "./settings.ts";

/**
 * Whether a second factor is required of a person signing in with a password.
 * COPIES of `iam.SecondFactors`, held by `internal/api`'s
 * `TestTheDashboardReadsExactlyTheSignInPolicyTheEngineSends`.
 */
type SecondFactorRule = "required" | "optional";

/**
 * A sign-in policy in force: `queries.SignInPolicy` in Go, member for member.
 * Every duration is in whole seconds.
 */
interface SignInPolicy {
  second_factor: SecondFactorRule;
  /** The floor every new password is held to. */
  min_password_length: number;
  /** The longest a session lives from sign-in, however it is used. */
  session_absolute_seconds: number;
  /** How long a session lives unused. */
  session_idle_seconds: number;
  /** How long a proof of identity authorises a sensitive change. */
  step_up_seconds: number;
}

/**
 * This node's operator limits on the company's policy, from its own file
 * (`api.auth.limits`): `queries.SignInLimits` in Go, defaults applied.
 */
interface SignInLimits {
  /** `required`: the company may not drop the second factor; `optional`: it may. */
  second_factor: SecondFactorRule;
  /** The shortest password floor the company may set. */
  min_password_length: number;
  max_session_lifetime_seconds: number;
  max_idle_timeout_seconds: number;
  max_step_up_seconds: number;
}

/**
 * The `sign_in_policy` answer: `queries.SignInPolicyAnswer` in Go. The settings
 * are the company's, ranges narrowed to `limits`; `policy` is what `node`
 * enforces, lower only where its operator holds a value there.
 */
export interface SignInPolicyAnswer {
  node: string;
  policy: SignInPolicy;
  limits: SignInLimits;
  settings: LimitsAnswer["limits"];
  second_factor_setting: SettingsAnswer["settings"][number];
}
