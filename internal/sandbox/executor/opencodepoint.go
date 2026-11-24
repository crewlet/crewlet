package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
	"github.com/crewlet/crewlet/internal/words"
)

// openCodePointWait bounds the read of a box's OpenCode point: two standalone
// servers started (model.list and config.get, each 0.5 to 1.3 s on two loaded
// cores, measured on the four releases of [openCodeCountReleases]) and a login
// imported first, on [openCodeListenWait]'s ground, the room a contended box
// needs. Past it the point is unread and the continuation told as a summary.
const openCodePointWait = openCodeListenWait

// openCodePointAnswerBytes bounds each answer the read keeps: a box lists the
// models of the providers whose credentials it holds, 7 to 97 KiB measured
// with one provider's key, and a box's documents are a few KiB. Past it the
// point is unread rather than read cut.
const openCodePointAnswerBytes = 4 << 20

// openCodePointCleanupWait bounds the removal of the read's answers from the
// box, a `rm -rf` of a directory of three files.
const openCodePointCleanupWait = 10 * time.Second

// The share of a model's limit OpenCode holds back below its point where no
// buffer is set, and the floor it holds back at least once the limit is twice
// it (read in 2.3.2's bundle, measured at its boundaries on 2.3.2).
const (
	openCodeShare      = 0.15
	openCodeShareFloor = 16000
)

// readBoxPoint reads, in box, where its OpenCode compacts a continued
// session's first request ([Runner.ReadBoxPoint]): the package doc's section on
// the first request judged in its box says what it reads and why. Three
// answers, as [Runner.CheckCarry]'s: a point, a mark, and an error where the
// box could not be asked.
func (OpenCode) readBoxPoint(ctx context.Context, box sandbox.Sandbox,
	ask sandbox.PointAsk) (sandbox.BoxPoint, error) {

	if provider, _ := sandbox.ModelProvider(ask.Model); provider == "" {
		return pointMarked(sandbox.PointNoModel, fmt.Sprintf("the launch's model %q names no provider",
			ask.Model)), nil
	}
	if ask.Release != "" {
		if point, marked := releaseMarked(ask.Release, ask.Writer); marked {
			return point, nil
		}
	}
	paths := PathsFor(box)
	dir := paths.WorkDir() + "/opencode-point"
	env := make(map[string]string, len(ask.Env))
	maps.Copy(env, ask.Env)
	delete(env, "OPENCODE_CONFIG_CONTENT")
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(openCodePointScript(paths, dir, ask)),
		sandbox.ExecOptions{Env: env, TimeoutSec: openCodePointWait.Seconds()})
	defer clearPointAnswers(ctx, box, dir)
	if err != nil {
		return sandbox.BoxPoint{}, fmt.Errorf("executor: reading the box's OpenCode point: %w", err)
	}
	if res.ExitCode != 0 {
		return pointMarked(sandbox.PointUnread, fmt.Sprintf("the read of the box's OpenCode point exited %d",
			res.ExitCode)), nil
	}
	answers := openCodeAnswers{release: ask.Release, listing: res.Stdout}
	files := []struct {
		name string
		into *[]byte
	}{{"models", &answers.models}, {"config", &answers.config}}
	if ask.Release == "" {
		files = append(files, struct {
			name string
			into *[]byte
		}{"version", &answers.version})
	}
	for _, f := range files {
		if *f.into, err = pointAnswer(ctx, box, dir+"/"+f.name); err != nil {
			return pointMarked(sandbox.PointUnread, fmt.Sprintf("the box's %s answer could not be read: %v",
				f.name, err)), nil
		}
	}
	return openCodePointOf(ask, answers), nil
}

