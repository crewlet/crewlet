package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/store"
)

// holdStoreEnv names the database a re-executed copy of this test binary opens
// and holds, standing in for `crewlet run` ([holdTheStore]); holdEstateEnv says
// which of the node's two files it is.
const (
	holdStoreEnv  = "CREWLET_TEST_HOLD_STORE"
	holdEstateEnv = "CREWLET_TEST_HOLD_ESTATE"
)

// runHoldStore is the stand-in's whole life: open the database as the engine
// does, say so, and hold it until the parent closes stdin — which the kernel
// does too when the parent dies, so no stand-in outlives its case.
func runHoldStore(path string) int {
	var db *store.DB
	var err error
	if os.Getenv(holdEstateEnv) == string(store.EstateReplicated) {
		db, err = store.OpenEstate(context.Background(), store.EstateReplicated, path, store.Options{})
	} else {
		db, err = store.OpenNode(context.Background(), path, store.Options{})
	}
	if err != nil {
		fmt.Printf("open failed: %v\n", err)
		return 4
	}
	fmt.Println("held")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = db.Close()
	return 0
}

// holdTheStore makes the database at path held by a SECOND PROCESS for the
// rest of the case, which is exactly what a running `crewlet run` does to it.
// estate says which of the node's two files path is, since each opens under
// its own migration sequence.
//
// A second process is the whole property: the store's lock is refcounted
// within one, so a Go test opening the file itself would share the claim
// rather than meet it. So this binary is re-executed as the holder, and the
// case goes on only once the holder has said it has the file.
func holdTheStore(t *testing.T, path string, estate store.Estate) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holdStoreEnv+"="+path,
		holdEstateEnv+"="+string(estate),
		procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the store's holder: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("the holder did not take the store: %q (%v)", line, err)
	}
}
