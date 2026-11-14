package cdp

// BackendNodeID is DOM.BackendNodeId: a node of a document, stable for its
// life whether or not the DOM domain has handed it out — what a ref names.
type BackendNodeID int64

// Quad is DOM.Quad: four points, x and y in turn, clockwise from the top
// left.
type Quad []float64

// Rect is DOM.Rect.
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// DOMDescribeNode is DOM.describeNode. First consumer: refs.
var DOMDescribeNode = Method[DOMDescribeNodeParams, DOMDescribeNodeResult]{name: "DOM.describeNode"}

// DOMDescribeNodeParams is DOM.describeNode's parameters: the node, named by
// one of its ids.
type DOMDescribeNodeParams struct {
	BackendNodeID BackendNodeID  `json:"backendNodeId,omitempty"`
	ObjectID      RemoteObjectID `json:"objectId,omitempty"`
}

// DOMDescribeNodeResult is DOM.describeNode's result.
type DOMDescribeNodeResult struct {
	Node Node `json:"node"`
}

// Node is DOM.Node, as much of it as a ref reads.
type Node struct {
	BackendNodeID BackendNodeID `json:"backendNodeId"`
	NodeType      int64         `json:"nodeType"`
	NodeName      string        `json:"nodeName"`
	LocalName     string        `json:"localName"`
	// Attributes is name and value in turn.
	Attributes []string `json:"attributes"`
	// FrameID is a frame owner's frame, an iframe's document.
	FrameID FrameID `json:"frameId"`
}

// DOMResolveNode is DOM.resolveNode: a handle to a node, in one context.
// First consumer: refs.
var DOMResolveNode = Method[DOMResolveNodeParams, DOMResolveNodeResult]{name: "DOM.resolveNode"}

// DOMResolveNodeParams is DOM.resolveNode's parameters.
type DOMResolveNodeParams struct {
	BackendNodeID      BackendNodeID      `json:"backendNodeId"`
	ExecutionContextID ExecutionContextID `json:"executionContextId,omitempty"`
	ObjectGroup        string             `json:"objectGroup,omitempty"`
}

// DOMResolveNodeResult is DOM.resolveNode's result.
type DOMResolveNodeResult struct {
	Object RemoteObject `json:"object"`
}

// DOMGetContentQuads is DOM.getContentQuads: where a node's boxes are, in
// the viewport's CSS pixels. First consumer: hit tests.
var DOMGetContentQuads = Method[DOMGetContentQuadsParams, DOMGetContentQuadsResult]{name: "DOM.getContentQuads"}

// DOMGetContentQuadsParams is DOM.getContentQuads' parameters.
type DOMGetContentQuadsParams struct {
	BackendNodeID BackendNodeID `json:"backendNodeId"`
}

// DOMGetContentQuadsResult is DOM.getContentQuads' result.
type DOMGetContentQuadsResult struct {
	Quads []Quad `json:"quads"`
}

// DOMGetBoxModel is DOM.getBoxModel. First consumer: hit tests.
var DOMGetBoxModel = Method[DOMGetBoxModelParams, DOMGetBoxModelResult]{name: "DOM.getBoxModel"}

// DOMGetBoxModelParams is DOM.getBoxModel's parameters.
type DOMGetBoxModelParams struct {
	BackendNodeID BackendNodeID `json:"backendNodeId"`
}

// DOMGetBoxModelResult is DOM.getBoxModel's result.
type DOMGetBoxModelResult struct {
	Model BoxModel `json:"model"`
}

// BoxModel is DOM.BoxModel.
type BoxModel struct {
	Content Quad  `json:"content"`
	Padding Quad  `json:"padding"`
	Border  Quad  `json:"border"`
	Margin  Quad  `json:"margin"`
	Width   int64 `json:"width"`
	Height  int64 `json:"height"`
}

// DOMGetNodeForLocation is DOM.getNodeForLocation: the node a point of the
// viewport would hit. First consumer: hit tests.
var DOMGetNodeForLocation = Method[DOMGetNodeForLocationParams, DOMGetNodeForLocationResult]{name: "DOM.getNodeForLocation"}

// DOMGetNodeForLocationParams is DOM.getNodeForLocation's parameters.
type DOMGetNodeForLocationParams struct {
	X                         int64 `json:"x"`
	Y                         int64 `json:"y"`
	IncludeUserAgentShadowDOM bool  `json:"includeUserAgentShadowDOM,omitempty"`
	IgnorePointerEventsNone   bool  `json:"ignorePointerEventsNone,omitempty"`
}

// DOMGetNodeForLocationResult is DOM.getNodeForLocation's result.
type DOMGetNodeForLocationResult struct {
	BackendNodeID BackendNodeID `json:"backendNodeId"`
	FrameID       FrameID       `json:"frameId"`
}

// DOMGetFrameOwner is DOM.getFrameOwner: the element a frame sits in. First
// consumer: snapshots, inlining an out-of-process frame where it sits.
var DOMGetFrameOwner = Method[DOMGetFrameOwnerParams, DOMGetFrameOwnerResult]{name: "DOM.getFrameOwner"}

// DOMGetFrameOwnerParams is DOM.getFrameOwner's parameters.
type DOMGetFrameOwnerParams struct {
	FrameID FrameID `json:"frameId"`
}

// DOMGetFrameOwnerResult is DOM.getFrameOwner's result.
type DOMGetFrameOwnerResult struct {
	BackendNodeID BackendNodeID `json:"backendNodeId"`
}

// DOMScrollIntoViewIfNeeded is DOM.scrollIntoViewIfNeeded. First consumer:
// input.
var DOMScrollIntoViewIfNeeded = Method[DOMScrollIntoViewIfNeededParams, struct{}]{name: "DOM.scrollIntoViewIfNeeded"}

// DOMScrollIntoViewIfNeededParams is DOM.scrollIntoViewIfNeeded's parameters.
type DOMScrollIntoViewIfNeededParams struct {
	BackendNodeID BackendNodeID `json:"backendNodeId"`
}

// DOMFocus is DOM.focus. First consumer: input, which sends keys only once
// the ref's element holds the focus.
var DOMFocus = Method[DOMFocusParams, struct{}]{name: "DOM.focus"}

// DOMFocusParams is DOM.focus' parameters.
type DOMFocusParams struct {
	BackendNodeID BackendNodeID `json:"backendNodeId"`
}
