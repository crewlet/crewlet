package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// The reads the atlas makes of the account, under the Read groups the
// provisioner token already holds on every zone (levels.go): the account's
// own name, each zone's DNS records and each zone's certificate packs.

// Account is the account as Cloudflare names it.
type Account struct {
	ID   string
	Name string
}

// Account reads the account the client addresses.
func (c *Client) Account(ctx context.Context) (Account, error) {
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(c.account), nil, nil, &body); err != nil {
		return Account{}, err
	}
	return Account{ID: body.ID, Name: body.Name}, nil
}

// The record types the atlas reads. A, AAAA and CNAME say what a name points
// at; every other type is listed by its type and name alone, and a TXT
// record's content, where verification tokens and signing keys sit, is never
// decoded.
const (
	RecordA     = "A"
	RecordAAAA  = "AAAA"
	RecordCNAME = "CNAME"
)

// DNSRecord is one record of a zone, as the atlas reads it: its id, type and
// name, whether Cloudflare proxies it, and for an A, AAAA or CNAME record what
// it points at. Comments and tags are not read.
type DNSRecord struct {
	ID      string
	Type    string
	Name    string
	Content string
	Proxied bool
}

// dnsPageSize is the rows a page of a zone's records asks for. Cloudflare
// serves far more a page here than on the zone listing; a thousand records
// of about half a kilobyte each is well under the response bound
// ([maxResponse]), so a ten-thousand-record zone is ten calls.
const dnsPageSize = 1000

// DNSRecords reads every record of a zone.
func (c *Client) DNSRecords(ctx context.Context, zoneID string) ([]DNSRecord, error) {
	seen := map[string]bool{}
	var out []DNSRecord
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	err := c.listPages(ctx, path, nil, dnsPageSize, func(raw json.RawMessage) (int, error) {
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0, err
		}
		for _, row := range rows {
			// THE TYPE FIRST, AND THE CONTENT ONLY FOR THE THREE TYPES THE
			// ATLAS READS: a TXT record's content is never decoded.
			var head struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(row, &head); err != nil {
				return 0, err
			}
			if head.ID == "" || seen[head.ID] {
				continue
			}
			seen[head.ID] = true
			rec := DNSRecord{ID: head.ID, Type: head.Type, Name: head.Name}
			if slices.Contains([]string{RecordA, RecordAAAA, RecordCNAME}, head.Type) {
				var target struct {
					Content string `json:"content"`
					Proxied bool   `json:"proxied"`
				}
				if err := json.Unmarshal(row, &target); err != nil {
					return 0, err
				}
				rec.Content, rec.Proxied = target.Content, target.Proxied
			}
			out = append(out, rec)
		}
		return len(rows), nil
	})
	return out, err
}

// CertificatePack is one of a zone's certificate packs: its id, type and
// status as Cloudflare names them, the names it covers, and its certificates.
type CertificatePack struct {
	ID     string
	Type   string
	Status string
	Hosts  []string

	// PrimaryCertificate is the id of the certificate the pack serves, where
	// Cloudflare names one.
	PrimaryCertificate string
	Certificates       []Certificate
}

// Certificate is one certificate of a pack: who issued it and when it lapses.
type Certificate struct {
	ID        string
	Issuer    string
	ExpiresOn time.Time
}

// CertificatePacks reads every certificate pack of a zone, whatever its
// status.
func (c *Client) CertificatePacks(ctx context.Context, zoneID string) ([]CertificatePack, error) {
	seen := map[string]bool{}
	var out []CertificatePack
	path := "/zones/" + url.PathEscape(zoneID) + "/ssl/certificate_packs"
	err := c.list(ctx, path, url.Values{"status": {"all"}}, func(raw json.RawMessage) (int, error) {
		var rows []struct {
			ID                 string   `json:"id"`
			Type               string   `json:"type"`
			Status             string   `json:"status"`
			Hosts              []string `json:"hosts"`
			PrimaryCertificate string   `json:"primary_certificate"`
			Certificates       []struct {
				ID        string `json:"id"`
				Issuer    string `json:"issuer"`
				ExpiresOn string `json:"expires_on"`
			} `json:"certificates"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0, err
		}
		for _, row := range rows {
			if row.ID == "" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			pack := CertificatePack{ID: row.ID, Type: row.Type, Status: row.Status, Hosts: row.Hosts,
				PrimaryCertificate: row.PrimaryCertificate}
			for _, cert := range row.Certificates {
				pack.Certificates = append(pack.Certificates, Certificate{
					ID: cert.ID, Issuer: cert.Issuer, ExpiresOn: parseTime(cert.ExpiresOn),
				})
			}
			out = append(out, pack)
		}
		return len(rows), nil
	})
	return out, err
}

// Primary is the certificate the pack serves: the one Cloudflare names as
// primary, else the one that lapses first, and false for a pack holding none.
func (p CertificatePack) Primary() (Certificate, bool) {
	if i := slices.IndexFunc(p.Certificates, func(c Certificate) bool {
		return c.ID != "" && c.ID == p.PrimaryCertificate
	}); i >= 0 {
		return p.Certificates[i], true
	}
	var best Certificate
	found := false
	for _, c := range p.Certificates {
		if c.ExpiresOn.IsZero() {
			continue
		}
		if !found || c.ExpiresOn.Before(best.ExpiresOn) {
			best, found = c, true
		}
	}
	return best, found
}
