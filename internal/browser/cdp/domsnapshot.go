package cdp

// DOMSnapshotCaptureSnapshot is DOMSnapshot.captureSnapshot: every document
// of the target flattened, with each laid-out node's box. First consumer:
// masking, which paints over the boxes that hold a filled value.
var DOMSnapshotCaptureSnapshot = Method[DOMSnapshotCaptureSnapshotParams, DOMSnapshotCaptureSnapshotResult]{name: "DOMSnapshot.captureSnapshot"}

// DOMSnapshotCaptureSnapshotParams is DOMSnapshot.captureSnapshot's
// parameters.
type DOMSnapshotCaptureSnapshotParams struct {
	// ComputedStyles names the styles to report for each laid-out node; nil
	// asks for none.
	ComputedStyles List[string] `json:"computedStyles"`
}

// DOMSnapshotCaptureSnapshotResult is DOMSnapshot.captureSnapshot's result.
// Every string it holds is an index into Strings.
type DOMSnapshotCaptureSnapshotResult struct {
	Documents []DocumentSnapshot `json:"documents"`
	Strings   []string           `json:"strings"`
}

// DocumentSnapshot is DOMSnapshot.DocumentSnapshot: one document, its nodes
// as parallel arrays.
type DocumentSnapshot struct {
	// DocumentURL and FrameID are indexes into the result's Strings.
	DocumentURL   int64              `json:"documentURL"`
	FrameID       int64              `json:"frameId"`
	Nodes         NodeTreeSnapshot   `json:"nodes"`
	Layout        LayoutTreeSnapshot `json:"layout"`
	ScrollOffsetX float64            `json:"scrollOffsetX"`
	ScrollOffsetY float64            `json:"scrollOffsetY"`
}

// NodeTreeSnapshot is DOMSnapshot.NodeTreeSnapshot: the document's nodes, one
// index across every array.
type NodeTreeSnapshot struct {
	ParentIndex   []int64         `json:"parentIndex"`
	NodeType      []int64         `json:"nodeType"`
	NodeName      []int64         `json:"nodeName"`
	BackendNodeID []BackendNodeID `json:"backendNodeId"`
	// InputValue is the current value of the nodes that have one (input
	// and textarea), and TextValue a text node's text.
	InputValue RareStringData `json:"inputValue"`
	TextValue  RareStringData `json:"textValue"`
}

// RareStringData is DOMSnapshot.RareStringData: a string for some nodes, by
// node index; each value an index into the result's Strings.
type RareStringData struct {
	Index []int64 `json:"index"`
	Value []int64 `json:"value"`
}

// LayoutTreeSnapshot is DOMSnapshot.LayoutTreeSnapshot: the nodes that have a
// box, by node index.
type LayoutTreeSnapshot struct {
	NodeIndex []int64 `json:"nodeIndex"`
	// Bounds is each box's x, y, width and height, in the document's CSS
	// pixels.
	Bounds [][]float64 `json:"bounds"`
}
