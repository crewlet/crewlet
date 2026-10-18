package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet artifacts`: the company's artifacts from a shell, through the same
// HTTP surface the dashboard is built on (`/artifacts`), as whoever
// CREWLET_API_TOKEN stands for. Every gesture is decided by the node on that
// principal exactly as the dashboard's is: a share is the owner's, a publish an
// editor's with the artifacts app, a purge an administrator's with a person
// present.
//
// # A publish moves only the bytes the artifact does not hold
//
// `publish` always begins with an upload intent: every file's path, digest and
// size are declared first, the node answers which of them the artifact already
// holds (those are carried and no byte moves) and how each of the rest is
// transferred (straight to the bucket on a presigned put where the object
// store offers one, through the node otherwise), and only then are the new
// files sent and the version published, based on the head it read.
//
// # A retry is the same operation
//
// Every write mints its operation id before the first request and sends it as
// the Idempotency-Key; an answer whose outcome is unknown, or none at all,
// prints it, and `-op-id` sends it again, so a retry is the first attempt's
// operation rather than a second one.

// artifactsUsage is the command's usage.
const artifactsUsage = `usage: crewlet artifacts <command> [arguments] [<config.yaml>] [-url URL]

  list [-scope mine|shared|company|agents|all] [-kind html|markdown|sheet]
       [-q TEXT] [-status live|trashed] [-work-item KEY] [-limit N]
  get <id>
  publish <dir|file> [-artifact ID] [-title TEXT] [-kind html|markdown]
       [-entry PATH] [-label TEXT] [-contract VERSION] [-capabilities a,b]
       [-work-item KEY] [-force] [-op-id ID]
  versions <id>
  restore <id> <version> [-label TEXT] [-op-id ID]
  label <id> <version> <label> [-op-id ID]
  share <id> <seat:HANDLE|unit:KEY|company> <viewer|commenter|contributor|editor> [-op-id ID]
  unshare <id> <seat:HANDLE|unit:KEY|company> [-op-id ID]
  trash <id> [-op-id ID]
  untrash <id> [-op-id ID]
  purge <id> -confirm <id> [-op-id ID]
  assets <id>
  usage

