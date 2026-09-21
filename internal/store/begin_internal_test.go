package store

import (
	"database/sql"
	"database/sql/driver"
	"testing"
)

// THE BEGIN ADAPTER HIDES NO INTERFACE OF THE DRIVER'S CONNECTION.
//
// database/sql chooses its exec, query and prepare paths from the OPTIONAL
// interfaces a connection implements, and a wrapper carries only the ones it
// declares — an embedded driver.Conn carries none of them. [beginModeConn]
// forwards the five the pinned driver has today, and the failure mode of a
// driver bump that adds a sixth (a value checker, a session resetter) is
// silent: every statement quietly takes a different path than the driver
// meant, with no error anywhere. This is the tripwire for that, and it is
// written against the DRIVER's own set rather than a list of names, so it
// starts failing the day the driver gains one rather than the day somebody
// thinks to look.
func TestTheBeginAdapterHidesNoInterfaceOfTheDriver(t *testing.T) {
	t.Parallel()
	if err := prepareTursoLibrary(); err != nil {
		t.Fatalf("prepare the driver's library: %v", err)
	}
	probe, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("resolve the driver: %v", err)
	}
	drv := probe.Driver()
	_ = probe.Close()

	raw, err := drv.Open(":memory:")
	if err != nil {
		t.Fatalf("open a raw connection: %v", err)
	}
	defer func() { _ = raw.Close() }()
	wrapped, err := (&beginModeDriver{inner: drv}).Open(":memory:")
	if err != nil {
		t.Fatalf("open a wrapped connection: %v", err)
	}
	defer func() { _ = wrapped.Close() }()

	for name, has := range map[string]func(driver.Conn) bool{
		"ExecerContext":      func(c driver.Conn) bool { _, ok := c.(driver.ExecerContext); return ok },
		"QueryerContext":     func(c driver.Conn) bool { _, ok := c.(driver.QueryerContext); return ok },
		"ConnPrepareContext": func(c driver.Conn) bool { _, ok := c.(driver.ConnPrepareContext); return ok },
		"ConnBeginTx":        func(c driver.Conn) bool { _, ok := c.(driver.ConnBeginTx); return ok },
		"Pinger":             func(c driver.Conn) bool { _, ok := c.(driver.Pinger); return ok },
		"NamedValueChecker":  func(c driver.Conn) bool { _, ok := c.(driver.NamedValueChecker); return ok },
		"SessionResetter":    func(c driver.Conn) bool { _, ok := c.(driver.SessionResetter); return ok },
		"Validator":          func(c driver.Conn) bool { _, ok := c.(driver.Validator); return ok },
	} {
		if has(raw) && !has(wrapped) {
			t.Errorf("the driver's connection implements driver.%s and the begin "+
				"adapter hides it: forward it in begin.go, or database/sql will "+
				"quietly take a different path for every statement", name)
		}
	}
}
