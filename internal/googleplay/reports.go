package googleplay

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// maxReportBytes bounds one report the engine reads: a monthly CSV of one app
// and dimension is kilobytes to a few megabytes, a month's earnings ZIP tens.
const maxReportBytes = 64 << 20

// readerTokens mints the company's reader's token for every call, whoever
// the seat: the bucket is read as the company, then cut to the seat's apps.
type readerTokens struct{ reader Reader }

func (t readerTokens) GoogleGrowthToken(ctx context.Context, _ string, scopes []string, _ time.Duration,
	hc *http.Client) (googleauth.SeatToken, error) {
	token, err := t.reader.GoogleReaderToken(ctx, scopes, hc)
	return googleauth.SeatToken{Token: token}, err
}

// readerClient is the client that reads the bucket as the company's reader,
// nil where this node mints no reader token.
func (p *Product) readerClient() *googlegrowth.Client {
	if p.deps.Reader == nil {
		return nil
	}
	c := *p.client
	c.Tokens = readerTokens{reader: p.deps.Reader}
	return &c
}

var (
	// statsObject is a statistics report: kind, package, month, dimension.
	statsObject = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^stats/([a-z_]+)/([a-z_]+)_(.+)_([0-9]{6})_([a-z_]+)\.csv$`)
	})
	// reviewsObject is a month's reviews of one package.
	reviewsObject = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^reviews/reviews_(.+)_([0-9]{6})\.csv$`)
	})
	// cancellationsObject is one package's cancellation survey answers.
	cancellationsObject = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^subscriptions/cancellations/freeform_(.+)\.csv$`)
	})
)

// objectApp is the one package a report is of, "" for an account-wide one
// (sales, earnings, invoices, balances, subscription statistics, whose names
// cannot be told apart from another package's). The month and dimension end
// each name, so the package read between them is exact.
func objectApp(object string) string {
	if m := statsObject().FindStringSubmatch(object); m != nil && m[1] == m[2] && config.GooglePlayPackage(m[3]) {
		return m[3]
	}
	for _, re := range []*regexp.Regexp{reviewsObject(), cancellationsObject()} {
		if m := re.FindStringSubmatch(object); m != nil && config.GooglePlayPackage(m[1]) {
			return m[1]
		}
	}
	return ""
}

// objectInScope refuses a report of an app the seat may not reach, and an
// account-wide report to a seat that does not reach every company app.
func (p *Product) objectInScope(b *config.RoleGooglePlay, object string) error {
	app := objectApp(object)
	if app == "" {
		// AN ACCOUNT-WIDE REPORT HOLDS EVERY APP OF THE ACCOUNT, listed by the
		// company or not, so it is given on purpose: an agent at administer.
		if b.LevelOrDefault() != growth.LevelAdminister {
			return refusal(growth.RefusedForbidden, object+" is an account-wide report, covering every app of the "+
				"developer account, which only an agent at administer reads. No vendor was reached.")
		}
		return p.reachesAll(b, object+" is an account-wide report, which covers every app of the account")
	}
	if !slices.Contains(b.Apps, app) || !p.company().Reaches(app) {
		return refusal(growth.RefusedForbidden, object+" is app "+app+"'s, which is not one of this agent's apps. "+
			"No vendor was reached.")
	}
	return nil
}

// bucketArg is the bucket a reports call names, or the company's one bucket.
func (p *Product) bucketArg(v any) (string, error) {
	bucket, _ := v.(string)
	company := p.company()
	if bucket == "" && company != nil && len(company.ReportsBuckets) == 1 {
		bucket = company.ReportsBuckets[0]
	}
	if bucket == "" {
		return "", refusal(growth.RefusedInvalid, "name the reports bucket, one of "+
			"integrations.google_play.reports_buckets. No vendor was reached.")
	}
	return bucket, nil
}

// Report is a monthly report as an agent reads it: its rows, paged, which
// carry words others wrote (reviews, product titles, survey answers).
type Report struct {
	Bucket    string   `json:"bucket"`
	Object    string   `json:"object"`
	Columns   []string `json:"columns"`
	Rows      string   `json:"rows" untrusted:"fence"`
	Offset    int      `json:"offset"`
	TotalRows int      `json:"total_rows"`
	Truncated bool     `json:"truncated"`
	Note      string   `json:"note"`
}

// Objects is a reports listing, cut to what the seat may read.
type Objects struct {
	Bucket  string        `json:"bucket"`
	Objects []ObjectEntry `json:"objects"`
	Next    string        `json:"next_page_token,omitempty"`
}

// ObjectEntry is one report of a listing.
type ObjectEntry struct {
	Name    string `json:"name"`
	Size    string `json:"size"`
	Updated string `json:"updated"`
}

// readReport is google_play_read_report: a listing by prefix, or one report.
func (p *Product) readReport(ctx context.Context, call growth.Call) (any, error) {
	bucket, err := p.bucketArg(call.Args["bucket"])
	if err != nil {
		return nil, err
	}
	object, _ := call.Args["object"].(string)
	if object == "" {
		prefix, _ := call.Args["prefix"].(string)
		op, _ := p.Catalog().Operation("storage.objects.list")
		req, listErr := newRequest(op, map[string]string{"bucket": bucket}, map[string]string{"prefix": prefix,
			"maxResults": "1000"}, nil)
		if listErr != nil {
			return nil, listErr
		}
		return p.readBucket(ctx, call, req)
	}
	op, _ := p.Catalog().Operation("storage.objects.get")
	req, err := newRequest(op, map[string]string{"bucket": bucket, "object": object}, map[string]string{"alt": "media"},
		nil)
	if err != nil {
		return nil, err
	}
	return p.readBucket(ctx, call, req)
}

// readBucket makes a reports bucket read as the company's reader: a listing
// cut to the seat's reports, a report's rows decoded, or the read as Google
// answers it.
func (p *Product) readBucket(ctx context.Context, call growth.Call, req googlegrowth.Request) (any, error) {
	if err := p.inScope(call.Handle, req); err != nil {
		return nil, err
	}
	reader := p.readerClient()
	if reader == nil {
		return nil, refusal(growth.RefusedUnavailable, "this node reads no reports bucket. No vendor was reached.")
	}
	var out any
	var err error
	switch {
	case req.Op.ID == "storage.objects.list":
		out, err = p.listObjects(ctx, call.Handle, reader, req)
	case req.Op.ID == "storage.objects.get" && req.Query.Get("alt") == "media":
		out, err = p.report(ctx, call, reader, req)
	default:
		out, err = reader.SendAndRead(ctx, call.Handle, req, nil)
	}
	reader.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// listObjects lists a bucket's reports the seat may read.
func (p *Product) listObjects(ctx context.Context, handle string, reader *googlegrowth.Client,
	req googlegrowth.Request) (Objects, error) {
	raw, err := reader.Send(ctx, handle, req)
	if err != nil {
		return Objects{}, err
	}
	var doc struct {
		Items []struct {
			Name    string `json:"name"`
			Size    string `json:"size"`
			Updated string `json:"updated"`
		} `json:"items"`
		Next string `json:"nextPageToken"`
	}
	if err := decode(raw, "the reports listing", &doc); err != nil {
		return Objects{}, err
	}
	b := p.seat(handle)
	out := Objects{Bucket: req.Params["bucket"], Objects: []ObjectEntry{}, Next: doc.Next}
	for _, it := range doc.Items {
		if b != nil && p.objectInScope(b, it.Name) == nil {
			out.Objects = append(out.Objects, ObjectEntry{Name: it.Name, Size: it.Size, Updated: it.Updated})
		}
	}
	return out, nil
}

// errTooLarge is a report larger than the engine reads.
var errTooLarge = errors.New("googleplay: the report is larger than Crewlet reads")

// capped is a buffer refusing to hold more than its cap.
type capped struct {
	bytes.Buffer
	cap int
}

func (c *capped) Write(b []byte) (int, error) {
	if c.Len()+len(b) > c.cap {
		return 0, errTooLarge
	}
	return c.Buffer.Write(b)
}

// report reads one report and answers its rows, paged.
func (p *Product) report(ctx context.Context, call growth.Call, reader *googlegrowth.Client,
	req googlegrowth.Request) (Report, error) {
	buf := &capped{cap: maxReportBytes}
	if err := reader.SendTo(ctx, call.Handle, req, buf); err != nil {
		return Report{}, err
	}
	object := req.Params["object"]
	text, err := reportText(object, buf.Bytes())
	if err != nil {
		return Report{}, err
	}
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return Report{}, fmt.Errorf("googleplay: read %s as CSV: %w", object, err)
	}
	out := Report{Bucket: req.Params["bucket"], Object: object, Note: "The company's figures, read as its reader: " +
		"Google posts statistics within 3 to 7 days of each day, in monthly files."}
	if len(records) == 0 {
		out.Rows = "[]"
		return out, nil
	}
	out.Columns, records = records[0], records[1:]
	limit, offset := intArg(call.Args["limit"], 200), intArg(call.Args["offset"], 0)
	limit = min(max(limit, 1), growth.ReportMaxRows)
	out.TotalRows, out.Offset = len(records), offset
	if offset > len(records) {
		offset = len(records)
	}
	page := records[offset:min(offset+limit, len(records))]
	out.Truncated = offset+len(page) < len(records)
	rows, _ := json.Marshal(page)
	out.Rows = string(rows)
	return out, nil
}

// reportText is a report's CSV as UTF-8: a ZIP's first CSV, UTF-16 decoded
// where Google wrote it so (its statistics files are).
func reportText(object string, data []byte) (string, error) {
	if strings.HasSuffix(object, ".zip") {
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return "", fmt.Errorf("googleplay: open %s: %w", object, err)
		}
		for _, f := range z.File {
			if !strings.HasSuffix(f.Name, ".csv") {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("googleplay: open %s in %s: %w", f.Name, object, err)
			}
			inner, err := io.ReadAll(io.LimitReader(rc, maxReportBytes+1))
			_ = rc.Close()
			if err != nil {
				return "", fmt.Errorf("googleplay: read %s in %s: %w", f.Name, object, err)
			}
			if len(inner) > maxReportBytes {
				return "", errTooLarge
			}
			return reportText(f.Name, inner)
		}
		return "", fmt.Errorf("googleplay: %s holds no CSV", object)
	}
	return utf8Of(data), nil
}

// utf8Of is text as UTF-8, read as UTF-16 where it starts with a UTF-16 mark.
func utf8Of(data []byte) string {
	if len(data) >= 2 && (data[0] == 0xFF && data[1] == 0xFE || data[0] == 0xFE && data[1] == 0xFF) {
		little := data[0] == 0xFF
		units := make([]uint16, 0, len(data)/2)
		for i := 2; i+1 < len(data); i += 2 {
			if little {
				units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
			} else {
				units = append(units, uint16(data[i])<<8|uint16(data[i+1]))
			}
		}
		return string(utf16.Decode(units))
	}
	return strings.TrimPrefix(string(data), "\ufeff")
}

// intArg is an integer argument, def where absent.
func intArg(v any, def int) int {
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return def
}