// openCodePointScript is the one exec the read runs ([Runner.ReadBoxPoint]):
// the login imported where the box holds one, the release asked where the
// launch has none, model.list and config.get each answered into a file (the
// exec's stdout carries the read's own lines, and each answer is read back
// apart, held to its bound), the plugin
// folders listed. Every line it prints is the read's: `status`, `dir`,
// `plugin`.
func openCodePointScript(paths Paths, dir string, ask sandbox.PointAsk) string {
	q := shellword.Quote
	doc := q(paths.CredentialDocument())
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`PATH=%s:"$PATH"; export PATH; unset OPENCODE_CONFIG_CONTENT`, q(paths.BinDir()))
	line(`d=%s; rm -rf "$d" && mkdir -p "$d" || exit 70`, q(dir))
	line(`if [ -s %s ]; then opencode auth import --standalone < %s > /dev/null 2>&1; echo "status import $?"; fi`,
		doc, doc)
	if ask.Release == "" {
		line(`opencode --version < /dev/null > "$d/version" 2> /dev/null; echo "status version $?"`)
	}
	line(`opencode api --standalone model.list < /dev/null > "$d/models" 2> /dev/null; echo "status models $?"`)
	line(`opencode api --standalone config.get < /dev/null > "$d/config" 2> /dev/null; echo "status config $?"`)
	line(`c=${XDG_CONFIG_HOME:-$HOME/.config}/opencode`)
	line(`for g in "$c" ${OPENCODE_CONFIG_DIR:+"$OPENCODE_CONFIG_DIR"}; do echo "dir $g"; ` +
		`for p in "$g"/plugins/* "$g"/plugin/*; do [ -e "$p" ] && echo "plugin $p"; done; done`)
	line(`w=$PWD; while :; do echo "dir $w/.opencode"; ` +
		`for p in "$w"/.opencode/plugins/* "$w"/.opencode/plugin/*; do [ -e "$p" ] && echo "plugin $p"; done; ` +
		`[ "$w" = / ] && break; w=$(dirname "$w"); done`)
	line(`exit 0`)
	return b.String()
}

// pointAnswer is the answer the read left at file, held to
// [openCodePointAnswerBytes].
func pointAnswer(ctx context.Context, box sandbox.Sandbox, file string) ([]byte, error) {
	body, err := box.OpenFile(ctx, file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(body, openCodePointAnswerBytes+1))
	switch {
	case err != nil:
		return nil, err
	case len(raw) > openCodePointAnswerBytes:
		return nil, fmt.Errorf("it is past %d bytes", openCodePointAnswerBytes)
	}
	return raw, nil
}

// clearPointAnswers removes the read's answers from the box: config.get
// answers a document's `{env:…}` and `{file:…}` references resolved, which
// the box's agent should not find left lying in the work directory. A
// teardown, so on a context of its own ([context.WithoutCancel]); a removal
// that fails leaves them to the next read's, which starts by removing them.
func clearPointAnswers(ctx context.Context, box sandbox.Sandbox, dir string) {
	_, _ = box.Exec(context.WithoutCancel(ctx), "rm -rf "+shellword.Quote(dir),
		sandbox.ExecOptions{TimeoutSec: openCodePointCleanupWait.Seconds()})
}

// openCodeAnswers is what the read brought back: the box's release (the
// launch's, or the version answer), model.list's and config.get's answers,
// and the lines the script printed.
type openCodeAnswers struct {
	release        string
	version        []byte
	models, config []byte
	listing        string
}

// pointMarked is no point, for mark, why redacted.
func pointMarked(mark sandbox.PointMark, why string) sandbox.BoxPoint {
	return sandbox.BoxPoint{Mark: mark, Why: redact.Secrets(why)}
}

// releaseMarked is no point where release is outside the set this build read
// or is not writer's, the release that wrote the session: a set, never a
// range, and a pair only where it is one release ([openCodeCountRead]).
func releaseMarked(release, writer string) (sandbox.BoxPoint, bool) {
	release, writer = strings.TrimSpace(release), strings.TrimSpace(writer)
	switch {
	case !openCodeCountRead(release):
		return pointMarked(sandbox.PointRelease, fmt.Sprintf("the box runs OpenCode %q, outside the releases "+
			"whose point this build reads", release)), true
	case release != writer:
		return pointMarked(sandbox.PointPair, fmt.Sprintf("the box runs OpenCode %s and the session was "+
			"written by %q: no pair of differing releases has been measured", release, writer)), true
	}
	return sandbox.BoxPoint{}, false
}

