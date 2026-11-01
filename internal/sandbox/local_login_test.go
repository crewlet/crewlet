//go:build unix

package sandbox

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// The cases here hold a box to the SHARED LOGIN BEING ONE LOGIN: a CLI that
// keeps its login in two files — kimi-code's token beside its config.toml,
// gemini-cli's token beside the account it names — is seeded with both from
// one sign-in and writes both back in one step, or neither.

// twoFileBox is a login of two files in a shared directory of the case's own,
// mapped where a box keeps them: the map a CLI entry hands a local box.
func twoFileBox(t *testing.T) (dir string, files map[string]string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "credentials")
	return dir, map[string]string{
		".fake/auth.json":   filepath.Join(dir, "auth.json"),
		".fake/config.toml": filepath.Join(dir, "config.toml"),
	}
}

// signedIn is a login of the two files, both naming one sign-in.
func signedIn(who string) map[string]string {
	return map[string]string{"auth.json": `{"token":"` + who + `"}`, "config.toml": `provider = "` + who + `"`}
}

// putLogin writes a login into a shared directory directly.
func putLogin(t *testing.T, dir string, login map[string]string) {
	t.Helper()
	for name, body := range login {
		writeFile(t, filepath.Join(dir, name), body)
	}
}

// sharedNow reads a shared directory's login as the next reader would.
func sharedNow(t *testing.T, dir string) map[string]string {
	t.Helper()
	release, err := hostbox.LockCredentials(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	login, err := hostbox.ReadCredentials(dir, []string{"auth.json", "config.toml"})
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	out := map[string]string{}
	for name, content := range login {
		out[name] = string(content)
	}
	return out
}

// boxHolds reads a box's copy of the login, "" left out for a file it lacks.
func boxHolds(t *testing.T, box Sandbox) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"auth.json", "config.toml"} {
		raw, err := os.ReadFile(filepath.Join(box.Home(), ".fake", name))
		switch {
		case err == nil:
			out[name] = string(raw)
		case !errors.Is(err, os.ErrNotExist):
			t.Fatal(err)
		}
	}
	return out
}

// A BOX IS SEEDED WITH THE LOGIN A WRITE COMMITTED, never the half it left. A
// writer killed between its two files left the new token beside the old
// config.toml and its journal beside both, and the box seeded a file at a time
// took the two from two sign-ins.
//
// Mutation: seed the box from the files as they lie, and it is handed half of
// each login.
func TestABoxIsSeededWithTheLoginAWriteCommitted(t *testing.T) {
	t.Parallel()
	dir, files := twoFileBox(t)
	putLogin(t, dir, signedIn("before"))
	// What a writer killed between its two files leaves, made by the
	// primitive itself in a directory of its own and moved into place.
	scratch := filepath.Join(t.TempDir(), "credentials")
	writeFile(t, filepath.Join(scratch, "config.toml", "in-the-way"), "x")
	after := map[string][]byte{}
	for name, body := range signedIn("after") {
		after[name] = []byte(body)
	}
	var unapplied *hostbox.UnappliedError
	if err := hostbox.WriteCredentials(scratch, []string{"auth.json", "config.toml"}, after); !errors.As(err, &unapplied) {
		t.Fatalf("the premise: a write stopped by its second file = %v", err)
	}
	for from, to := range map[string]string{
		hostbox.CredentialJournal(scratch):  hostbox.CredentialJournal(dir),
		filepath.Join(scratch, "auth.json"): filepath.Join(dir, "auth.json"),
	} {
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
	}

	box := mustCreate(t, newDirect(t), Spec{CredentialFiles: files})
	if got, want := boxHolds(t, box), signedIn("after"); !maps.Equal(got, want) {
		t.Errorf("the box was seeded with %v, want the login the write committed %v", got, want)
	}
}