The credential is ` + apiTokenEnv + `.`

// runArtifacts dispatches `crewlet artifacts`.
func runArtifacts(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	run, known := map[string]func(config.Source, []string, io.Writer, io.Writer) error{
		"list": artifactsList, "get": artifactsGet, "publish": artifactsPublish,
		"versions": artifactsVersions, "restore": artifactsRestore, "label": artifactsLabel,
		"share": artifactsShare, "unshare": artifactsUnshare, "trash": artifactsTrash,
		"untrash": artifactsUntrash, "purge": artifactsPurge, "assets": artifactsAssets,
		"usage": artifactsUsageCmd,
	}[sub]
	switch {
	case sub == "" || sub == "help":
		fmt.Fprintln(stderr, artifactsUsage)
		return flag.ErrHelp
	case !known:
		fmt.Fprintln(stderr, artifactsUsage)
		return fmt.Errorf("unknown artifacts command %q", sub)
	}
	return run(environ, rest, stdout, stderr)
}

// positionals takes the n leading arguments a subcommand names, refusing a
// command short of them.
func positionals(args []string, n int, what string) ([]string, []string, error) {
	out := make([]string, 0, n)
	for len(out) < n {
		var next string
		next, args = splitSubject(args)
		if next == "" {
			return nil, nil, fmt.Errorf("name %s\n\n%s", what, artifactsUsage)
		}
		out = append(out, next)
	}
	return out, args, nil
}

// artifactID refuses an argument that is not an artifact id before anything
// is sent.
func artifactID(raw string) (artifacts.ID, error) {
	id, err := artifacts.ParseID(strings.TrimSpace(raw))
	if err != nil {
		return artifacts.ID{}, fmt.Errorf("%q is not an artifact id: an id is the uuid "+
			"`crewlet artifacts list` prints", raw)
	}
	return id, nil
}

// operationFlag registers -op-id, the retry of an unknown outcome.
func operationFlag(fs *flag.FlagSet) *string {
	return fs.String("op-id", "", "retry an `unknown` outcome with the id it printed, so the "+
		"retry is the same operation; empty starts a new one")
}

// operation is the id a write is sent under: the one the operator brought,
// held to the node's rule before anything is sent, or a fresh one.
func operation(given, name string) (string, error) {
	if given = strings.TrimSpace(given); given == "" {
		return statelog.NewOpID(time.Now(), name), nil
	}
	if err := statelog.CheckCallerOpID(given); err != nil {
		return "", fmt.Errorf("-op-id: %w", err)
	}
	return given, nil
}

// unknownOutcome turns an answer that never came, or one that could not be
// accounted for, into the sentence that says how to retry it.
func unknownOutcome(err error, gesture, op string) error {
	retry := fmt.Sprintf("run the same command again with -op-id %s, so the retry is this "+
		"operation rather than a second one", op)
	var lost noAnswer
	if errors.As(err, &lost) {
		return fmt.Errorf("%w\n\nWhether the %s landed is unknown: %s", err, gesture, retry)
	}
	var refused *nodeRefusal
	if errors.As(err, &refused) && refused.Unsettled {
		if refused.Unvouched {
			return fmt.Errorf("%w\n\nThis node cannot tell whether it landed: run it through "+
				"another node with -url <that node> -op-id %s, never a fresh id", err, op)
		}
		return fmt.Errorf("%w\n\nTo find out, %s", err, retry)
	}
	return err
}

// write sends one keyed JSON write and decodes its answer.
func (c *nodeClient) write(ctx context.Context, method, path, key string, header http.Header,
	body, into any) error {

	if header == nil {
		header = http.Header{}
	}
	header.Set(opkey.Header, key)
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode the request: %w", err)
		}
		payload = raw
	}
	return c.send(ctx, method, path, header, payload, into)
}

// writtenAnswer is a gesture's answer: its outcome and the operation it ran
// under.
type writtenAnswer struct {
	Outcome string `json:"outcome"`
	OpID    string `json:"op_id"`
}

// gesture runs one keyed write on an artifact and prints what landed.
func gesture(environ config.Source, args []string, stdout, stderr io.Writer, name string,
	need int, what string, send func(c *nodeClient, pos []string, key string) (string, error),
	extra func(*flag.FlagSet)) error {

	pos, rest, err := positionals(args, need, what)
	if err != nil {
		return err
	}
	var opID *string
	client, err := nodeClientFor(environ, rest, "artifacts "+name, stderr, func(fs *flag.FlagSet) {
		opID = operationFlag(fs)
		if extra != nil {
			extra(fs)
		}
	})
	if err != nil {
		return err
	}
	key, err := operation(*opID, name)
	if err != nil {
		return err
	}
	done, err := send(client, pos, key)
	if err != nil {
		return unknownOutcome(err, name, key)
	}
	fmt.Fprintln(stdout, done)
	return nil
}

// outcomeLine is what a gesture prints: what it did and, where the node has
// not applied it yet, that it is durable.
func outcomeLine(done string, a writtenAnswer) string {
	if a.Outcome == string(statelog.OutcomePending) {
		return done + " (durable; this node has not applied it yet)"
	}
	return done
}

// ---- reads ---------------------------------------------------------------- //

// artifactRow is one artifact as the listing and `get` print it.
type artifactRow struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Owner      string    `json:"owner"`
	HeadNumber int       `json:"head_number"`
	Status     string    `json:"status"`
	Role       string    `json:"role"`
	ShareCount int       `json:"share_count"`
	UpdatedAt  time.Time `json:"updated_at"`
	Shares     []struct {
		Audience artifacts.Audience `json:"audience"`
		Role     string             `json:"role"`
	} `json:"shares"`
}

func artifactsList(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var scope, kind, q, status, item *string
	var limit *int
	client, err := nodeClientFor(environ, args, "artifacts list", stderr, func(fs *flag.FlagSet) {
		scope = fs.String("scope", "", "mine, shared, company, agents or all (the default)")
		kind = fs.String("kind", "", kindNames())
		q = fs.String("q", "", "text the title, the description or the content holds")
		status = fs.String("status", "", "live (the default) or trashed")
		item = fs.String("work-item", "", "only the artifacts made for this work item")
		limit = fs.Int("limit", 0, "how many to list (default 50, at most 200)")
	})
	if err != nil {
		return err
	}
	query := url.Values{}
	for name, value := range map[string]string{"scope": *scope, "kind": *kind, "q": *q,
		"status": *status, "work_item": *item} {
		if value = strings.TrimSpace(value); value != "" {
			query.Set(name, value)
		}
	}
	if *limit > 0 {
		query.Set("limit", strconv.Itoa(*limit))
	}
	var page struct {
		Items []artifactRow `json:"items"`
		Total int           `json:"total"`
	}
	target := "/artifacts"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	if err := client.get(context.Background(), target, &page); err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(stdout, "no artifacts")
		return nil
	}
	for _, a := range page.Items {
		fmt.Fprintf(stdout, "%s  %-8s  v%-3d  %-10s  %s\n", a.ID, a.Kind, a.HeadNumber, a.Owner, a.Title)
	}
	if page.Total > len(page.Items) {
		fmt.Fprintf(stdout, "%d of %d shown; narrow the listing or raise -limit\n", len(page.Items), page.Total)
	}
	return nil
}

func artifactsGet(environ config.Source, args []string, stdout, stderr io.Writer) error {
	pos, rest, err := positionals(args, 1, "the artifact")
	if err != nil {
		return err
	}
	id, err := artifactID(pos[0])
	if err != nil {
		return err
	}
	client, err := nodeClientFor(environ, rest, "artifacts get", stderr, nil)
	if err != nil {
		return err
	}
	var a artifactRow
	if err := client.get(context.Background(), "/artifacts/"+id.String(), &a); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n  title    %s\n  kind     %s\n  owner    %s\n  version  %d\n"+
		"  status   %s\n  role     %s (yours)\n  updated  %s\n", a.ID, a.Title, a.Kind, a.Owner,
		a.HeadNumber, a.Status, a.Role, a.UpdatedAt.Format(time.RFC3339))
	if a.Shares == nil {
		fmt.Fprintf(stdout, "  shares   %d\n", a.ShareCount)
		return nil
	}
	for _, s := range a.Shares {
		fmt.Fprintf(stdout, "  shared   %s as %s\n", audienceText(s.Audience), s.Role)
	}
	return nil
}

// audienceText is an audience as an operator names it.
func audienceText(a artifacts.Audience) string {
	if a.Kind == artifacts.AudienceCompany {
		return string(artifacts.AudienceCompany)
	}
	return string(a.Kind) + ":" + a.ID
}

func artifactsVersions(environ config.Source, args []string, stdout, stderr io.Writer) error {
	pos, rest, err := positionals(args, 1, "the artifact")
	if err != nil {
		return err
	}
	id, err := artifactID(pos[0])
	if err != nil {
		return err
	}
	client, err := nodeClientFor(environ, rest, "artifacts versions", stderr, nil)
	if err != nil {
		return err
	}
	var page struct {
		Versions []struct {
			Number    int       `json:"number"`
			Label     string    `json:"label"`
			Files     int       `json:"files"`
			Bytes     int64     `json:"bytes"`
			CreatedAt time.Time `json:"created_at"`
			Author    struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"versions"`
	}
	if err := client.get(context.Background(), "/artifacts/"+id.String()+"/versions", &page); err != nil {
		return err
	}
	for _, v := range page.Versions {
		label := ""
		if v.Label != "" {
			label = "  [" + v.Label + "]"
		}
		fmt.Fprintf(stdout, "v%-4d %s  %-12s  %d files, %d bytes%s\n", v.Number,
			v.CreatedAt.Format(time.RFC3339), v.Author.Name, v.Files, v.Bytes, label)
	}
	return nil
}

