package metaads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// The token slots, least privileged first: a call goes through the first
// whose permissions cover its operation's, among those its class may use.
const (
	slotRead    = "read"
	slotOperate = "operate"
	slotAdmin   = "admin"
	// slotOwn is a seat's own system user's one token, which signs every
	// call of that seat (package doc, "Seat identities").
	slotOwn = "own"
)

// slot is one token position of the block.
type slot struct {
	name, field string
	ref         func(*config.MetaAds) string
}

var slots = []slot{
	{slotRead, "read_token", func(b *config.MetaAds) string { return b.ReadToken }},
	{slotOperate, "operate_token", func(b *config.MetaAds) string { return b.OperateToken }},
	{slotAdmin, "admin_token", func(b *config.MetaAds) string { return b.AdminToken }},
}

// slotsFor are the slots a call of class c may go through. The admin token
// makes an admin act an administrator allowed and nothing else: never a read
// or a change made at once.
func slotsFor(c growth.Class) []string {
	switch c {
	case growth.ClassRead:
		return []string{slotRead, slotOperate}
	case growth.ClassAdmin:
		return []string{slotOperate, slotAdmin}
	}
	return []string{slotOperate}
}

// readOnly are the permissions that write nothing: the read token holds these
// alone, so the read path cannot change anything.
var readOnly = []string{"ads_read", "pages_show_list", "pages_read_engagement", "pages_read_user_content",
	"instagram_basic", "instagram_branded_content_ads_brand", "leads_retrieval", "read_audience_network_insights",
	"public_profile", "read_insights", "attribution_read", "instagram_manage_insights"}

// token is one slot's token as Meta's debug_token answered it.
type token struct {
	slot string
	// level is an own token's seat's level, which its permissions are held
	// to; "" for a company slot.
	level growth.Level
	// pasted is the document's token, from its digest; value is the token in
	// use: its renewal while the renewal descends from it.
	pasted, from string

	mu      sync.Mutex
	value   string
	scopes  []string
	user    string
	expires time.Time
	checked time.Time
	refused error
}

// label is t as a refusal names it: "the read token", "the agent's own token".
func (t *token) label() string {
	if t.slot == slotOwn {
		return "the agent's own token"
	}
	return "the " + t.slot + " token"
}

// signed is how the access log names who t signs as: the company's slot and
// SignerCompany, or no slot and SignerOwn for a seat's own token (ADR-0179).
func (t *token) signed() (via string, signer growth.Signer) {
	if t.slot == slotOwn {
		return "", growth.SignerOwn
	}
	return t.slot, growth.SignerCompany
}

// refusal is why t was last refused, nil for a token in use.
func (t *token) refusal() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refused
}

// checkTTL is how long a token's debug_token answer is reused: permissions
// change only when a person generates a token, and the pass reads it anew.
const checkTTL = 10 * time.Minute

// ErrTokenRefused is a token Meta or the engine refuses to call with.
var ErrTokenRefused = errors.New("metaads: the token is refused")

// holds reports whether scopes cover need: ads_management reads as well.
func holds(scopes, need []string) bool {
	for _, n := range need {
		if !slices.Contains(scopes, n) && (n != "ads_read" || !slices.Contains(scopes, "ads_management")) {
			return false
		}
	}
	return true
}

// missing are need's permissions scopes lacks.
func missing(scopes, need []string) []string {
	var out []string
	for _, n := range need {
		if !holds(scopes, []string{n}) {
			out = append(out, n)
		}
	}
	return out
}

