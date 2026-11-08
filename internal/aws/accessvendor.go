package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// AWS's half of per-seat access. The seat's account is its IAM role, recorded
// in the ledger by role ID; its access is the Crewlet level policies attached
// to it; its credential is a session the engine issues at use for 15 minutes,
// never sealed.
//
// DENY-ALL IS WHAT "OFF" MEANS. Off attaches AWS's deny-all policy first,
// which cuts every session of the role, issued before or after, and only then
// detaches the levels, so a later write that fails leaves the role denied.
// The role and its CloudTrail history stay.

func init() { seataccess.Register(accessVendor{}) }

// CredentialsField is the seat's session, as the box's credentials file holds it.
const CredentialsField = "CREDENTIALS"

// accessVendor is AWS's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindAWS }

// Catalog is AWS's per-seat offer: three fixed levels, no scope (the Regions
// are the company's, set in the stack), nothing sealed and nothing to rotate,
// since the session is issued at use for 15 minutes.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind: integration.KindAWS,
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneAWS, Levels: true, Issued: CredentialsField,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
		}},
	}
}

// BoxDefaults is the two files the seat's AWS CLI reads, and no variable: its
// crewlet profile, and the session file the completion poll rewrites as the
// session renews.
func (accessVendor) BoxDefaults(c *config.Company, role *config.Role) seataccess.BoxSet {
	if c == nil || c.Integrations.AWS == nil || c.Integrations.AWS.Provisioning == nil || role == nil {
		return seataccess.BoxSet{}
	}
	return seataccess.BoxSet{Files: []seataccess.BoxFile{
		{Path: BoxConfigPath, Ref: BoxConfig(c.Integrations.AWS.Provisioning.HomeRegion)},
		{Path: BoxCredentialsPath, Ref: "${" + SeatVariable(role.IdentityKey()) + "}", Rewritten: true},
	}}
}

// settleFor is how long the lane's tool waits for IAM to apply a grant: a
// level change reached a live session within about 2 minutes, live.
const settleFor = 2 * time.Minute

// settling marks a grant the step wrote as still applying.
func settling() *seataccess.Settling {
	return &seataccess.Settling{Reason: "AWS is still applying the change", Until: time.Now().Add(settleFor)}
}

// levelInfo is how the Agents tab names a level, and its note.
func levelInfo(level string) (name, note string) {
	switch level {
	case config.AWSLevelRead:
		return "Read", "Reads resource settings, metrics and the contents of CloudWatch Logs, which " +
			"hold whatever your workloads print. It reads no stored objects, items, parameter values or secrets."
	case config.AWSLevelOperate:
		return "Operate", "Read, plus starting, stopping, rebooting and scaling what exists, " +
			"invoking functions, invalidating the CDN, editing alarms and reading and writing S3 objects."
	case config.AWSLevelAdminister:
		return "Administer", "Operate, plus managing those services whole. It can create " +
			"access that outlives the agent's session: instance key pairs, user data on instances, database " +
			"users, and bucket and key policies."
	}
	return level, ""
}

// LevelName is a level's name as the Agents tab shows it.
func LevelName(level string) string {
	name, _ := levelInfo(level)
	return name
}

// leftBehind is the caveat on a step that takes Administer away.
const leftBehind = "Administer could leave access behind on what it reached: instance key pairs, user data, " +
	"database users, and bucket and key policies. Review and remove those."