func artifactsAssets(environ config.Source, args []string, stdout, stderr io.Writer) error {
	pos, rest, err := positionals(args, 1, "the artifact")
	if err != nil {
		return err
	}
	id, err := artifactID(pos[0])
	if err != nil {
		return err
	}
	client, err := nodeClientFor(environ, rest, "artifacts assets", stderr, nil)
	if err != nil {
		return err
	}
	var answer struct {
		Assets []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			ContentType string `json:"content_type"`
			Object      struct {
				Size int64 `json:"size"`
			} `json:"object"`
		} `json:"assets"`
	}
	if err := client.get(context.Background(), "/artifacts/"+id.String()+"/assets", &answer); err != nil {
		return err
	}
	if len(answer.Assets) == 0 {
		fmt.Fprintln(stdout, "no assets")
	}
	for _, a := range answer.Assets {
		fmt.Fprintf(stdout, "%s  %-24s  %s  %d bytes\n", a.ID, a.Name, a.ContentType, a.Object.Size)
	}
	return nil
}

func artifactsUsageCmd(environ config.Source, args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(environ, args, "artifacts usage", stderr, nil)
	if err != nil {
		return err
	}
	var answer struct {
		Usage struct {
			Artifacts int64 `json:"artifacts"`
			Bytes     int64 `json:"bytes"`
			Versions  int64 `json:"versions"`
			Assets    int64 `json:"assets"`
		} `json:"usage"`
		Limits artifacts.Limits `json:"limits"`
	}
	if err := client.get(context.Background(), "/artifacts/usage", &answer); err != nil {
		return err
	}
	u, l := answer.Usage, answer.Limits
	fmt.Fprintf(stdout, "artifacts  %d of %d\nbytes      %d of %d\nversions   %d\nassets     %d\n",
		u.Artifacts, l.Artifacts, u.Bytes, l.CompanyBytes, u.Versions, u.Assets)
	return nil
}