// policy is why a slot's permissions are refused, "" where they are not: the
// read token writes nothing, the operate token holds ads_management and
// never business_management, the admin token holds business_management, and
// none holds a permission no operation needs.
func policy(slotName string, scopes []string) string {
	known := func(p string) bool {
		return slices.Contains(readOnly, p) || slices.ContainsFunc(permissions, func(k growth.Permission) bool { return k.ID == p })
	}
	var wide []string
	for _, s := range scopes {
		if !known(s) {
			wide = append(wide, s)
		}
	}
	switch {
	case len(wide) > 0:
		return fmt.Sprintf("it holds %s, which no Meta Ads operation needs; generate a token without it", words.And(wide))
	case slotName == slotRead && slices.ContainsFunc(scopes, func(s string) bool { return !slices.Contains(readOnly, s) }):
		var writes []string
		for _, s := range scopes {
			if !slices.Contains(readOnly, s) {
				writes = append(writes, s)
			}
		}
		return fmt.Sprintf("the read token holds %s, which writes; generate it with ads_read and read permissions alone",
			words.And(writes))
	case slotName == slotRead && !slices.Contains(scopes, "ads_read"):
		return "the read token lacks ads_read"
	case slotName == slotOperate && !slices.Contains(scopes, "ads_management"):
		return "the operate token lacks ads_management"
	case slotName == slotOperate && slices.Contains(scopes, "business_management"):
		return "the operate token holds business_management, which can change the business portfolio; generate it " +
			"without, and paste one with business_management as the admin token if account changes are wanted"
	case slotName == slotAdmin && !slices.Contains(scopes, "business_management"):
		return "the admin token lacks business_management"
	}
	return ""
}

// debugAnswer is what debug_token says of a token.
type debugAnswer struct {
	Data struct {
		AppID     string   `json:"app_id"`
		Type      string   `json:"type"`
		IsValid   bool     `json:"is_valid"`
		ExpiresAt int64    `json:"expires_at"`
		Scopes    []string `json:"scopes"`
		UserID    string   `json:"user_id"`
		Error     struct {
			Code    int    `json:"code"`
			Subcode int    `json:"subcode"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"data"`
}

// check reads t as debug_token answers it, with the app's own token, and
// holds it to the app and its slot's permissions.
func (p *Product) check(ctx context.Context, c *conn, t *token) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.checked.IsZero() && p.deps.Now().Sub(t.checked) < checkTTL {
		return t.refused
	}
	value, err := p.inUse(ctx, t)
	if err != nil {
		return err
	}
	t.value = value
	c.client.hold(value, t.pasted, c.appID+"|"+c.client.secret)
	resp, err := c.client.Do(ctx, c.appID+"|"+c.client.secret, Request{Method: http.MethodGet, Path: "/debug_token",
		Params: url.Values{"input_token": {value}}, Read: true, Reserved: true})
	if err != nil {
		return err
	}
	var d debugAnswer
	if err := json.Unmarshal(resp.Body, &d); err != nil {
		return fmt.Errorf("metaads: read debug_token's answer for %s: %w", t.label(), err)
	}
	t.checked, t.scopes, t.user, t.expires, t.refused = p.deps.Now(), d.Data.Scopes, d.Data.UserID, time.Time{}, nil
	if d.Data.ExpiresAt > 0 {
		t.expires = time.Unix(d.Data.ExpiresAt, 0).UTC()
	}
	refuse := func(why string) {
		t.refused = fmt.Errorf("%w: %s: %s", ErrTokenRefused, t.label(), why)
	}
	switch {
	case !d.Data.IsValid:
		refuse("Meta no longer accepts it (" + validityWords(d.Data.Error.Subcode) + "); generate a new token and paste it")
	case d.Data.AppID != c.appID:
		refuse("it was issued by another Meta app than app " + c.appID)
	case d.Data.Type == "PAGE" || d.Data.Type == "APP":
		refuse("it is a " + strings.ToLower(d.Data.Type) + " token; generate a system user's token")
	case t.slot == slotOwn:
		if why := seatPolicy(t.level, t.scopes); why != "" {
			refuse(why + "; a person sets up the agent's identity again on the integration's Agents tab")
		}
	default:
		if why := policy(t.slot, t.scopes); why != "" {
			refuse(why)
		}
	}
	return t.refused
}

// validityWords is why Meta refused a token, in words, by its subcode.
func validityWords(subcode int) string {
	switch subcode {
	case 463:
		return "it expired"
	case 458:
		return "the app was removed from the system user"
	case 460, 467:
		return "it was invalidated"
	}
	return "it is not valid"
}

// tokenFor is own where a seat keeps its own token ([Product.ownOf]), which
// signs or refuses its every call; otherwise c's least privileged token whose
// class and permissions cover the call. Engine policy reads pass no own.
func (p *Product) tokenFor(ctx context.Context, c *conn, own *token, class growth.Class, need []string) (*token, error) {
	if own != nil {
		if err := p.check(ctx, c, own); err != nil {
			if errors.Is(err, ErrTokenRefused) {
				return nil, growth.Refuse(growth.RefusedForbidden, err.Error()+". No call was made.")
			}
			return nil, err
		}
		if !holds(own.scopes, need) {
			return nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("the agent's own system user's token lacks "+
				"%s, which this call needs and its level does not give. No call was made.", words.And(missing(own.scopes, need))))
		}
		return own, nil
	}
	var tried []string
	for _, name := range slotsFor(class) {
		t := c.tokens[name]
		if t == nil {
			continue
		}
		if err := p.check(ctx, c, t); err != nil {
			if errors.Is(err, ErrTokenRefused) {
				tried = append(tried, err.Error())
				continue
			}
			return nil, err
		}
		if holds(t.scopes, need) {
			return t, nil
		}
		tried = append(tried, fmt.Sprintf("the %s token lacks %s", name, words.And(missing(t.scopes, need))))
	}
	if len(tried) == 0 {
		if class == growth.ClassRead {
			return nil, growth.Refuse(growth.RefusedUnavailable, "no token reads Meta Ads. No call was made.")
		}
		return nil, growth.Refuse(growth.RefusedForbidden, "Meta Ads holds no operate token, so agents only read. A "+
			"person pastes the system user's token with ads_management on the integration's page. No call was made.")
	}
	why := strings.Join(tried, "; ")
	if class != growth.ClassAdmin && slices.Contains(need, "business_management") {
		why += ". business_management is held by the admin token alone, which makes account changes an " +
			"administrator allows and nothing else: a person reads it in Business Settings"
	}
	return nil, growth.Refuse(growth.RefusedForbidden, "no connected token may make this call: "+why+". No call was made.")
}

