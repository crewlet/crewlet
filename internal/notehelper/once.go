package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// THE ONE-SHOT CORE: a hook of the harness's own runs the helper once per
// firing, and the helper takes the run's notes once over the bridge
// (steer_take, the tool a hook of the run calls on its MCP server) or reports
// one skill load, and exits. Its logic names no harness: an adapter (oneShots) reads its
// harness's hook input and writes the answer in its harness's shape.

// oneShots is every adapter the helper runs once per firing, by -adapter.
var oneShots = map[string]oneShot{
	"muse-hook":         {run: runMuseHook, config: true},
	"grok-hook":         {run: runGrokHook, config: true},
	"checkout-launch":   {run: runCheckoutLaunch, config: true},
	"codex-nested":      {run: runCodexNested, config: true},
	"claude-nested":     {run: runClaudeNested, config: true},
	"gemini-nested":     {run: runGeminiNested, config: true},
	"qwen-nested":       {run: runQwenNested, config: true},
	"codex-bridge":      {run: runCodexBridge, config: true},
	"grok-nested":       {run: runGrokNested, config: true},
	"pi-nested":         {run: runPiNested, config: true},
	"kimi-nested":       {run: runKimiNested, config: true},
	"opencode-nested":   {run: runOpenCodeNested, config: true},
	"codex-renew":       {run: runCodexRenew, config: true},
	"codex-materialize": {run: runCodexMaterialize},
	"copilot-launch":    {run: runCopilotLaunch, config: true},
	"copilot-nested":    {run: runCopilotNested, config: true},
}

// oneShot is one adapter run once: its run, and whether it reads -config,
// which it then requires; one that reads none refuses it.
type oneShot struct {
	run func(ctx context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
		getenv func(string) string) int
	config bool
}

// runOnce runs args as a one-shot adapter's firing, and is its exit status
// and true; false, running nothing, where -adapter names no one-shot adapter,
// for [run] to read the command line.
func runOnce(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader,
	stdout, stderr io.Writer) (int, bool) {

	var f flags
	set := flag.NewFlagSet("notehelper", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&f.adapter, "adapter", "", "")
	set.StringVar(&f.config, "config", "", "")
	set.BoolVar(&f.skillLoads, "skill-loads", false, "")
	set.BoolVar(&f.loadsOnly, "loads-only", false, "")
	set.IntVar(&f.slot, "slot", 0, "")
	set.StringVar(&f.threadFile, "thread-file", "", "")
	f.digests = digestFlag{}
	set.Var(f.digests, "skill-digest", "")
	set.StringVar(&f.rollout, "rollout", "", "")
	set.StringVar(&f.sessions, "sessions", "", "")
	set.StringVar(&f.thread, "thread", "", "")
	set.IntVar(&f.keep, "keep", -1, "")
	set.StringVar(&f.kept, "kept", "", "")
	if set.Parse(args) != nil {
		return 0, false
	}
	once, ok := oneShots[f.adapter]
	if !ok {
		return 0, false
	}
	switch {
	case set.NArg() > 0 || (once.config && f.config == ""):
		fmt.Fprintf(stderr, "notehelper: %s takes -config, the take config's file, and no other arguments\n",
			f.adapter)
		return exitUsage, true
	case !once.config && f.config != "":
		fmt.Fprintf(stderr, "notehelper: %s reads no -config\n", f.adapter)
		return exitUsage, true
	}
	return once.run(ctx, f, stdin, stdout, stderr, getenv), true
}

