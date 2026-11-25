package coord

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A node's status record is filed as ROWS, each a key of its own in the node
// status bucket (ADR-0175, "The node status bucket declares its classes"): the
// node's own row (its epoch, instant, runner cells, infra summary and runs
// waiting for an operator), one row per configured MCP server, one per model
// entry, and one of its growth signals. One record per node had no bound but
// the configuration's size, and every reader of one part read every part.
//
// EVERY ROW CARRIES ITS EPOCH, and a reader counts a node's rows only at the
// epoch of its own row ([AssembleNodeStatusRecords]) and of its live presence
// ([LiveNodeStatuses]): a row an earlier incarnation left, or one whose node
// row is gone, is never shown. A writer writes a node's own row LAST, so a
// node counted at a new epoch has every row it wrote there in place.

// NodeStatusClass is one class of the node status bucket's keys, its first
// segment.
type NodeStatusClass string

const (
	// NodeStatusOwn keys a node's own row, `node_status.<node>`.
	NodeStatusOwn NodeStatusClass = "node_status"
	// NodeStatusMCP keys one configured MCP server's row,
	// `mcp_status.<node>.<server>`.
	NodeStatusMCP NodeStatusClass = "mcp_status"
	// NodeStatusModel keys one model entry's row,
	// `model_status.<node>.<entry>`.
	NodeStatusModel NodeStatusClass = "model_status"
	// NodeStatusGrowth keys a node's growth signals, `growth_status.<node>`:
	// a row of their own, since two lists of [MaxGrowthSignals] at their
	// widest take most of [MaxNodeStatusRowBytes] alone.
	NodeStatusGrowth NodeStatusClass = "growth_status"
)

// NodeStatusClasses is every class, the node's own first.
var NodeStatusClasses = []NodeStatusClass{NodeStatusOwn, NodeStatusMCP, NodeStatusModel, NodeStatusGrowth}

// Valid reports whether c is a class this build names.
func (c NodeStatusClass) Valid() bool { return slices.Contains(NodeStatusClasses, c) }

// Named reports whether c holds one row per name under its node: an MCP
// server's, or a model entry's.
func (c NodeStatusClass) Named() bool { return c == NodeStatusMCP || c == NodeStatusModel }

// MaxNodeStatusRowBytes bounds every row of the node status bucket as stored:
// [SmallValueBytes], since every reader (an alarm's evaluation, a page, the
// atlas) reads them at the read term. Each row's widest fits by its fields'
// bounds alone (nodestatusrows_test.go), but an MCP row's unlisted keys,
// which its writer keeps whole and cuts by count ([MCPServerStatus.Unlisted]).
const MaxNodeStatusRowBytes = SmallValueBytes

// MaxModelWordBytes bounds each of harness's words a model row or a runner
// cell names (a sign-in route, a window, a failure's kind, a login's or a
// cell's state): 32 bytes, past the longest any names (11), so a build that
// adds one travels whole and a value nothing names cannot grow the row.
const MaxModelWordBytes = 32

// NodeStatusRowKey is where one row is filed.
type NodeStatusRowKey struct {
	Class NodeStatusClass
	Node  string
	// Name is the MCP server's configured name or the model entry's key, ""
	// for a class that is not [NodeStatusClass.Named].
	Name string
}

// Check refuses a key a store cannot file ([ErrNoNodeStatus]): one naming no
// class this build knows, no node, or a name its class does not take.
func (k NodeStatusRowKey) Check() error {
	if !k.Class.Valid() || k.Node == "" || k.Class.Named() != (k.Name != "") {
		return fmt.Errorf("%w (class %q, node %q, name %q)", ErrNoNodeStatus, k.Class, k.Node, k.Name)
	}
	return nil
}

// Key is the key the row is filed under.
func (k NodeStatusRowKey) Key() string {
	if k.Class.Named() {
		return DocumentKey(string(k.Class), k.Node, k.Name)
	}
	return DocumentKey(string(k.Class), k.Node)
}

// ParseNodeStatusKey recovers a row's key, reporting false for one this
// grammar did not write.
func ParseNodeStatusKey(key string) (NodeStatusRowKey, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) < 2 {
		return NodeStatusRowKey{}, false
	}
	k := NodeStatusRowKey{Class: NodeStatusClass(segments[0]), Node: segments[1]}
	if len(segments) == 3 {
		k.Name = segments[2]
	}
	if len(segments) > 3 || k.Check() != nil {
		return NodeStatusRowKey{}, false
	}
	return k, true
}

// NodeStatusRow is one row as a store keeps it: its key and the bytes
// [NodeStatusRecord.Rows] encoded, its epoch among them.
type NodeStatusRow struct {
	Key   NodeStatusRowKey
	Value []byte
}