// A BOX IS SEEDED WITH A WHOLE LOGIN OR NONE. One of its files cannot be placed
// in the box — its path climbs out of the home — so the box is given neither:
// handed the other alone, the CLI starts on half a login and fails inside the
// run in a way nothing outside it explains.
//
// Mutation: seed the files that can be placed and skip the one that cannot,
// and the box holds half a login.
func TestABoxIsSeededWithAWholeLoginOrNone(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	putLogin(t, dir, signedIn("whole"))
	box := mustCreate(t, newDirect(t), Spec{CredentialFiles: map[string]string{
		".fake/auth.json":      filepath.Join(dir, "auth.json"),
		"../../../config.toml": filepath.Join(dir, "config.toml"),
	}})
	if got := boxHolds(t, box); len(got) != 0 {
		t.Errorf("a box that could not be given the whole login was given %v", got)
	}
}

// A BOX THAT REFRESHED BOTH FILES WRITES BOTH BACK, as one login.
func TestABoxWritesBackEveryFileItRefreshed(t *testing.T) {
	t.Parallel()
	dir, files := twoFileBox(t)
	putLogin(t, dir, signedIn("seed"))
	box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for name, body := range signedIn("refreshed") {
		writeFile(t, filepath.Join(box.Home(), ".fake", name), body)
	}
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := sharedNow(t, dir), signedIn("refreshed"); !maps.Equal(got, want) {
		t.Errorf("the shared login is %v, want the box's refresh %v", got, want)
	}
}

// A BOX'S REFRESH IS WRITTEN ONLY OVER THE WHOLE LOGIN IT WAS SEEDED WITH. The
// run refreshed the token while the host rewrote the config.toml beside it:
// judged a file at a time, the token's file still held its seed and was
// written back, and the shared login became the box's token beside the host's
// config.toml — a login no CLI wrote. The host's newer login is kept whole.
//
// Mutation: compare each file the run changed with its own seed alone, and
// the box's token lands beside the host's config.toml.
func TestABoxRefreshIsNeverWrittenBesideAnotherWritersFile(t *testing.T) {
	t.Parallel()
	dir, files := twoFileBox(t)
	putLogin(t, dir, signedIn("seed"))
	box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, filepath.Join(box.Home(), ".fake", "auth.json"), `{"token":"box-refreshed"}`)
	writeFile(t, filepath.Join(dir, "config.toml"), `provider = "host"`)
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := map[string]string{"auth.json": `{"token":"seed"}`, "config.toml": `provider = "host"`}
	if got := sharedNow(t, dir); !maps.Equal(got, want) {
		t.Errorf("the shared login is %v, want the host's kept whole %v", got, want)
	}
}

// A FILE THE LOGIN GAINED WHILE THE BOX RAN IS A NEWER LOGIN TOO. The box was
// seeded with the token alone; the host's login gained a config.toml
// meanwhile, the token's file untouched. The box's refresh of the token is no
// refresh of that login, and written beside it would make one no CLI wrote.
//
// Mutation: record only the files a box was given, and the box's token is
// written beside the host's new config.toml.
func TestABoxRefreshIsNeverWrittenOverALoginThatGainedAFile(t *testing.T) {
	t.Parallel()
	dir, files := twoFileBox(t)
	putLogin(t, dir, map[string]string{"auth.json": `{"token":"seed"}`})
	box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, filepath.Join(box.Home(), ".fake", "auth.json"), `{"token":"box-refreshed"}`)
	writeFile(t, filepath.Join(dir, "config.toml"), `provider = "host"`)
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := map[string]string{"auth.json": `{"token":"seed"}`, "config.toml": `provider = "host"`}
	if got := sharedNow(t, dir); !maps.Equal(got, want) {
		t.Errorf("the shared login is %v, want the host's kept whole %v", got, want)
	}
}

// A BOX OF A LOGIN NOBODY MADE IS GIVEN NOTHING. Every file of the login is
// offered whether or not it is held, and the box is seeded with what the login
// holds then — none of it here, so the CLI inside answers its own plain "not
// authenticated" rather than failing on a file that is not there.
func TestABoxOfALoginNobodyMadeIsGivenNothing(t *testing.T) {
	t.Parallel()
	_, files := twoFileBox(t)
	box := mustCreate(t, newDirect(t), Spec{CredentialFiles: files})
	if got := boxHolds(t, box); len(got) != 0 {
		t.Errorf("a box of a login nobody made was given %v", got)
	}
}