// takeConfig is what a one-shot run reads from the file -config names: the
// bridge's MCP endpoint and headers, the tool a take calls, the skill route,
// and, for a harness whose skill load is a read of the skill's file (Grok
// Build's), the directory the run's skills are written in. A file, since a
// hook's environment may be scrubbed, and one never on a command line, which
// every process in the box can read.
type takeConfig struct {
	Endpoint  string            `json:"endpoint,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Tool      string            `json:"tool,omitempty"`
	Skills    string            `json:"skills,omitempty"`
	SkillsDir string            `json:"skillsDir,omitempty"`
}

// maxConfig bounds the config file: an endpoint, a few headers and a route.
const maxConfig = 64 << 10

// readTakeConfig reads the config at path.
func readTakeConfig(path string) (takeConfig, error) {
	var c takeConfig
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("the take config could not be opened: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || len(raw) > maxConfig {
		return c, errors.New("the take config could not be read whole")
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, errors.New("the take config is not a JSON object of the take's fields")
	}
	return c, nil
}

// takeField is the field of a take's answer carrying its token: the core
// removes it, so the harness is handed the answer without it, and the adapter
// writes it where the harness keeps it out of its model's view.
const takeField = "crewlet_take"

// maxTake bounds steer_take's answer: one answer of the largest ceiling a
// harness states, with room for the JSON-RPC around it.
const maxTake = 1 << 20

// take calls cfg's steer_take at boundary and is the answer, without its
// token, and the token; both empty for none. Every error leaves nothing
// handed over: a take the harness never received is told again.
func take(ctx context.Context, cfg takeConfig, boundary string) (answer, token string, err error) {
	if cfg.Endpoint == "" || cfg.Tool == "" {
		return "", "", nil
	}
	client := mcpClient{endpoint: cfg.Endpoint, headers: cfg.Headers}
	if err = client.open(ctx); err != nil {
		return "", "", err
	}
	defer client.close(ctx)
	text, err := client.call(ctx, cfg.Tool, map[string]string{"event": boundary, "agent": ""})
	if err != nil || text == "" {
		return "", "", err
	}
	return splitTake(text)
}

// splitTake is text, a take's answer, without [takeField], and that field's
// token. The answer's other fields are handed on as their bytes stand, never
// re-encoded: a harness's stdout ceiling counts them, and encoding/json
// escapes U+2028 and U+2029 even in bytes it only re-emits, six bytes each.
// Its keys come in sorted order.
func splitTake(text string) (answer, token string, err error) {
	var fields map[string]json.RawMessage
	if err = json.Unmarshal([]byte(text), &fields); err != nil {
		return "", "", errors.New("the take's answer is not a JSON object")
	}
	raw, ok := fields[takeField]
	if !ok || json.Unmarshal(raw, &token) != nil || token == "" {
		return "", "", errors.New("the take's answer names no token")
	}
	delete(fields, takeField)
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range slices.Sorted(maps.Keys(fields)) {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		b.Write(name)
		b.WriteByte(':')
		b.Write(fields[key])
	}
	b.WriteByte('}')
	return b.String(), token, nil
}

// reportLoad posts skill, a load the harness made, to cfg's skill route.
func reportLoad(ctx context.Context, cfg takeConfig, skill string) error {
	if cfg.Skills == "" || skill == "" {
		return nil
	}
	body, err := json.Marshal(map[string]string{"skill": skill})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Skills, bytes.NewReader(body))
	if err != nil {
		return scrubURL(err, cfg.Skills, "the skill route")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirects.Do(req)
	if err != nil {
		return scrubURL(err, cfg.Skills, "the skill route")
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the skill route answered %s", resp.Status)
	}
	return nil
}

// noRedirects is every one-shot request's client: the bridge never
// redirects, and a redirect followed would carry the token in its path or
// headers to wherever it pointed.
var noRedirects = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// scrubURL is err with secret, an address holding the run's token, named as
// name instead.
func scrubURL(err error, secret, name string) error {
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, name))
}

// mcpClient is the least of an MCP client over streamable HTTP a take needs:
// initialize, initialized, one tools/call, and the session's end.
type mcpClient struct {
	endpoint string
	headers  map[string]string
	session  string
}

// open initializes the session.
func (c *mcpClient) open(ctx context.Context) error {
	_, err := c.rpc(ctx, 1, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "crewlet-notehelper", "version": "1"}}})
	if err != nil {
		return err
	}
	_, err = c.rpc(ctx, 0, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return err
}

// call calls tool with args and is its text content, "" where it answered
// no text; an answer that is no tool result, or a tool error, is an error
// naming which, so the take hands nothing over and says why.
func (c *mcpClient) call(ctx context.Context, tool string, args any) (string, error) {
	msg, err := c.rpc(ctx, 2, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return "", err
	}
	var answer struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err = json.Unmarshal(msg, &answer); err != nil || answer.Result == nil {
		return "", fmt.Errorf("the bridge answered %s with no tool result", tool)
	}
	if answer.Result.IsError {
		return "", fmt.Errorf("the bridge answered %s with a tool error", tool)
	}
	var text strings.Builder
	for _, part := range answer.Result.Content {
		if part.Type == "text" {
			text.WriteString(part.Text)
		}
	}
	return text.String(), nil
}

// close ends the session, within a second of its own: the bridge ends an
// abandoned one anyway. Under ctx stripped of its cancellation, since the
// take it ends may have ended on that very cancellation.
func (c *mcpClient) close(ctx context.Context) {
	if c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return
	}
	c.header(req)
	if resp, err := noRedirects.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// header sets the session's headers on req.
func (c *mcpClient) header(req *http.Request) {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
}

// rpc posts one JSON-RPC message and is the answer of id: a JSON body, or the
// `data:` line of an event stream that answers id, past any notification the
// stream carries before it (a running call's progress); nil for a
// notification (id 0), whose answer is empty.
func (c *mcpClient) rpc(ctx context.Context, id int, msg any) (json.RawMessage, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, scrubURL(err, c.endpoint, "the bridge")
	}
	c.header(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirects.Do(req)
	if err != nil {
		return nil, scrubURL(err, c.endpoint, "the bridge")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("the bridge answered %s", resp.Status)
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTake+1))
	if err != nil {
		return nil, scrubURL(err, c.endpoint, "the bridge")
	}
	if len(raw) > maxTake {
		return nil, fmt.Errorf("the bridge's answer is past %d bytes", maxTake)
	}
	if id == 0 {
		return nil, nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return raw, nil
	}
	for line := range strings.Lines(string(raw)) {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var head struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(data), &head) == nil && string(head.ID) == strconv.Itoa(id) {
			return json.RawMessage(strings.TrimSpace(data)), nil
		}
	}
	return nil, errors.New("the bridge's event stream ended with no answer")
}
