package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// A seat's name in AWS is its handle's first 20 characters and 6 hex of the
// handle's hash: crewlet-<company>-<slug> as its role, crewlet-<company>-seat-
// <slug> as its certificate. For the longest company slug the role name stays
// inside IAM's 64 characters and the certificate name inside Roles Anywhere's
// 61. The hash keeps two handles alike for 20 characters apart, which matters:
// a seat role's trust pins its certificate name, so two seats sharing one
// could each become the other.
const (
	seatHandleChars = 20
	seatHashChars   = 6
)

// SeatSlug is the name a seat with handle carries in AWS.
func SeatSlug(handle string) string {
	h := strings.TrimSpace(handle)
	head := org.Slugify(h)
	if len(head) > seatHandleChars {
		head = strings.TrimRight(head[:seatHandleChars], "-")
	}
	if head == "" {
		head = "agent"
	}
	sum := sha256.Sum256([]byte(h))
	return head + "-" + hex.EncodeToString(sum[:])[:seatHashChars]
}

// SeatRoleName is the role of the seat whose slug is slug.
func (c Company) SeatRoleName(slug string) string { return named("crewlet-{company}-", c) + slug }

// slugOf is the seat slug a seat role's name carries, false for a name under
// the seat path the engine did not make.
func (c Company) slugOf(roleName string) (string, bool) {
	slug, ok := strings.CutPrefix(roleName, named("crewlet-{company}-", c))
	return slug, ok && slug != "" && cnPattern().MatchString(seatCN(c, slug))
}

// Access is what a seat role's managed policies grant.
type Access struct {
	// Level is one of config.AWSLevels, or "" where the attached levels are
	// no level's exact set.
	Level string
	// IAMRead is the company-wide IAM read option.
	IAMRead bool
	// Off is AWS's deny-all policy attached: no session of the role, issued
	// before or after, can do anything.
	Off bool
}

// levelPolicies is the level policies a level attaches: its own and every
// lower one, so a change of level attaches or detaches one policy at a time.
func levelPolicies(level string) []string {
	switch level {
	case config.AWSLevelRead:
		return []string{levelRead}
	case config.AWSLevelOperate:
		return []string{levelRead, levelOperate}
	case config.AWSLevelAdminister:
		return []string{levelRead, levelOperate, levelAdminister}
	}
	return nil
}

// Policies is the managed policies, by ARN, that grant a. An off role holds
// only AWS's deny-all policy, so turning it on again attaches what it grants
// afresh.
func (conn *Connection) Policies(a Access) []string {
	if a.Off {
		return []string{conn.resolve(denyAllARN)}
	}
	var out []string
	for _, name := range levelPolicies(a.Level) {
		out = append(out, conn.resolve(policyARN(name)))
	}
	if a.IAMRead {
		out = append(out, conn.resolve(policyARN(levelIAMRead)))
	}
	return out
}

// AccessOf reads attached, by ARN, as an Access. Foreign reports a policy
// that is none of Crewlet's or AWS's deny-all: a grant made outside Crewlet.
func (conn *Connection) AccessOf(attached []string) (a Access, foreign []string) {
	names := map[string]bool{}
	for _, arn := range attached {
		switch {
		case arn == conn.resolve(denyAllARN):
			a.Off = true
		case arn == conn.resolve(policyARN(levelIAMRead)):
			a.IAMRead = true
		default:
			name, ok := conn.levelName(arn)
			if !ok {
				foreign = append(foreign, arn)
				continue
			}
			names[name] = true
		}
	}
	for _, level := range config.AWSLevels() {
		want := levelPolicies(level)
		if len(want) == len(names) && !slices.ContainsFunc(want, func(n string) bool { return !names[n] }) {
			a.Level = level
		}
	}
	return a, foreign
}

// levelName is the level policy arn names, false for any other.
func (conn *Connection) levelName(arn string) (string, bool) {
	for _, name := range []string{levelRead, levelOperate, levelAdminister} {
		if arn == conn.resolve(policyARN(name)) {
			return name, true
		}
	}
	return "", false
}

// Change is one attach or detach of a managed policy.
type Change struct {
	Attach    bool
	PolicyARN string
}