// openCodePointOf decides the point from what the read brought back, for the
// launch's model: every mark it finds, the first of [sandbox.PointMarks] kept;
// else the point by the box's release's rule, or Never where compaction is
// off.
func openCodePointOf(ask sandbox.PointAsk, a openCodeAnswers) sandbox.BoxPoint {
	release := strings.TrimSpace(a.release)
	if release == "" {
		release = releaseOf(string(a.version))
	}
	if point, marked := releaseMarked(release, ask.Writer); marked {
		return point
	}
	listing, err := readPointListing(a.listing)
	if err != nil {
		return pointMarked(sandbox.PointUnread, "the read's own lines did not read: "+err.Error())
	}
	if why := listing.failed(); why != "" {
		return pointMarked(sandbox.PointUnread, why)
	}
	docs, err := readOpenCodeDocuments(a.config)
	if err != nil {
		return pointMarked(sandbox.PointUnread, "the box's config.get answer did not read: "+err.Error())
	}
	models, err := readOpenCodeModels(a.models)
	if err != nil {
		return pointMarked(sandbox.PointUnread, "the box's model.list answer did not read: "+err.Error())
	}
	provider, model := sandbox.ModelProvider(ask.Model)
	found := map[sandbox.PointMark]string{}
	mark := func(m sandbox.PointMark, why string) {
		if _, ok := found[m]; !ok {
			found[m] = why
		}
	}
	point := sandbox.BoxPoint{Release: release, Anchored: true}
	point.Plugins = slices.Concat(docs.plugins, listing.plugins)
	if len(point.Plugins) > 0 {
		mark(sandbox.PointPlugin, "the box loads a plugin other than the engine's own, which can compact the "+
			"session or add to it before the first request: "+words.And(point.Plugins))
	}
	for _, dir := range docs.directories {
		if !listing.dirs[path.Clean(dir)] {
			mark(sandbox.PointPlugin, "the box reads a configuration directory the read did not list for "+
				"plugins: "+dir)
		}
	}
	if docs.contextManagement[provider] {
		mark(sandbox.PointContextManagement, "a box document sets "+provider+"'s contextManagement option, "+
			"which edits the conversation server-side")
	}
	if provider == "" {
		mark(sandbox.PointNoModel, fmt.Sprintf("the launch's model %q names no provider", ask.Model))
	}
	record, listed := models[openCodeModelKey{provider, model}]
	switch {
	case provider == "":
	case !listed:
		mark(sandbox.PointAbsent, fmt.Sprintf("the box's catalog lists no %s/%s", provider, model))
	case record.Compaction.Type == "native":
		mark(sandbox.PointNative, fmt.Sprintf("%s/%s compacts natively, by its provider's own rule",
			provider, model))
	case record.contextManagement:
		mark(sandbox.PointContextManagement, fmt.Sprintf("%s/%s's settings carry the contextManagement "+
			"option, which edits the conversation server-side", provider, model))
	}
	auto := docs.auto == nil || *docs.auto
	limit := record.limit()
	if listed && auto {
		switch {
		case !docs.statesLimit(provider, model, record):
			mark(sandbox.PointDefaulted, fmt.Sprintf("nothing states %s/%s's limit, so OpenCode holds it to "+
				"its default of 200,000 tokens, which may not be its server's window", provider, model))
		case limit <= 0:
			mark(sandbox.PointNoLimit, fmt.Sprintf("%s/%s's limit reads as zero with compaction on: only its "+
				"provider's refusal compacts it", provider, model))
		}
	}
	for _, m := range sandbox.PointMarks {
		if why, ok := found[m]; ok {
			point.Mark, point.Why = m, redact.Secrets(why)
			return point
		}
	}
	if !auto {
		point.Never = true
	} else if point.Tokens, err = openCodePointAt(limit, docs.buffer); err != nil {
		return pointMarked(sandbox.PointUnread, "the box's compaction buffer did not read: "+err.Error())
	}
	return point
}

// openCodePointAt is the point OpenCode compacts at for a model of limit L
// (above zero) under buffer, nil where no document sets one: L less a buffer
// below L, a `"N%"` buffer read as round(L·N/100); else L less the larger of
// [openCodeShare] of L and [openCodeShareFloor] once L is twice it.
func openCodePointAt(limit int64, buffer json.RawMessage) (int64, error) {
	if buffer != nil {
		held, err := openCodeBufferOf(limit, buffer)
		if err != nil {
			return 0, err
		}
		if held < limit {
			return limit - held, nil
		}
	}
	floor := int64(0)
	if limit >= 2*openCodeShareFloor {
		floor = openCodeShareFloor
	}
	return limit - max(int64(math.Floor(float64(limit)*openCodeShare)), floor), nil
}