// ---- the gestures ---------------------------------------------------------- //

// headOf is an artifact's head version, the base a publish or a rollback is
// decided against.
func headOf(c *nodeClient, id artifacts.ID) (int, error) {
	var a artifactRow
	if err := c.get(context.Background(), "/artifacts/"+id.String(), &a); err != nil {
		return 0, err
	}
	return a.HeadNumber, nil
}

// ifMatchHeader names the base version a write is decided against.
func ifMatchHeader(n int) http.Header {
	h := http.Header{}
	h.Set("If-Match", strconv.Quote(strconv.Itoa(n)))
	return h
}

func artifactsRestore(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var label *string
	return gesture(environ, args, stdout, stderr, "restore", 2, "the artifact and the version to restore",
		func(c *nodeClient, pos []string, key string) (string, error) {
			id, err := artifactID(pos[0])
			if err != nil {
				return "", err
			}
			n, err := strconv.Atoi(pos[1])
			if err != nil || n < 1 {
				return "", fmt.Errorf("%q is not a version number", pos[1])
			}
			base, err := headOf(c, id)
			if err != nil {
				return "", err
			}
			var answer struct {
				writtenAnswer
				Number int `json:"number"`
			}
			if err := c.write(context.Background(), http.MethodPost,
				"/artifacts/"+id.String()+"/versions/"+strconv.Itoa(n)+"/restore", key,
				ifMatchHeader(base), map[string]string{"label": *label}, &answer); err != nil {
				return "", err
			}
			return outcomeLine(fmt.Sprintf("restored version %d of %s as version %d", n, id,
				answer.Number), answer.writtenAnswer), nil
		}, func(fs *flag.FlagSet) { label = fs.String("label", "", "a label for the new version") })
}

func artifactsLabel(environ config.Source, args []string, stdout, stderr io.Writer) error {
	return gesture(environ, args, stdout, stderr, "label", 3, "the artifact, the version and the label",
		func(c *nodeClient, pos []string, key string) (string, error) {
			id, err := artifactID(pos[0])
			if err != nil {
				return "", err
			}
			if _, err := strconv.Atoi(pos[1]); err != nil {
				return "", fmt.Errorf("%q is not a version number", pos[1])
			}
			var answer writtenAnswer
			if err := c.write(context.Background(), http.MethodPatch,
				"/artifacts/"+id.String()+"/versions/"+pos[1], key, nil,
				map[string]string{"label": pos[2]}, &answer); err != nil {
				return "", err
			}
			return outcomeLine(fmt.Sprintf("labelled version %s of %s %q", pos[1], id, pos[2]), answer), nil
		}, nil)
}