// Changes is the attaches and detaches that take a role holding have, by
// ARN, to grant want, in the one safe order: every attach first, then every
// detach, AWS's deny-all last of all. So turning off denies before it detaches,
// turning on grants before it lifts the deny, and a level never lapses mid-way.
// A foreign policy is left alone: the engine only reports it.
func (conn *Connection) Changes(have []string, want Access) []Change {
	target := conn.Policies(want)
	var attach, detach []Change
	for _, arn := range target {
		if !slices.Contains(have, arn) {
			attach = append(attach, Change{Attach: true, PolicyARN: arn})
		}
	}
	ours := append(conn.Policies(Access{Level: config.AWSLevelAdminister, IAMRead: true}), conn.resolve(denyAllARN))
	for _, arn := range have {
		if slices.Contains(ours, arn) && !slices.Contains(target, arn) {
			detach = append(detach, Change{PolicyARN: arn})
		}
	}
	slices.SortStableFunc(detach, func(a, b Change) int {
		return boolOrder(a.PolicyARN == conn.resolve(denyAllARN), b.PolicyARN == conn.resolve(denyAllARN))
	})
	return append(attach, detach...)
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// PolicyName is what a person reads for a policy ARN: Crewlet's policy names
// and AWS's deny-all by name.
func (conn *Connection) PolicyName(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// resolve writes the connection's values into a name, path or ARN.
func (conn *Connection) resolve(s string) string { return concrete(s, conn.vars()).(string) }

// SeatRole is a seat's role as IAM holds it.
type SeatRole struct {
	Name, ARN, ID string
	Created       time.Time
	Path          string
	Boundary      string
	// Policies are its attached managed policies, by ARN.
	Policies []string
	// Inline are its inline policies' names: grants the engine never writes
	// and cannot remove.
	Inline []string
	// TrustProblem is why its trust is not the engine's exactly, "" where it is.
	TrustProblem string
}

// ErrInlinePolicy is a seat role holding an inline policy, which the
// provisioner may neither write nor delete, so a person removes it.
var ErrInlinePolicy = errors.New("aws: the role holds an inline policy, which Crewlet never writes and cannot remove")

// SeatRoles is every role under the company's seat path, by name to ARN, all
// pages. A listing decides existence: IAM authorizes GetRole on a missing role
// against an ARN with no path, which the provisioner is not granted, so a
// missing role would read as refused rather than missing.
func (c *Client) SeatRoles(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	pages := iam.NewListRolesPaginator(c.IAM, &iam.ListRolesInput{
		PathPrefix: awssdk.String(c.conn.resolve(seatPath)), MaxItems: awssdk.Int32(1000),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws: list the agents' roles: %w", err)
		}
		for _, r := range page.Roles {
			out[awssdk.ToString(r.RoleName)] = awssdk.ToString(r.Arn)
		}
	}
	return out, nil
}

// ReadSeatRole reads one listed seat role: its identity, boundary, trust and
// every policy it holds.
func (c *Client) ReadSeatRole(ctx context.Context, name string) (SeatRole, error) {
	got, err := c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(name)})
	if err != nil {
		return SeatRole{}, fmt.Errorf("aws: read role %s: %w", name, err)
	}
	r := got.Role
	role := SeatRole{Name: name, ARN: awssdk.ToString(r.Arn), ID: awssdk.ToString(r.RoleId),
		Created: awssdk.ToTime(r.CreateDate), Path: awssdk.ToString(r.Path)}
	if r.PermissionsBoundary != nil {
		role.Boundary = awssdk.ToString(r.PermissionsBoundary.PermissionsBoundaryArn)
	}
	if slug, ok := c.conn.Company.slugOf(name); !ok {
		role.TrustProblem = "its name is not one Crewlet gives an agent's role"
	} else if same, hint := samePolicy(awssdk.ToString(r.AssumeRolePolicyDocument),
		concrete(seatTrustDoc(seatCN(c.conn.Company, slug)), c.conn.vars())); !same {
		role.TrustProblem = hint
	}
	attached := iam.NewListAttachedRolePoliciesPaginator(c.IAM, &iam.ListAttachedRolePoliciesInput{RoleName: awssdk.String(name)})
	for attached.HasMorePages() {
		page, err := attached.NextPage(ctx)
		if err != nil {
			return SeatRole{}, fmt.Errorf("aws: list role %s's policies: %w", name, err)
		}
		for _, p := range page.AttachedPolicies {
			role.Policies = append(role.Policies, awssdk.ToString(p.PolicyArn))
		}
	}
	inline := iam.NewListRolePoliciesPaginator(c.IAM, &iam.ListRolePoliciesInput{RoleName: awssdk.String(name)})
	for inline.HasMorePages() {
		page, err := inline.NextPage(ctx)
		if err != nil {
			return SeatRole{}, fmt.Errorf("aws: list role %s's inline policies: %w", name, err)
		}
		role.Inline = append(role.Inline, page.PolicyNames...)
	}
	return role, nil
}