// openCodeBufferOf is the tokens a buffer holds back from a limit of L: an
// integer, or a `"N%"` of L rounded half up as JavaScript's Math.round does.
// Anything else does not read, since a release whose schema refuses it starts
// no run on it.
func openCodeBufferOf(limit int64, raw json.RawMessage) (int64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("a buffer of %d tokens", n)
		}
		return n, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, fmt.Errorf("the buffer %s is no integer this release takes", raw)
	}
	digits, percent := strings.CutSuffix(text, "%")
	share, err := strconv.ParseFloat(digits, 64)
	if !percent || err != nil || share < 0 || math.IsInf(share, 0) || math.IsNaN(share) {
		return 0, fmt.Errorf("the buffer %q is no share of the limit", text)
	}
	return int64(math.Floor(float64(limit)*share/100 + 0.5)), nil
}

// pointListing is the lines the read's script printed ([openCodePointScript]).
type pointListing struct {
	statuses map[string]int
	dirs     map[string]bool
	plugins  []string
}

// readPointListing reads the script's lines; one it does not know does not
// read, since a path printed with a newline in it says nothing whole.
func readPointListing(text string) (pointListing, error) {
	l := pointListing{statuses: map[string]int{}, dirs: map[string]bool{}}
	for text != "" {
		var line string
		line, text, _ = strings.Cut(text, "\n")
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "status":
			name, code, _ := strings.Cut(value, " ")
			n, err := strconv.Atoi(code)
			if err != nil {
				return l, fmt.Errorf("a status %q", value)
			}
			l.statuses[name] = n
		case "dir":
			l.dirs[path.Clean(value)] = true
		case "plugin":
			l.plugins = append(l.plugins, value)
		case "":
		default:
			return l, fmt.Errorf("a line %q", firstLine(line, transcriptDetailLimit))
		}
	}
	return l, nil
}

// failed is why a step of the read failed, "" where none did: the login
// refused, or the release, the catalog or the documents not answered.
func (l pointListing) failed() string {
	for _, step := range []string{"import", "version", "models", "config"} {
		if code, ok := l.statuses[step]; ok && code != 0 {
			return fmt.Sprintf("the box's OpenCode failed its %s with exit %d", step, code)
		}
	}
	for _, step := range []string{"models", "config"} {
		if _, ok := l.statuses[step]; !ok {
			return "the read did not say its " + step + " step ran"
		}
	}
	return ""
}

// openCodeDocuments is what the read keeps of config.get's documents, each
// read for the keys the point is decided by and dropped: never a value a
// document's reference resolved.
type openCodeDocuments struct {
	// limits and releaseDates say, by provider and model, whether some
	// document states its limit, and its release date.
	limits, releaseDates map[openCodeModelKey]bool
	// auto and buffer are each the highest document's that sets it; nil
	// where none does, so compaction is on and the release's share holds.
	auto   *bool
	buffer json.RawMessage
	// plugins is every plugin a document adds, by package or name.
	plugins []string
	// directories is every configuration directory config.get names.
	directories []string
	// contextManagement is every provider a document sets the option on.
	contextManagement map[string]bool
}

// openCodeModelKey names a model as OpenCode lists it.
type openCodeModelKey struct{ provider, model string }

// readOpenCodeDocuments reads config.get's answer, lowest document first.
func readOpenCodeDocuments(raw []byte) (openCodeDocuments, error) {
	var entries []struct {
		Type string                     `json:"type"`
		Path string                     `json:"path"`
		Info map[string]json.RawMessage `json:"info"`
	}
	if err := strictJSON(raw, &entries); err != nil {
		return openCodeDocuments{}, err
	}
	d := openCodeDocuments{limits: map[openCodeModelKey]bool{}, releaseDates: map[openCodeModelKey]bool{},
		contextManagement: map[string]bool{}}
	for _, e := range entries {
		switch e.Type {
		case "directory":
			d.directories = append(d.directories, e.Path)
			continue
		case "document":
		default:
			return d, fmt.Errorf("an entry of type %q", e.Type)
		}
		if err := d.read(e.Info); err != nil {
			return d, err
		}
	}
	return d, nil
}