// twoFileLogin is the box login of [twoFileBox]: both files from dir, seeded at
// the paths a box keeps them.
func twoFileLogin(dir string, files map[string]string) boxLogin {
	return boxLogin{dir: dir, files: files}
}

// A BOX SEED THAT STOPS PART WAY TAKES BACK WHAT IT PUT IN THE BOX. The second
// file cannot be put where the box keeps it — something is in the way — after
// the first was: handed the first alone, the CLI starts on half a login and
// fails inside the run in a way nothing outside it explains.
//
// Mutation: drop seedLogin's removal of the files it had already written, and
// the box holds the first file.
func TestABoxSeedThatStopsPartWayLeavesTheBoxNoFile(t *testing.T) {
	t.Parallel()
	dir, files := twoFileBox(t)
	putLogin(t, dir, signedIn("whole"))
	layout := boxLayout{id: "seed", root: t.TempDir()}
	// The second file's place in the box is a directory, so writing it fails
	// once the first file — sorted ahead of it — is in.
	writeFile(t, filepath.Join(layout.home(), ".fake", "config.toml", "in-the-way"), "x")
	if _, err := seedLogin(t.Context(), layout, twoFileLogin(dir, files)); err == nil {
		t.Fatal("a seed whose second file could not be written reported success")
	}
	if _, err := os.Lstat(filepath.Join(layout.home(), ".fake", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a seed that stopped part way left its first file in the box (%v)", err)
	}
}

// A BOX REFRESH WHOSE WRITE COMMITTED IS WRITTEN, even where the disk refused
// one of its files: its journal landed, so it is the login of record and the
// next reader of the shared directory puts it in place. Reported as a failed
// write-back, the teardown said the refresh went nowhere while every seat was
// about to be seeded with it.
//
// NOT PARALLEL: it reads the process's log.
//
// Mutation: treat any error from the write as a write-back that failed, and
// the refresh is reported as not written.
func TestABoxRefreshThatCommittedIsWrittenThoughAFileWasRefused(t *testing.T) {
	logs := captureLogs(t)
	dir, files := twoFileBox(t)
	putLogin(t, dir, signedIn("before"))
	layout := boxLayout{id: "unapplied", root: t.TempDir()}
	seeded := map[string]seededCredential{}
	for relative, source := range files {
		seeded[relative] = seededCredential{Source: source, Seed: digestOf([]byte(signedIn("before")[filepath.Base(source)]))}
	}
	for name, body := range signedIn("refreshed") {
		writeFile(t, filepath.Join(layout.home(), ".fake", name), body)
	}
	obstacle := filepath.Join(dir, "config.toml")
	// The disk refusing config.toml once the write has committed: the
	// obstacle goes down between the write-back's read and its write.
	refuseSecond := func(d string, names []string, login map[string][]byte) error {
		if err := os.Remove(obstacle); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(obstacle, "in-the-way"), 0o700); err != nil {
			return err
		}
		return hostbox.WriteCredentials(d, names, login)
	}

	got, err := writeBackLogin(t.Context(), layout, twoFileLogin(dir, files), seeded, refuseSecond)
	if err != nil {
		t.Fatalf("a refresh whose write committed was reported as failed: %v", err)
	}
	for relative := range files {
		if got[relative] != writeBackWritten {
			t.Errorf("%s: %q, want %q", relative, got[relative], writeBackWritten)
		}
	}
	if !strings.Contains(logs.String(), "local_sandbox_credential_unapplied") {
		t.Errorf("a refresh not yet put in place was not said: %q", logs.String())
	}
	if err := os.RemoveAll(obstacle); err != nil {
		t.Fatal(err)
	}
	if got, want := sharedNow(t, dir), signedIn("refreshed"); !maps.Equal(got, want) {
		t.Errorf("the next reader read %v, want the refresh the write committed %v", got, want)
	}
}