// Steps is each op as AWS takes it, in the order Apply takes it. Pure.
func (accessVendor) Steps(
	_ seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	losesAdminister := have.Level == config.AWSLevelAdminister && want.Level != config.AWSLevelAdminister
	var out []seataccess.Step
	switch op {
	case seataccess.OpCreate:
		out = []seataccess.Step{
			step(seataccess.VerbCreate, "Create its own IAM role, within the Crewlet boundary and trusting only "+
				"Crewlet's sign-in under its name"),
			step(seataccess.VerbGrant, "Attach the "+LevelName(want.Level)+" policies"),
		}
	case seataccess.OpEnable:
		out = []seataccess.Step{step(seataccess.VerbEnable, "Attach the "+LevelName(want.Level)+
			" policies, then detach AWS's deny-all policy")}
	case seataccess.OpLevel:
		s := step(seataccess.VerbRerole, "Attach and detach its policies so it holds "+LevelName(want.Level))
		if losesAdminister {
			s.Note = leftBehind
		}
		out = []seataccess.Step{s}
	case seataccess.OpDisable:
		off := step(seataccess.VerbDisable, "Attach AWS's deny-all policy, so it stops within about 2 minutes")
		if have.Level == config.AWSLevelAdminister {
			off.Note = leftBehind
		}
		out = []seataccess.Step{off, step(seataccess.VerbRevoke,
			"Detach its level policies. The role and its CloudTrail history stay")}
	case seataccess.OpRemove:
		out = []seataccess.Step{step(seataccess.VerbRemove,
			"Delete its IAM role, which ends its sessions at once. CloudTrail keeps its entries")}
	default:
		// A mint, rotate, revoke or retire has nothing to take: the session
		// is issued at use for 15 minutes and never sealed.
		return nil
	}
	for i := range out {
		out[i].Index = i
	}
	return out
}

// Open builds a session from the company's AWS block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil)
}

// open is [accessVendor.Open] with the transport a test supplies; nil is the
// default.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client) (seataccess.Session, error) {
	client, err := ClientFor(in.Config, in.Env, hc)
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		return nil, err
	case err != nil:
		return nil, &seataccess.Refusal{
			Said: "AWS: " + err.Error(), Actor: seataccess.ByYou,
			Remedy: "Correct the value it names on the AWS connection",
		}
	}
	return &accessSession{client: client, iamRead: in.Config.Integrations.AWS.Provisioning.IAMRead}, nil
}

// ClientFor is the provisioner's client for company, from its document as env
// resolves it. An error wrapping [integration.ErrNotConfigured] is a company
// that does not provision AWS yet: the block absent or off, or its stack
// output or certificate authority not set or not resolving. Any other error
// names the value that is wrong. hc is nil in production.
func ClientFor(c *config.Company, env *config.Resolver, hc *http.Client) (*Client, error) {
	if c == nil || c.Integrations.AWS == nil || !c.Integrations.AWS.Enabled || c.Integrations.AWS.Provisioning == nil {
		return nil, fmt.Errorf("aws: %w: integrations.aws is off or has no provisioning block", integration.ErrNotConfigured)
	}
	if env == nil {
		return nil, fmt.Errorf("aws: %w: nothing resolves the certificate authority", integration.ErrNotConfigured)
	}
	prov := *c.Integrations.AWS.Provisioning
	prov.Connection = strings.TrimSpace(env.Value(prov.Connection))
	prov.CertificateAuthority = strings.TrimSpace(env.Value(prov.CertificateAuthority))
	if prov.CertificateAuthority == "" {
		return nil, fmt.Errorf("aws: %w: integrations.aws.provisioning.certificate_authority is not set or did not "+
			"resolve", integration.ErrNotConfigured)
	}
	conn, err := NewConnection(&prov)
	if errors.Is(err, ErrNotConnected) {
		return nil, fmt.Errorf("%w: %w", integration.ErrNotConfigured, err)
	}
	if err != nil {
		return nil, err
	}
	return NewClient(conn, hc), nil
}

// accessSession is one pass's connection to IAM as the provisioner.
type accessSession struct {
	client *Client
	// iamRead is the company-wide IAM read option every seat that is on holds.
	iamRead bool
	// await bounds the wait for a role IAM has just made to accept writes;
	// tests shorten it.
	await time.Duration
}

