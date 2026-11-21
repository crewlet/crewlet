package appstoreconnect

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// maxReportBytes bounds a sales report as it decompresses: a large app's
// daily file across every territory and product stays far below it, and a
// file past it stops the read with an error naming the day.
const maxReportBytes = 64 << 20

// SalesRow is one aggregate of a daily sales summary.
type SalesRow struct {
	App         string `json:"app"`
	Title       string `json:"title" untrusted:"name"`
	Territory   string `json:"territory"`
	ProductType string `json:"product_type"`
	Units       int64  `json:"units"`
	// Proceeds is the developer's proceeds in Currency, a decimal string.
	Proceeds string `json:"proceeds"`
	Currency string `json:"currency"`
	// Item is an in-app purchase's or subscription's own Apple ID, "" for an
	// app's own row.
	Item string `json:"item,omitempty"`

	// parent is the parent app's SKU an in-app purchase's row names, which
	// the sales tool reads as the app.
	parent string
}

// ErrReportTooLarge is a report past maxReportBytes.
var ErrReportTooLarge = errors.New("appstoreconnect: the sales report is past the size one read takes")

// salesKey groups a summary's lines.
type salesKey struct{ app, parent, title, territory, product, currency string }

// readSales aggregates a gzip TSV daily sales summary by app, territory,
// product type and proceeds currency: units summed, proceeds as units times
// the per-unit proceeds, in micros, never summed across currencies.
func readSales(gz []byte) ([]SalesRow, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("appstoreconnect: the sales report is not gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	r := bufio.NewReader(io.LimitReader(zr, maxReportBytes+1))
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("appstoreconnect: the sales report has no header: %w", err)
	}
	col := map[string]int{}
	for i, name := range strings.Split(strings.TrimRight(header, "\r\n"), "\t") {
		col[strings.TrimSpace(name)] = i
	}
	for _, need := range []string{"Apple Identifier", "Parent Identifier", "Title", "Country Code",
		"Product Type Identifier", "Units", "Developer Proceeds", "Currency of Proceeds"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("appstoreconnect: the sales report has no %q column", need)
		}
	}
	sums := map[salesKey]*[2]int64{}
	read := int64(len(header))
	for {
		line, err := r.ReadString('\n')
		read += int64(len(line))
		if read > maxReportBytes {
			return nil, ErrReportTooLarge
		}
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			f := strings.Split(line, "\t")
			get := func(name string) string {
				if i := col[name]; i < len(f) {
					return strings.TrimSpace(f[i])
				}
				return ""
			}
			units, perr := strconv.ParseInt(get("Units"), 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("appstoreconnect: a sales line's units are %q", get("Units"))
			}
			each, perr := signedMicros(get("Developer Proceeds"))
			if perr != nil {
				return nil, perr
			}
			k := salesKey{app: get("Apple Identifier"), parent: get("Parent Identifier"), title: get("Title"),
				territory: get("Country Code"), product: get("Product Type Identifier"),
				currency: get("Currency of Proceeds")}
			s := sums[k]
			if s == nil {
				s = &[2]int64{}
				sums[k] = s
			}
			s[0] += units
			s[1] += units * each
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("appstoreconnect: read the sales report: %w", err)
		}
	}
	out := make([]SalesRow, 0, len(sums))
	for k, s := range sums {
		proceeds, err := growth.FormatAmount(s[1], 6)
		if err != nil {
			return nil, err
		}
		out = append(out, SalesRow{App: k.app, Title: k.title, Territory: k.territory, ProductType: k.product,
			Units: s[0], Proceeds: trimZeros(proceeds), Currency: k.currency, parent: k.parent})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.App+a.parent+a.Territory+a.ProductType+a.Currency < b.App+b.parent+b.Territory+b.ProductType+b.Currency
	})
	return out, nil
}

// signedMicros reads a report's decimal amount, which a refund makes negative.
func signedMicros(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	m, err := growth.ParseAmount(strings.TrimPrefix(s, "-"))
	if err != nil {
		return 0, fmt.Errorf("appstoreconnect: a sales line's proceeds are %q: %w", s, err)
	}
	if neg {
		m = -m
	}
	return m, nil
}

// trimZeros drops a decimal's trailing zeros, and its point when nothing is
// left after it.
func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