// Check refuses a row a store cannot file ([ErrNoNodeStatus]): a key
// [NodeStatusRowKey.Check] refuses, or a value naming no presence epoch.
func (r NodeStatusRow) Check() error {
	if err := r.Key.Check(); err != nil {
		return err
	}
	if epoch, ok := NodeStatusRowEpoch(r.Value); !ok || epoch <= 0 {
		return fmt.Errorf("%w (%s names no epoch)", ErrNoNodeStatus, r.Key.Key())
	}
	return nil
}

// NodeStatusRowEpoch is the epoch a row's value was written under, false for
// a value that is not a row.
func NodeStatusRowEpoch(value []byte) (int64, bool) {
	var row struct {
		Epoch *int64 `json:"epoch"`
	}
	if err := json.Unmarshal(value, &row); err != nil || row.Epoch == nil {
		return 0, false
	}
	return *row.Epoch, true
}

// NodeStatusRead is which classes a listing reads beside every node's own row,
// which it always reads: a reader of one part pays for that part alone.
type NodeStatusRead struct {
	MCP, Models, Growth bool
}

// Classes is the classes the listing reads, the node's own first: read
// first, so a row its writer wrote before that own row is read too.
func (r NodeStatusRead) Classes() []NodeStatusClass {
	out := []NodeStatusClass{NodeStatusOwn}
	if r.MCP {
		out = append(out, NodeStatusMCP)
	}
	if r.Models {
		out = append(out, NodeStatusModel)
	}
	if r.Growth {
		out = append(out, NodeStatusGrowth)
	}
	return out
}