// Options is AWS's three levels, least access first. Administer is the one
// that administers, so it takes a fresh sign-in to grant.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: accessVendor{}.FixedLevels(lane), ActionURL: s.rolesURL()}, nil
}

// FixedLevels is the three levels, least access first ([seataccess.FixedLevels]).
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	var levels []seataccess.Level
	for _, id := range config.AWSLevels() {
		name, note := levelInfo(id)
		levels = append(levels, seataccess.Level{ID: id, Name: name, Note: note,
			Administers: id == config.AWSLevelAdminister})
	}
	return levels
}

// Observe lists the seat roles once, then reads each seat's role where the
// ledger records it under that name and ID. A role the ledger does not record
// is not the seat's: nothing is adopted by its name.
func (s *accessSession) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	listed, err := s.client.SeatRoles(ctx)
	if err != nil {
		return nil, s.refused(err)
	}
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	for _, handle := range in.Handles() {
		rec := in.Record(handle)
		name := s.client.conn.Company.SeatRoleName(SeatSlug(handle))
		if _, ok := listed[name]; !ok || rec == nil || !rec.Held() {
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneAWS: {}}
			continue
		}
		observed, err := s.observe(ctx, *rec, name)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			faults[handle] = err
			continue
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneAWS: observed}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observe is one recorded seat role as the shared plan reads it: absent where
// the role of its name is not the recorded one, off where AWS's deny-all is
// attached, and its level only where its policies are exactly one level's and
// the company's IAM read option, with nothing foreign beside them.
func (s *accessSession) observe(ctx context.Context, rec seataccess.Record, name string) (seataccess.Observed, error) {
	role, err := s.client.ReadSeatRole(ctx, name)
	if err != nil {
		return seataccess.Observed{}, s.refused(err)
	}
	if rec.Account == nil || role.ID != rec.Account.ID {
		return seataccess.Observed{}, nil
	}
	access, foreign := s.client.conn.AccessOf(role.Policies)
	out := seataccess.Observed{
		Exists: true, Enabled: !access.Off, Account: role.Name, AccountID: role.ID, CreatedAt: rec.Account.CreatedAt,
	}
	if access.Off {
		out.DisabledBy = seataccess.DisabledByOf(false, rec, true)
		return out, nil
	}
	if len(foreign) == 0 && len(role.Inline) == 0 && role.TrustProblem == "" && access.IAMRead == s.iamRead {
		out.Level = access.Level
	}
	return out, nil
}

// Apply takes one step in IAM, chosen by its op and its place in the op.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch in.Step.Op {
	case seataccess.OpCreate:
		if in.Step.Index == 0 {
			return s.create(ctx, in)
		}
		return s.granted(s.converge(ctx, in, s.want(in.Want), true))
	case seataccess.OpEnable, seataccess.OpLevel:
		return s.granted(s.converge(ctx, in, s.want(in.Want), false))
	case seataccess.OpDisable:
		if in.Step.Index == 0 {
			name, err := s.roleOf(in)
			if err != nil {
				return seataccess.StepResult{}, err
			}
			deny := Change{Attach: true, PolicyARN: s.client.conn.resolve(denyAllARN)}
			return seataccess.StepResult{Disabled: true}, s.refused(s.client.Apply(ctx, name, deny))
		}
		return seataccess.StepResult{}, s.converge(ctx, in, Access{Off: true}, false)
	case seataccess.OpRemove:
		return s.remove(ctx, in)
	default:
		return seataccess.StepResult{}, fmt.Errorf("aws: no step for %q", in.Step.Op)
	}
}

// granted is a grant step's result: settling while IAM applies it.
func (s *accessSession) granted(err error) (seataccess.StepResult, error) {
	if err != nil {
		return seataccess.StepResult{}, err
	}
	return seataccess.StepResult{Settling: settling()}, nil
}

// want is what a seat that is on holds: its level and the company's IAM read
// option.
func (s *accessSession) want(d seataccess.Desired) Access {
	return Access{Level: d.Level, IAMRead: s.iamRead}
}