// read reads one document's keys into d, over what a lower one set.
func (d *openCodeDocuments) read(info map[string]json.RawMessage) error {
	if raw, ok := info["compaction"]; ok {
		var c struct {
			Auto   *bool           `json:"auto"`
			Buffer json.RawMessage `json:"buffer"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("compaction: %w", err)
		}
		if c.Auto != nil {
			d.auto = c.Auto
		}
		if c.Buffer != nil && !bytes.Equal(c.Buffer, []byte("null")) {
			d.buffer = c.Buffer
		}
	}
	for _, key := range []string{"plugins", "plugin"} {
		raw, ok := info[key]
		if !ok {
			continue
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		for _, entry := range entries {
			var name string
			if json.Unmarshal(entry, &name) != nil {
				var named struct {
					Package string `json:"package"`
					Name    string `json:"name"`
				}
				if err := json.Unmarshal(entry, &named); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				name = cmpOr(named.Package, named.Name, "a plugin it names by no package")
			}
			// A REMOVAL, `-<id>`, adds nothing the box would run.
			if !strings.HasPrefix(name, "-") {
				d.plugins = append(d.plugins, name)
			}
		}
	}
	raw, ok := info["providers"]
	if !ok {
		return nil
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &providers); err != nil {
		return fmt.Errorf("providers: %w", err)
	}
	for provider, raw := range providers {
		if holdsKey(raw, "contextManagement") {
			d.contextManagement[provider] = true
		}
		var p struct {
			Models map[string]struct {
				Limit       map[string]json.RawMessage `json:"limit"`
				ReleaseDate json.RawMessage            `json:"releaseDate"`
			} `json:"models"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("providers.%s: %w", provider, err)
		}
		for model, m := range p.Models {
			key := openCodeModelKey{provider, model}
			if m.Limit["context"] != nil || m.Limit["input"] != nil {
				d.limits[key] = true
			}
			if m.ReleaseDate != nil {
				d.releaseDates[key] = true
			}
		}
	}
	return nil
}

// cmpOr is the first of values that is not "".
func cmpOr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// statesLimit reports whether model's limit is stated rather than OpenCode's
// default: a document states it, or the catalog does, which its record's
// release time says where no document set that time.
func (d openCodeDocuments) statesLimit(provider, model string, record openCodeModel) bool {
	key := openCodeModelKey{provider, model}
	switch {
	case d.limits[key]:
		return true
	case record.Time.Released > 0 && !d.releaseDates[key]:
		return true
	}
	return false
}

// openCodeModel is what the read keeps of one model.list record.
type openCodeModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Limit      struct {
		Context int64 `json:"context"`
		Input   int64 `json:"input"`
	} `json:"limit"`
	Time struct {
		Released float64 `json:"released"`
	} `json:"time"`
	Compaction struct {
		Type string `json:"type"`
	} `json:"compaction"`
	Settings json.RawMessage `json:"settings"`
	// contextManagement says the record's settings carry the option.
	contextManagement bool
}

// limit is the model's limit as OpenCode reads it: the smaller of its input
// and context limits where both are set, else its input limit, else its
// context limit.
func (m openCodeModel) limit() int64 {
	in, ctx := m.Limit.Input, m.Limit.Context
	if in > 0 && ctx > 0 {
		return min(in, ctx)
	}
	if in > 0 {
		return in
	}
	return ctx
}

// readOpenCodeModels reads model.list's answer, by provider and model.
func readOpenCodeModels(raw []byte) (map[openCodeModelKey]openCodeModel, error) {
	var answer struct {
		Data []openCodeModel `json:"data"`
	}
	if err := strictJSON(raw, &answer); err != nil {
		return nil, err
	}
	if answer.Data == nil {
		return nil, errors.New("it lists no models")
	}
	models := make(map[openCodeModelKey]openCodeModel, len(answer.Data))
	for _, m := range answer.Data {
		m.contextManagement = holdsKey(m.Settings, "contextManagement")
		m.Settings = nil
		models[openCodeModelKey{m.ProviderID, m.ID}] = m
	}
	return models, nil
}

// strictJSON decodes raw into v, one value and nothing after it.
func strictJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("it runs on past its value")
	}
	return nil
}

// holdsKey reports whether raw, a JSON value, holds key as an object's key at
// any depth.
func holdsKey(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch v := v.(type) {
		case map[string]any:
			for k, inner := range v {
				if k == key || walk(inner) {
					return true
				}
			}
		case []any:
			for _, inner := range v {
				if walk(inner) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}
