// Package aws is the AWS integration: each agent seat is its own IAM role in
// the company's account, reached for 15 minutes at a time and attributable in
// CloudTrail to that agent alone, at a level the company chooses.
//
// The engine holds no AWS secret. It generates a certificate authority, seals
// the key, and the owner registers only the certificate as an IAM Roles
// Anywhere trust anchor, from one CloudShell paste of a stack the engine
// renders (RenderStack). Its identities sign in through it, each on a profile
// that offers one role and a trust that admits one certificate name:
//
//   - the PROVISIONER (CN crewlet-<company>-provisioner) creates, changes and
//     deletes seat roles only under the company's seat path, only with the
//     seat boundary attached, and attaches only the fixed level policies; it
//     writes no policy document, cannot touch itself or the broker, and can
//     never become a role;
//   - the BROKER (CN crewlet-<company>-seat-*) can only assume seat roles, each
//     of which trusts the broker alone with its source identity pinned to its
//     own certificate name, so CloudTrail's sourceIdentity names the agent.
//
// A third identity is there only where the company asks for it
// (provisioning.atlas_read): the ATLAS (CN crewlet-<company>-atlas), a role of
// nine named listings that the atlas-collect duty signs in as, so CloudTrail
// names the atlas behind an hourly read of the account and never an agent
// (ADR-0148, atlas.go).
//
// A seat's level is one or more fixed customer managed policies; turning a
// seat off attaches AWS's AWSDenyAll, which reaches sessions already issued,
// and removing it deletes the role. The boundary holds what no level may ever
// do. A live probe against a real account proved every call here before it
// was built (docs/integrations/aws.md).
package aws