func artifactsShare(environ config.Source, args []string, stdout, stderr io.Writer) error {
	return gesture(environ, args, stdout, stderr, "share", 3, "the artifact, the audience and the role",
		func(c *nodeClient, pos []string, key string) (string, error) {
			id, err := artifactID(pos[0])
			if err != nil {
				return "", err
			}
			audience, err := artifacts.ParseAudience(pos[1])
			if err != nil {
				return "", fmt.Errorf("the audience is seat:<handle>, unit:<key> or company: %w", err)
			}
			role := artifacts.Role(pos[2])
			if !slices.Contains(artifacts.ShareableRoles, role) {
				return "", fmt.Errorf("the role is one of %v: ownership moves only by transfer",
					artifacts.ShareableRoles)
			}
			var answer writtenAnswer
			if err := c.write(context.Background(), http.MethodPut, "/artifacts/"+id.String()+"/shares",
				key, nil, map[string]any{"audience": audience, "role": role}, &answer); err != nil {
				return "", err
			}
			return outcomeLine(fmt.Sprintf("shared %s with %s as %s", id, audienceText(audience), role),
				answer), nil
		}, nil)
}

func artifactsUnshare(environ config.Source, args []string, stdout, stderr io.Writer) error {
	return gesture(environ, args, stdout, stderr, "unshare", 2, "the artifact and the audience",
		func(c *nodeClient, pos []string, key string) (string, error) {
			id, err := artifactID(pos[0])
			if err != nil {
				return "", err
			}
			audience, err := artifacts.ParseAudience(pos[1])
			if err != nil {
				return "", fmt.Errorf("the audience is seat:<handle>, unit:<key> or company: %w", err)
			}
			var answer writtenAnswer
			if err := c.write(context.Background(), http.MethodDelete,
				"/artifacts/"+id.String()+"/shares/"+audienceText(audience), key, nil, nil,
				&answer); err != nil {
				return "", err
			}
			return outcomeLine(fmt.Sprintf("withdrew %s's share of %s", audienceText(audience), id),
				answer), nil
		}, nil)
}

// statusGesture is a write on an artifact that names nothing beyond it.
func statusGesture(environ config.Source, args []string, stdout, stderr io.Writer, name, method,
	suffix, done string) error {

	return gesture(environ, args, stdout, stderr, name, 1, "the artifact",
		func(c *nodeClient, pos []string, key string) (string, error) {
			id, err := artifactID(pos[0])
			if err != nil {
				return "", err
			}
			var answer writtenAnswer
			if err := c.write(context.Background(), method, "/artifacts/"+id.String()+suffix, key,
				nil, nil, &answer); err != nil {
				return "", err
			}
			return outcomeLine(done+" "+id.String(), answer), nil
		}, nil)
}

func artifactsTrash(environ config.Source, args []string, stdout, stderr io.Writer) error {
	return statusGesture(environ, args, stdout, stderr, "trash", http.MethodDelete, "",
		"moved to the trash:")
}

func artifactsUntrash(environ config.Source, args []string, stdout, stderr io.Writer) error {
	return statusGesture(environ, args, stdout, stderr, "untrash", http.MethodPost, "/restore",
		"restored from the trash:")
}

// artifactsPurge destroys an artifact for good. The id is asked twice, the
// second time as -confirm, because nothing undoes it.
func artifactsPurge(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var confirm *string
	return gesture(environ, args, stdout, stderr, "purge", 1, "the artifact",
		func(c *nodeClient, pos []string, key string) (string, error) {
			id, err := artifactID(pos[0])
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(*confirm) != id.String() {
				return "", fmt.Errorf("a purge destroys %s and its data for good: repeat its id as "+
					"-confirm %s", id, id)
			}
			var answer writtenAnswer
			if err := c.write(context.Background(), http.MethodPost, "/artifacts/"+id.String()+"/purge",
				key, nil, nil, &answer); err != nil {
				return "", err
			}
			return outcomeLine("purged "+id.String(), answer), nil
		}, func(fs *flag.FlagSet) {
			confirm = fs.String("confirm", "", "the artifact's id again: nothing undoes a purge")
		})
}

// ---- publish ------------------------------------------------------------- //

// localFile is one file a publish declares.
type localFile struct {
	path  string // the published path, slash-separated
	disk  string // where it is on this machine
	hash  objstore.Hash
	size  int64
	bytes []byte
}