// ErrRoleExists is a create refused because a role of the seat's name is
// already there. Nothing is adopted by its name: the caller decides from its
// own record whether the role is the one it made.
var ErrRoleExists = errors.New("aws: a role of that name already exists")

// CreateSeatRole creates the role of the seat whose slug is slug: under the
// seat path, within the boundary, trusting only the broker in a session whose
// source identity is the seat's certificate name, and holding no policy yet.
func (c *Client) CreateSeatRole(ctx context.Context, handle, slug string) (SeatRole, error) {
	name := c.conn.Company.SeatRoleName(slug)
	trust := string(compactJSON(concrete(seatTrustDoc(seatCN(c.conn.Company, slug)), c.conn.vars())))
	out, err := c.IAM.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: awssdk.String(name), Path: awssdk.String(c.conn.resolve(seatPath)),
		AssumeRolePolicyDocument: awssdk.String(trust),
		PermissionsBoundary:      awssdk.String(c.conn.resolve(policyARN(boundaryName))),
		MaxSessionDuration:       awssdk.Int32(roleMaxSeconds),
		Description:              awssdk.String("Managed by Crewlet for agent " + strings.TrimSpace(handle) + ". Do not edit."),
		Tags: []iamtypes.Tag{
			{Key: awssdk.String("crewlet:managed-by"), Value: awssdk.String("crewlet")},
			{Key: awssdk.String("crewlet:company"), Value: awssdk.String(string(c.conn.Company))},
			{Key: awssdk.String("crewlet:seat"), Value: awssdk.String(slug)},
		},
	})
	if err == nil {
		// What CreateRole answers is the role as asked, read from the write
		// itself: a read straight after it could miss the role and, IAM
		// authorizing a missing role against an ARN with no path, be refused.
		r := out.Role
		return SeatRole{Name: name, ARN: awssdk.ToString(r.Arn), ID: awssdk.ToString(r.RoleId),
			Created: awssdk.ToTime(r.CreateDate), Path: awssdk.ToString(r.Path),
			Boundary: c.conn.resolve(policyARN(boundaryName))}, nil
	}
	if hasCode(err, "EntityAlreadyExists") {
		return SeatRole{}, fmt.Errorf("%w: %s", ErrRoleExists, name)
	}
	return SeatRole{}, fmt.Errorf("aws: create role %s: %w", name, err)
}

// Apply makes one change to a seat role. Detaching a policy already gone is
// done.
func (c *Client) Apply(ctx context.Context, roleName string, ch Change) error {
	in := awssdk.String(roleName)
	if ch.Attach {
		if _, err := c.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{RoleName: in, PolicyArn: awssdk.String(ch.PolicyARN)}); err != nil {
			return fmt.Errorf("aws: attaching %s to %s: %w", c.conn.PolicyName(ch.PolicyARN), roleName, err)
		}
		return nil
	}
	_, err := c.IAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: in, PolicyArn: awssdk.String(ch.PolicyARN)})
	if err != nil && !hasCode(err, "NoSuchEntity") {
		return fmt.Errorf("aws: detaching %s from %s: %w", c.conn.PolicyName(ch.PolicyARN), roleName, err)
	}
	return nil
}

// DeleteSeatRole detaches every policy the role holds and deletes it, which
// ends every session of it at once. A role holding an inline policy is kept
// ([ErrInlinePolicy]); a role no longer listed is done.
func (c *Client) DeleteSeatRole(ctx context.Context, name string) error {
	listed, err := c.SeatRoles(ctx)
	if err != nil {
		return err
	}
	if _, ok := listed[name]; !ok {
		return nil
	}
	role, err := c.ReadSeatRole(ctx, name)
	if err != nil {
		return err
	}
	if len(role.Inline) > 0 {
		return fmt.Errorf("%w: remove %s from role %s in IAM, then retry", ErrInlinePolicy, strings.Join(role.Inline, ", "), name)
	}
	for _, arn := range role.Policies {
		if err := c.Apply(ctx, name, Change{PolicyARN: arn}); err != nil {
			return err
		}
	}
	if _, err := c.IAM.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: awssdk.String(name)}); err != nil && !hasCode(err, "NoSuchEntity") {
		return fmt.Errorf("aws: delete role %s: %w", name, err)
	}
	return nil
}

// hasCode reports an AWS refusal with code.
func hasCode(err error, code string) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == code
}