// create makes the seat's role. A role of its name the ledger records is
// still settling at IAM and is retried; any other is refused, never adopted.
func (s *accessSession) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	slug := SeatSlug(in.Seat.Handle)
	role, err := s.client.CreateSeatRole(ctx, in.Seat.Handle, slug)
	switch {
	case errors.Is(err, ErrRoleExists) && in.Record != nil && in.Record.Account != nil &&
		in.Record.Account.Name == s.client.conn.Company.SeatRoleName(slug):
		return seataccess.StepResult{}, fmt.Errorf("aws: %s's role is still being deleted or listed at IAM: %w",
			in.Seat.Handle, err)
	case errors.Is(err, ErrRoleExists):
		name := s.client.conn.Company.SeatRoleName(slug)
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said:   "AWS: a role " + name + " already exists, and Crewlet has no record of making it",
			Remedy: "Nothing is adopted by its name. Delete it in IAM if nobody uses it, then retry",
			Actor:  seataccess.ByYou, ActionURL: s.roleURL(name),
		}
	case err != nil:
		return seataccess.StepResult{}, s.refused(err)
	}
	return seataccess.StepResult{AccountID: role.ID, Account: role.Name}, nil
}

// converge makes the seat's role hold want, attaching before detaching and
// lifting the deny-all last ([Connection.Changes]). A role that has just been
// made may not accept a write yet, which is waited out; a role holding what
// the provisioner may not change is the person's to fix.
func (s *accessSession) converge(ctx context.Context, in seataccess.ApplyInput, want Access, fresh bool) error {
	name, err := s.roleOf(in)
	if err != nil {
		return err
	}
	var have []string
	if !fresh {
		role, err := s.client.ReadSeatRole(ctx, name)
		if err != nil {
			return s.refused(err)
		}
		if err := s.foreign(role); err != nil {
			return err
		}
		have = role.Policies
	}
	for _, ch := range s.client.conn.Changes(have, want) {
		err := s.client.Apply(ctx, name, ch)
		for wait := time.Duration(0); fresh && hasCode(err, "NoSuchEntity") && wait < s.awaitBound(); wait += pollEvery {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollEvery):
			}
			err = s.client.Apply(ctx, name, ch)
		}
		if err != nil {
			return s.refused(err)
		}
	}
	return nil
}

// pollEvery spaces the reads that wait out IAM's eventual consistency.
const pollEvery = 5 * time.Second

func (s *accessSession) awaitBound() time.Duration {
	if s.await > 0 {
		return s.await
	}
	return 2 * time.Minute
}

// foreign refuses a role holding a grant the engine never makes and the
// provisioner cannot remove: an inline policy, a managed policy that is not
// Crewlet's, or a trust that is not the engine's.
func (s *accessSession) foreign(role SeatRole) *seataccess.Refusal {
	_, foreign := s.client.conn.AccessOf(role.Policies)
	var held []string
	for _, arn := range foreign {
		held = append(held, "the policy "+s.client.conn.PolicyName(arn))
	}
	for _, name := range role.Inline {
		held = append(held, "the inline policy "+name)
	}
	switch {
	case role.TrustProblem != "":
		return &seataccess.Refusal{
			Said:   "AWS: the trust policy of " + role.Name + " is not the one Crewlet wrote (" + role.TrustProblem + ")",
			Remedy: "Turn this agent off and remove it, so Crewlet makes its role again, or restore the trust in IAM",
			Actor:  seataccess.ByYou, ActionURL: s.roleURL(role.Name),
		}
	case len(held) > 0:
		return &seataccess.Refusal{
			Said:   "AWS: " + role.Name + " also holds " + words.And(held) + ", which no level grants",
			Remedy: "Remove them from the role in IAM; Crewlet's provisioner may only attach and detach its own levels",
			Actor:  seataccess.ByYou, ActionURL: s.roleURL(role.Name),
		}
	}
	return nil
}

