package scaleway_test

import (
	"encoding/xml"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sigv4"
)

// productSets is the permission set each product listing takes at the fake,
// by route.
var productSets = map[string]string{
	"instances": "InstancesReadOnly", "kubernetes": "KubernetesReadOnly",
	"databases": "RelationalDatabasesReadOnly", "redis": "RedisReadOnly",
	"load_balancers": "LoadBalancersReadOnly", "dns": "DomainsDNSReadOnly",
	"containers": "ContainersReadOnly", "functions": "FunctionsReadOnly",
	"buckets": "ObjectStorageBucketsRead",
}

// productRoute is a product listing's route key, "" for any other path. A
// serverless namespace listing and a zone's records are routes of their own
// under their product's key.
func productRoute(parts []string) string {
	at := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	switch at(0) + "/" + at(1) {
	case "instance/v1":
		if at(2) == "zones" && at(4) == "servers" && len(parts) == 5 {
			return "instances"
		}
	case "k8s/v1":
		if at(2) == "regions" && at(4) == "clusters" && len(parts) == 5 {
			return "kubernetes"
		}
	case "rdb/v1":
		if at(2) == "regions" && at(4) == "instances" && len(parts) == 5 {
			return "databases"
		}
	case "redis/v1":
		if at(2) == "zones" && at(4) == "clusters" && len(parts) == 5 {
			return "redis"
		}
	case "lb/v1":
		if at(2) == "zones" && at(4) == "lbs" && len(parts) == 5 {
			return "load_balancers"
		}
	case "domain/v2beta1":
		switch {
		case at(2) == "dns-zones" && len(parts) == 3:
			return "dns"
		case at(2) == "dns-zones" && at(4) == "records" && len(parts) == 5:
			return "dns records"
		}
	case "containers/v1beta1", "functions/v1beta1":
		if at(2) == "regions" && len(parts) == 5 && (at(4) == "namespaces" || at(4) == at(0)) {
			if at(4) == "namespaces" {
				return at(0) + " namespaces"
			}
			return at(0)
		}
	}
	return ""
}

// grants reports whether the key a secret belongs to holds set on the
// Organization, through a policy on its application.
func (f *fakeScaleway) grants(secret, set string) bool {
	for _, k := range f.keys {
		if k.Secret != secret {
			continue
		}
		for _, p := range f.policies {
			if p.AppID != k.AppID {
				continue
			}
			for _, rule := range p.Rules {
				if rule.OnOrganization() && slices.Contains(rule.PermissionSetNames, set) {
					return true
				}
			}
		}
	}
	return false
}

// serveProduct answers one product listing: 403 to a key whose application
// holds no policy granting the product's set, 404 in a locality the product
// is not served in, and otherwise a page of the rows the test planted.
func (f *fakeScaleway) serveProduct(w http.ResponseWriter, r *http.Request, name string) {
	product := strings.Fields(name)[0]
	if !f.grants(r.Header.Get("X-Auth-Token"), productSets[product]) {
		f.fail(w, http.StatusForbidden, "permissions_denied", "insufficient permissions")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	var key, field string
	switch name {
	case "dns":
		key, field = name, "dns_zones"
	case "dns records":
		key, field = name+"/"+parts[3], "records"
	default:
		key, field = name+"/"+parts[3], parts[4]
	}
	if len(parts) == 5 && name != "dns records" && f.unserved[parts[3]] {
		f.fail(w, http.StatusNotFound, "not_found", "unknown locality "+parts[3])
		return
	}
	q := r.URL.Query()
	rows := make([]any, 0, len(f.rows[key]))
	for _, row := range f.rows[key] {
		rows = append(rows, row)
	}
	if name != "instances" {
		if name != "dns records" && q.Get("organization_id") != testOrg {
			f.fail(w, http.StatusBadRequest, "invalid_arguments", "organization_id is required")
			return
		}
		f.page(w, r, field, rows)
		return
	}
	// THE INSTANCE API'S OWN PAGING: `per_page`, and the total in a header.
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("per_page"))
	if q.Get("organization") != testOrg || page < 1 || size < 1 || size > 100 {
		f.fail(w, http.StatusBadRequest, "invalid_arguments", "organization, page and per_page are required")
		return
	}
	start := min((page-1)*size, len(rows))
	w.Header().Set("X-Total-Count", strconv.Itoa(len(rows)))
	f.ok(w, map[string]any{field: rows[start:min(start+size, len(rows))]})
}

// serveBuckets answers Object Storage's ListBuckets, signed as the S3
// protocol signs it: the access key names its project after an `@`, the
// signature is checked against the key's own secret, and the answer is the
// project's buckets in the region the request was signed for.
func (f *fakeScaleway) serveBuckets(w http.ResponseWriter, r *http.Request) {
	deny := func(status int, code string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_ = xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"Error"`
			Code    string
			Message string
		}{Code: code, Message: "access denied by the fake"})
	}
	auth := r.Header.Get("Authorization")
	credential, _, _ := strings.Cut(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential="), ",")
	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[3] != "s3" {
		deny(http.StatusForbidden, "InvalidAccessKeyId")
		return
	}
	access, project, _ := strings.Cut(scope[0], "@")
	region := scope[2]
	k, ok := f.keys[access]
	at, err := time.Parse(sigv4.DateFormat, r.Header.Get("X-Amz-Date"))
	if !ok || err != nil || r.URL.Path != "/" || r.Method != http.MethodGet {
		deny(http.StatusForbidden, "InvalidAccessKeyId")
		return
	}
	signed := http.Header{}
	for _, h := range []string{"X-Amz-Date", "X-Amz-Content-Sha256"} {
		signed.Set(h, r.Header.Get(h))
	}
	want := sigv4.Sign(sigv4.Request{
		Method: http.MethodGet, Host: "s3." + region + ".scw.cloud", Path: "/", Header: signed,
		PayloadHash: sigv4.PayloadHash(nil), S3: true,
	}, sigv4.Credentials{AccessKeyID: scope[0], SecretAccessKey: k.Secret}, region, "s3", at)
	switch {
	case auth != want:
		deny(http.StatusForbidden, "SignatureDoesNotMatch")
		return
	case f.refuse["buckets"] != 0:
		deny(f.refuse["buckets"], "AccessDenied")
		return
	case !f.grants(k.Secret, productSets["buckets"]):
		deny(http.StatusForbidden, "AccessDenied")
		return
	case f.unserved[region]:
		deny(http.StatusNotFound, "NoSuchRegion")
		return
	}
	type bucket struct {
		Name         string
		CreationDate string
	}
	var out struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Owner   struct{ ID, DisplayName string }
		Buckets struct {
			Bucket []bucket
		}
	}
	out.Owner.ID, out.Owner.DisplayName = project, canaryEmail
	for _, name := range f.buckets[project+"/"+region] {
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucket{Name: name, CreationDate: "2026-10-01T09:00:00.000Z"})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}