// Renewal is a slot's renewal as the estate keeps it: the token, the digest
// of the pasted token it descends from, when it expires, and the day the last
// renewal was tried.
type Renewal struct {
	Token     string `json:"token"`
	From      string `json:"from"`
	ExpiresAt int64  `json:"expires_at"`
	Tried     string `json:"tried"`
}

// inUse is t's token now: its renewal while that descends from the pasted
// token, the pasted one otherwise. A renewal of another paste is deleted: a
// new paste wins, and a replaced token's renewal never outlives it.
func (p *Product) inUse(ctx context.Context, t *token) (string, error) {
	if p.deps.Renewals == nil || t.slot == slotOwn {
		// AN OWN TOKEN is renewed by the seat's access sync, sealed in place.
		return t.pasted, nil
	}
	raw, _, found, err := p.deps.Renewals.Held(ctx, t.slot)
	if err != nil {
		return "", fmt.Errorf("metaads: read the %s token's renewal: %w", t.slot, err)
	}
	if !found {
		return t.pasted, nil
	}
	var r Renewal
	if json.Unmarshal([]byte(raw), &r) != nil || r.From != t.from {
		if err := p.deps.Renewals.Delete(ctx, t.slot); err != nil {
			return "", fmt.Errorf("metaads: drop the %s token's stale renewal: %w", t.slot, err)
		}
		return t.pasted, nil
	}
	if r.Token == "" {
		return t.pasted, nil
	}
	return r.Token, nil
}

// renewAt is how long before a token's end it is renewed: ten days of sixty,
// which a long outage or a holiday still leaves room in.
const renewAt = 10 * 24 * time.Hour

// renewResult is one slot's renewal outcome for the pass.
type renewResult struct {
	slot    string
	expires time.Time
	failed  string
}