// Rows is the record as its rows, in the order a writer writes them: every
// MCP, model and growth row, then the node's own LAST. A row naming no server
// or entry is left out, as a reader would drop it, and so is the growth row of
// a node that met nothing. Each row is within [MaxNodeStatusRowBytes].
func (r NodeStatusRecord) Rows() ([]NodeStatusRow, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	var out []NodeStatusRow
	add := func(class NodeStatusClass, name string, body map[string]any) error {
		body["epoch"] = r.Epoch
		key := NodeStatusRowKey{Class: class, Node: r.Node, Name: name}
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("coord: encode %s: %w", key.Key(), err)
		}
		if err := HoldValue(key.Key(), len(raw), MaxNodeStatusRowBytes); err != nil {
			return err
		}
		out = append(out, NodeStatusRow{Key: key, Value: raw})
		return nil
	}
	for _, m := range r.MCP {
		if m.Server != "" {
			if err := add(NodeStatusMCP, m.Server, m.meta()); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range r.Models {
		if m.Key != "" {
			if err := add(NodeStatusModel, m.Key, m.meta()); err != nil {
				return nil, err
			}
		}
	}
	if g := r.Growth.meta(); g != nil {
		if err := add(NodeStatusGrowth, "", g); err != nil {
			return nil, err
		}
	}
	if err := add(NodeStatusOwn, "", r.ownMeta()); err != nil {
		return nil, err
	}
	return out, nil
}

// ownMeta is the node's own row: its instant and each part it carries, every
// part OMITTED when it says nothing, so a node that runs nothing writes its
// epoch and its instant alone.
func (r NodeStatusRecord) ownMeta() map[string]any {
	out := map[string]any{"at": stamp(r.At)}
	if r.Runners != nil {
		out["runners"] = map[string]any{
			"direct":    r.Runners.Direct.meta(),
			"container": r.Runners.Container.meta(),
		}
	}
	if infra := r.Infra.Meta(); infra != nil {
		out["infra"] = infra
	}
	if b := r.ResumesBlocked; b != nil && b.Runs > 0 {
		// OMITTED WHERE NOTHING WAITS, which is every row of a node whose
		// seats wait for no operator: absent reads as none.
		out["resumes_blocked"] = map[string]any{
			"runs":  b.Runs,
			"seat":  fitWithin(b.Seat, iam.MaxLogin),
			"kind":  fitWithin(b.Kind, MaxResumeBlockedKindBytes),
			"since": stamp(b.Since),
		}
	}
	return out
}

// AssembleNodeStatusRecords is each node's record, ordered by node: its own
// row decides its epoch, every other row counts only at it, and a row that
// does not decode is SKIPPED, one node read as not saying, never all failed.
//
// WHOLE OR NOT AT ALL, from passes that are not one instant: reread is the
// own rows read again after every other class (nil for rows read at one
// instant). A node is dropped where its own row left reread or moved epoch
// (it stopped, or was claimed anew), or any row of it is at a later epoch
// than its own (its writer began its next): its reader keeps what it last
// counted, or reads it as not saying, never as part of its rows.
func AssembleNodeStatusRecords(rows, reread []NodeStatusRow) []NodeStatusRecord {
	byNode := map[string]*NodeStatusRecord{}
	for _, row := range rows {
		if row.Key.Class != NodeStatusOwn {
			continue
		}
		if r, ok := decodeOwnRow(row.Key.Node, row.Value); ok {
			byNode[row.Key.Node] = &r
		}
	}
	if reread != nil {
		still := map[string]int64{}
		for _, row := range reread {
			if epoch, ok := NodeStatusRowEpoch(row.Value); ok && row.Key.Class == NodeStatusOwn {
				still[row.Key.Node] = epoch
			}
		}
		for node, r := range byNode {
			if epoch, ok := still[node]; !ok || epoch != r.Epoch {
				delete(byNode, node)
			}
		}
	}
	moved := map[string]bool{}
	for _, row := range rows {
		r, ok := byNode[row.Key.Node]
		if !ok || row.Key.Class == NodeStatusOwn {
			continue
		}
		meta, ok := rowMeta(row.Value)
		if !ok {
			continue
		}
		if epoch, _ := int64FromMeta(meta["epoch"]); epoch > r.Epoch {
			moved[row.Key.Node] = true
		} else if epoch != r.Epoch {
			continue
		}
		switch row.Key.Class {
		case NodeStatusMCP:
			r.MCP = append(r.MCP, mcpFromMeta(row.Key.Name, meta))
		case NodeStatusModel:
			r.Models = append(r.Models, modelFromMeta(row.Key.Name, meta))
		case NodeStatusGrowth:
			r.Growth = growthFromMeta(meta)
		}
	}
	out := make([]NodeStatusRecord, 0, len(byNode))
	for node, r := range byNode {
		if moved[node] {
			continue
		}
		slices.SortFunc(r.MCP, func(a, b MCPServerStatus) int { return cmp.Compare(a.Server, b.Server) })
		slices.SortFunc(r.Models, func(a, b ModelStatus) int { return cmp.Compare(a.Key, b.Key) })
		out = append(out, *r)
	}
	SortNodeStatusRecords(out)
	return out
}

// rowMeta is a row's JSON object, false for a body that is not one. A key this
// build does not know is IGNORED rather than refused, for the reasons
// [StatusFromMeta] gives.
func rowMeta(raw []byte) (map[string]any, bool) {
	// NUMBERS AS WRITTEN ([json.Number]): a float64 would round a count or
	// an epoch past 2^53 and truncate a fraction into one nobody wrote.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var meta map[string]any
	if err := dec.Decode(&meta); err != nil || meta == nil || dec.More() {
		return nil, false
	}
	return meta, true
}

// decodeOwnRow reads a node's own row, false for one that is not an object.
func decodeOwnRow(node string, raw []byte) (NodeStatusRecord, bool) {
	meta, ok := rowMeta(raw)
	if !ok {
		return NodeStatusRecord{}, false
	}
	epoch, _ := int64FromMeta(meta["epoch"])
	r := NodeStatusRecord{Node: node, Epoch: epoch, At: instant(meta["at"])}
	if runners, ok := meta["runners"].(map[string]any); ok {
		r.Runners = &RunnerStatus{
			Direct:    cellFromMeta(runners["direct"]),
			Container: cellFromMeta(runners["container"]),
		}
	}
	if infra, ok := meta["infra"].(map[string]any); ok {
		r.Infra = infraFromMeta(infra)
	}
	r.ResumesBlocked = resumesBlockedFromMeta(meta["resumes_blocked"])
	return r, true
}

// fitWithin is s cut as [textcut.Within] cuts it, marked, so that JSON writes
// it in at most max bytes: a bound on what a row STORES, which a byte JSON
// writes as six (a control character, `<`) would otherwise pass sixfold.
// Text within it in its own bytes is what it was.
func fitWithin(s string, max int) string { return fit(s, max, textcut.Within) }

// fitBytes is [fitWithin] unmarked ([textcut.Bytes]), for an identifier or a
// host whose appended marker would read as part of it.
func fitBytes(s string, max int) string { return fit(s, max, textcut.Bytes) }

// fit is the longest cut of s whose JSON text is at most max bytes: s itself
// where it fits, else the cut at the largest budget that fits, found by
// bisection, since a larger budget never stores shorter. Budget zero fits
// wherever max holds [textcut.Within]'s marker, as it does wherever called.
func fit(s string, max int, cut func(string, int) string) string {
	if c := cut(s, max); jsonTextBytes(c) <= max {
		return c
	}
	fits, over := 0, max
	for over-fits > 1 {
		mid := fits + (over-fits)/2
		if jsonTextBytes(cut(s, mid)) <= max {
			fits = mid
		} else {
			over = mid
		}
	}
	return cut(s, fits)
}

// jsonTextBytes is how many bytes JSON writes s in, its quotes aside.
func jsonTextBytes(s string) int {
	raw, _ := json.Marshal(s)
	return len(raw) - 2
}