// collectFiles is every file a publish of root sends: root itself, or every
// regular file under a directory, a dotted name (`.git`, `.DS_Store`) left out
// with everything beneath it.
func collectFiles(root string) ([]localFile, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	var files []localFile
	add := func(published, disk string) error {
		raw, err := os.ReadFile(disk) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		files = append(files, localFile{path: published, disk: disk,
			hash: objstore.Hash(hex.EncodeToString(sum[:])), size: int64(len(raw)), bytes: raw})
		return nil
	}
	if !info.IsDir() {
		return files, add(filepath.Base(root), root)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return add(filepath.ToSlash(rel), p)
	})
	if len(files) == 0 && err == nil {
		err = fmt.Errorf("%s holds no file to publish", root)
	}
	return files, err
}

// entryFor is the entry a publish of files names: the one asked for, else the
// one default entry present (the kind's own defaults when -kind names one,
// every kind's otherwise, from the kind table), else the only file. Two
// defaults present is refused rather than picked between: which file a
// directory opens on, and so what kind it is, is the author's to say.
func entryFor(files []localFile, asked string, kind artifacts.Kind) (string, error) {
	if asked != "" {
		for _, f := range files {
			if f.path == asked {
				return asked, nil
			}
		}
		return "", fmt.Errorf("-entry %s is not one of the files published", asked)
	}
	candidates := artifacts.DefaultEntries()
	if kind != "" {
		spec, _ := kind.Spec()
		candidates = spec.Entry.Defaults
	}
	var found []string
	for _, candidate := range candidates {
		if slices.ContainsFunc(files, func(f localFile) bool { return f.path == candidate }) {
			found = append(found, candidate)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return "", fmt.Errorf("the files hold %s: name the entry with -entry, or the kind "+
			"with -kind", strings.Join(found, " and "))
	case len(files) == 1:
		return files[0].path, nil
	case len(candidates) == 0:
		return "", fmt.Errorf("name the entry with -entry: a %s opens on no default file", kind)
	}
	return "", fmt.Errorf("name the entry with -entry: there is no %s",
		strings.Join(candidates, " or "))
}

// kindFor is the kind a new artifact is created as: the one -kind names, else
// the kind whose entry the entry's type may be, from the kind table.
func kindFor(asked artifacts.Kind, entry string) (artifacts.Kind, error) {
	if asked != "" {
		return asked, nil
	}
	typ, ok := mediatype.For(entry)
	if ok {
		if kind, ok := artifacts.KindFor(entry, typ.Essence()); ok {
			return kind, nil
		}
	}
	return "", fmt.Errorf("name the kind with -kind: no kind opens on %s", entry)
}

// kindNames lists every kind this build serves, for the flags' help.
func kindNames() string {
	names := make([]string, len(artifacts.Kinds))
	for i, k := range artifacts.Kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// fileSpec is one file of a publish as the surface takes it.
type fileSpec struct {
	Path   string        `json:"path"`
	Upload bool          `json:"upload,omitempty"`
	Carry  objstore.Hash `json:"carry,omitempty"`
}

func artifactsPublish(environ config.Source, args []string, stdout, stderr io.Writer) error {
	pos, rest, err := positionals(args, 1, "the directory or the file to publish")
	if err != nil {
		return err
	}
	var target, title, kind, entry, label, contract, item, opID *string
	var force *bool
	var caps capabilitiesFlag
	client, err := nodeClientFor(environ, rest, "artifacts publish", stderr, func(fs *flag.FlagSet) {
		target = fs.String("artifact", "", "publish a new version of this artifact; empty creates one")
		title = fs.String("title", "", "the title of a new artifact (default: the entry's name)")
		kind = fs.String("kind", "", kindNames()+" (default: from the entry)")
		entry = fs.String("entry", "", "the file the artifact opens on (default: "+
			strings.Join(artifacts.DefaultEntries(), ", ")+")")
		label = fs.String("label", "", "a label for the version")
		contract = fs.String("contract", "", "the runtime contract the page pins: latest or an exact version")
		fs.Var(&caps, "capabilities", "what the page uses, comma-separated: "+
			capabilityNames()+"; none declares none (default: the head's, none for a new artifact)")
		item = fs.String("work-item", "", "the work item this is made for, which lists it there")
		force = fs.Bool("force", false, "publish over a head that moved since it was read")
		opID = operationFlag(fs)
	})
	if err != nil {
		return err
	}
	key, err := operation(*opID, "publish")
	if err != nil {
		return err
	}
	files, err := collectFiles(pos[0])
	if err != nil {
		return err
	}
	asked := artifacts.Kind(*kind)
	if asked != "" && !asked.Valid() {
		return fmt.Errorf("-kind %s is not a kind; name one of %s", asked, kindNames())
	}
	entryPath, err := entryFor(files, *entry, asked)
	if err != nil {
		return err
	}
	var created artifacts.Kind
	if *target == "" {
		if created, err = kindFor(asked, entryPath); err != nil {
			return err
		}
	}
	p := publishing{client: client, files: files, key: key, stdout: stdout}
	if err := p.run(*target, entryPath, *title, created, *label, *contract, *item, *force, caps.list); err != nil {
		return unknownOutcome(err, "publish", key)
	}
	return nil
}

// capabilitiesFlag is `-capabilities`. It remembers whether it was named at
// all, because the node reads an absent set as "keep the head's" and an empty
// one as "declare none": a republish that does not name the flag must send
// nothing, or it strips a page of the datastore it reads.
type capabilitiesFlag struct {
	list []artifacts.Capability // nil until the flag is named
}

func (f *capabilitiesFlag) String() string {
	names := make([]string, len(f.list))
	for i, c := range f.list {
		names[i] = string(c)
	}
	return strings.Join(names, ",")
}

// Set parses a comma-separated set. `none`, or an empty value, declares none;
// a name this build does not serve is refused before any byte moves.
func (f *capabilitiesFlag) Set(value string) error {
	list := []artifacts.Capability{}
	if v := strings.TrimSpace(value); v != "" && v != "none" {
		for _, name := range strings.Split(v, ",") {
			c := artifacts.Capability(strings.TrimSpace(name))
			if !c.Valid() {
				return fmt.Errorf("%q is not a capability; name some of %s, or none", name, capabilityNames())
			}
			if !slices.Contains(list, c) {
				list = append(list, c)
			}
		}
	}
	f.list = list
	return nil
}

// capabilityNames lists every capability this build serves, for the flag's
// help and its refusal.
func capabilityNames() string {
	names := make([]string, len(artifacts.Capabilities))
	for i, c := range artifacts.Capabilities {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}

// publishing is one publish in flight.
type publishing struct {
	client *nodeClient
	files  []localFile
	key    string
	stdout io.Writer
}

// run declares the files, sends the ones the artifact does not hold, and
// publishes the version.
func (p publishing) run(target, entry, title string, kind artifacts.Kind, label, contract, item string, force bool,
	caps []artifacts.Capability) error {

	ctx := context.Background()
	entries := make([]artifacts.UploadEntry, len(p.files))
	for i, f := range p.files {
		entries[i] = artifacts.UploadEntry{Path: f.path, SHA256: f.hash, Size: f.size}
	}
	intentPath, base := "/artifacts/uploads", 0
	var id artifacts.ID
	if target != "" {
		var err error
		if id, err = artifactID(target); err != nil {
			return err
		}
		if base, err = headOf(p.client, id); err != nil {
			return err
		}
		intentPath = "/artifacts/" + id.String() + "/uploads"
	}
	// THE INTENT FIRST, under the invocation's key, so a retry is answered
	// the same upload; the publish under a step of it.
	var intent artifacts.UploadIntent
	if err := p.client.write(ctx, http.MethodPost, intentPath, p.key, nil,
		artifacts.UploadRequest{Entries: entries}, &intent); err != nil {
		return err
	}
	if err := p.transfer(ctx, intent); err != nil {
		return err
	}
	carried := map[string]bool{}
	for _, c := range intent.Carried {
		carried[c] = true
	}
	specs := make([]fileSpec, len(p.files))
	for i, f := range p.files {
		if carried[f.path] {
			specs[i] = fileSpec{Path: f.path, Carry: f.hash}
		} else {
			specs[i] = fileSpec{Path: f.path, Upload: true}
		}
	}
	publishKey := statelog.StepOpID(p.key, "publish")
	var answer struct {
		writtenAnswer
		Artifact string `json:"artifact"`
		Number   int    `json:"number"`
		Warnings []struct {
			Path    string `json:"path"`
			Line    int    `json:"line"`
			Message string `json:"message"`
		} `json:"warnings"`
	}
	body := map[string]any{"upload": intent.Upload, "files": specs, "entry": entry,
		"label": label, "contract": contract, "work_item": item}
	if caps != nil {
		// Absent keeps the head's capabilities; see [capabilitiesFlag].
		body["capabilities"] = caps
	}
	if target == "" {
		if title == "" {
			title = strings.TrimSuffix(path.Base(entry), path.Ext(entry))
		}
		body["title"], body["kind"] = title, kind
		if err := p.client.write(ctx, http.MethodPost, "/artifacts", publishKey, nil, body,
			&answer); err != nil {
			return err
		}
	} else {
		body["force"] = force
		var header http.Header
		if !force {
			header = ifMatchHeader(base)
		}
		if err := p.client.write(ctx, http.MethodPost, "/artifacts/"+id.String()+"/versions",
			publishKey, header, body, &answer); err != nil {
			return err
		}
	}
	fmt.Fprintln(p.stdout, outcomeLine(fmt.Sprintf("published %s version %d (%d sent, %d carried)",
		answer.Artifact, answer.Number, len(intent.Transfers), len(intent.Carried)), answer.writtenAnswer))
	for _, w := range answer.Warnings {
		fmt.Fprintf(p.stdout, "  warning: %s:%d: %s\n", w.Path, w.Line, w.Message)
	}
	return nil
}

// transfer sends every file the intent did not find in the artifact, the way
// the intent says to: straight to the bucket on its signed URL, or through the
// node.
func (p publishing) transfer(ctx context.Context, intent artifacts.UploadIntent) error {
	byPath := map[string]localFile{}
	for _, f := range p.files {
		byPath[f.path] = f
	}
	for _, t := range intent.Transfers {
		f, ok := byPath[t.Path]
		if !ok {
			return fmt.Errorf("the node asked for %s, which this publish did not declare", t.Path)
		}
		if err := p.put(ctx, t, f); err != nil {
			return fmt.Errorf("send %s: %w", t.Path, err)
		}
	}
	return nil
}

// put sends one file's bytes as its transfer says.
func (p publishing) put(ctx context.Context, t artifacts.Transfer, f localFile) error {
	// A FILE'S OWN PACE, the object store's figure, beside the ordinary
	// ceiling: a large file on a slow link is not a wrong address.
	limit := nodeRequestTimeout + objstore.PaceFor(f.size)
	if !t.Direct {
		header := http.Header{}
		for k, v := range t.Headers {
			header.Set(k, v)
		}
		return p.client.patiently(limit).sendRaw(ctx, t.Method, t.URL, header, f.bytes)
	}
	req, err := http.NewRequestWithContext(ctx, t.Method, t.URL, bytes.NewReader(f.bytes))
	if err != nil {
		return err
	}
	req.ContentLength = f.size
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := httpx.Client(limit).Do(req)
	if err != nil {
		return noAnswer{fmt.Errorf("reach the object store: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("the object store answered %d: %s", resp.StatusCode,
			strings.TrimSpace(string(detail)))
	}
	return nil
}

// sendRaw is [nodeClient.send] for a body that is a file's bytes rather than
// JSON.
func (c *nodeClient) sendRaw(ctx context.Context, method, target string, header http.Header,
	body []byte) error {

	req, err := http.NewRequestWithContext(ctx, method, c.base+target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	req.ContentLength = int64(len(body))
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return noAnswer{fmt.Errorf("reach the node at %s: %w", c.base, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxNodeResponseBytes))
	if err != nil {
		return noAnswer{fmt.Errorf("reading the node's answer to %s: %w", target, err)}
	}
	if resp.StatusCode/100 != 2 {
		if engineCode(answer) == "" {
			return notTheNode(target, resp.StatusCode, resp.Header.Get("Content-Type"), answer)
		}
		return nodeError(resp.StatusCode, answer, c.token != "")
	}
	return nil
}