// renew renews t where it ends within [renewAt] and no renewal was tried
// today, under the fleet lease refresh/meta_ads: Meta's fb_exchange_token
// exchange, its secrets in the form body, sealed as t's renewal with the
// pasted token's digest.
func (p *Product) renew(ctx context.Context, c *conn, t *token) (renewResult, error) {
	t.mu.Lock()
	expires, value := t.expires, t.value
	t.mu.Unlock()
	out := renewResult{slot: t.slot, expires: expires}
	now := p.deps.Now().UTC()
	if expires.IsZero() || expires.Sub(now) > renewAt || p.deps.Renewals == nil || p.deps.Leases == nil {
		return out, nil
	}
	release, err := p.deps.Leases.Hold(ctx, "refresh/"+Kind)
	switch {
	case errors.Is(err, growth.ErrActHeld):
		// Another node renews now; the next pass reads its result.
		return out, nil
	case err != nil:
		return out, fmt.Errorf("metaads: hold the %s token's renewal lease, so its renewal was not sent: %w", t.slot,
			err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, p.applyBound)
	defer cancel()
	raw, version, found, err := p.deps.Renewals.Held(ctx, t.slot)
	if err != nil {
		return out, fmt.Errorf("metaads: read the %s token's renewal: %w", t.slot, err)
	}
	var r Renewal
	if found && json.Unmarshal([]byte(raw), &r) == nil && r.From == t.from {
		if r.Tried == now.Format(time.DateOnly) {
			return out, nil
		}
		if r.Token != "" && r.Token != value {
			// ANOTHER NODE RENEWED IT: the next check reads the renewal.
			return out, nil
		}
	} else {
		r, version = Renewal{From: t.from}, 0
		if found {
			if err := p.deps.Renewals.Delete(ctx, t.slot); err != nil {
				return out, err
			}
		}
	}
	r.Tried = now.Format(time.DateOnly)
	fresh, expiresIn, xerr := exchange(ctx, c.client, c.appID, value)
	if xerr == nil {
		c.client.hold(fresh)
		r.Token, r.ExpiresAt = fresh, now.Add(expiresIn).Unix()
		out.expires = time.Unix(r.ExpiresAt, 0).UTC()
	} else {
		out.failed = xerr.Error()
	}
	data, _ := json.Marshal(r)
	if _, err := p.deps.Renewals.Put(ctx, t.slot, string(data), version); err != nil {
		return out, fmt.Errorf("metaads: seal the %s token's renewal: %w", t.slot, err)
	}
	t.mu.Lock()
	t.checked = time.Time{}
	t.mu.Unlock()
	return out, nil
}

// exchange is Meta's fb_exchange_token exchange of value for a token living
// sixty days, every secret in the POST form body.
func exchange(ctx context.Context, client *Client, appID, value string) (string, time.Duration, error) {
	resp, err := client.Do(ctx, appID+"|"+client.secret, Request{Method: http.MethodPost, Path: "/oauth/access_token",
		Params: url.Values{"grant_type": {"fb_exchange_token"}, "client_id": {appID}, "client_secret": {client.secret},
			"fb_exchange_token": {value}, "set_token_expires_in_60_days": {"true"}}, Reserved: true})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return "", 0, errors.New(apiErr.Error())
		}
		return "", 0, err
	}
	var doc struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if json.Unmarshal(resp.Body, &doc) != nil || doc.AccessToken == "" {
		return "", 0, errors.New("metaads: Meta's renewal answer holds no token")
	}
	secs, err := strconv.ParseInt(strings.Trim(string(doc.ExpiresIn), `"`), 10, 64)
	if err != nil || secs <= 0 {
		secs = int64((60 * 24 * time.Hour).Seconds())
	}
	return doc.AccessToken, time.Duration(secs) * time.Second, nil
}

// Forget deletes every slot's renewal: the integration was turned off, so no
// renewal outlives the tokens it descends from.
func (p *Product) Forget(ctx context.Context) error {
	if p.deps.Renewals == nil {
		return nil
	}
	var errs []error
	for _, s := range slots {
		if err := p.deps.Renewals.Delete(ctx, s.name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