// remove deletes the seat's role, which ends its sessions at once. A role
// already gone is the state asked for.
func (s *accessSession) remove(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	name, err := s.roleOf(in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	err = s.client.DeleteSeatRole(ctx, name)
	if errors.Is(err, ErrInlinePolicy) {
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said: "AWS: " + err.Error(), Remedy: "Remove the inline policy from the role in IAM, then retry",
			Actor: seataccess.ByYou, ActionURL: s.roleURL(name),
		}
	}
	if err != nil {
		return seataccess.StepResult{}, s.refused(err)
	}
	return seataccess.StepResult{Gone: true}, nil
}

// roleOf is the step's role name: the ledger's, else the one an earlier step
// of the op created, else the one the pass observed.
func (s *accessSession) roleOf(in seataccess.ApplyInput) (string, error) {
	if in.Record != nil && in.Record.Account != nil && in.Record.Account.Name != "" {
		return in.Record.Account.Name, nil
	}
	for _, p := range in.Prior {
		if p.Account != "" {
			return p.Account, nil
		}
	}
	if in.Have.Account != "" {
		return in.Have.Account, nil
	}
	return "", fmt.Errorf("aws: %s has no role to %s", in.Seat.Handle, in.Step.Verb)
}

// refused turns an IAM refusal into the shared [seataccess.Refusal], with the
// remedy and who acts; throttling, a 5xx and a transport failure stay a fault
// the sync retries.
func (s *accessSession) refused(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*seataccess.Refusal](err); ok {
		return err
	}
	status := Status(err)
	switch {
	case status == http.StatusForbidden || hasCode(err, "AccessDenied") || hasCode(err, "AccessDeniedException"):
		return &seataccess.Refusal{
			Said: "AWS: Crewlet's provisioner was refused: " + err.Error(),
			Remedy: "Run the CloudShell block on the AWS connection again, so the stack is the one this engine " +
				"expects, then press Test",
			Actor: seataccess.ByYou,
		}
	case hasCode(err, "LimitExceeded"):
		return &seataccess.Refusal{
			Said:   "AWS: " + err.Error(),
			Remedy: "The account holds as many IAM roles as its quota allows. Delete roles nobody uses, or ask AWS to raise it, then retry",
			Actor:  seataccess.ByYou, ActionURL: "https://console.aws.amazon.com/servicequotas/home/services/iam/quotas",
		}
	case status >= 400 && status < 500 && status != http.StatusTooManyRequests && !hasCode(err, "Throttling"):
		return &seataccess.Refusal{Said: "AWS: " + err.Error(), Remedy: "Settle what AWS names in IAM, then retry",
			Actor: seataccess.ByYou, ActionURL: s.rolesURL()}
	}
	return err
}

// rolesURL is the IAM console; roleURL one seat role's page, through the
// console's ARN resolver.
func (s *accessSession) rolesURL() string {
	return consoleURL(s.client.conn.Partition) + "/iam/home"
}

func (s *accessSession) roleURL(name string) string {
	arn := s.client.conn.resolve("arn:{partition}:iam::{account}:role"+seatPath) + name
	return consoleURL(s.client.conn.Partition) + "/go/view?arn=" + url.QueryEscape(arn)
}

// consoleURL is the AWS console of a partition.
func consoleURL(partition string) string {
	switch partition {
	case "aws-us-gov":
		return "https://console.amazonaws-us-gov.com"
	case "aws-cn":
		return "https://console.amazonaws.cn"
	}
	return "https://console.aws.amazon.com"
}

// Status is the HTTP status of an AWS refusal: Roles Anywhere's, or one the
// SDK read. 0 for any other error, a transport failure among them.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	var resp interface{ HTTPStatusCode() int }
	if errors.As(err, &resp) {
		return resp.HTTPStatusCode()
	}
	return 0
}
