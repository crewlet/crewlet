package cdp

import "encoding/json"

// AXNodeID is Accessibility.AXNodeId: a node of an accessibility tree.
type AXNodeID string

// AccessibilityEnable is Accessibility.enable. First consumer: snapshots.
var AccessibilityEnable = Method[struct{}, struct{}]{name: "Accessibility.enable"}

// AccessibilityGetFullAXTree is Accessibility.getFullAXTree: one frame's
// accessibility tree. Nothing bounds its size at its source: Depth bounds how
// deep the tree goes, never how many nodes come back, and a frame of a few
// thousand table rows answers past ReadLimit, which fails the call with
// ErrMessageTooBig — so its consumer holds what it asks for below that
// itself. First consumer: snapshots, which fetch each out-of-process frame's
// on its own session.
var AccessibilityGetFullAXTree = Method[AccessibilityGetFullAXTreeParams, AccessibilityGetFullAXTreeResult]{name: "Accessibility.getFullAXTree"}

// AccessibilityGetFullAXTreeParams is Accessibility.getFullAXTree's
// parameters.
type AccessibilityGetFullAXTreeParams struct {
	Depth   int64   `json:"depth,omitempty"`
	FrameID FrameID `json:"frameId,omitempty"`
}

// AccessibilityGetFullAXTreeResult is Accessibility.getFullAXTree's result.
type AccessibilityGetFullAXTreeResult struct {
	Nodes []AXNode `json:"nodes"`
}

// AXNode is Accessibility.AXNode.
type AXNode struct {
	NodeID     AXNodeID     `json:"nodeId"`
	Ignored    bool         `json:"ignored"`
	Role       *AXValue     `json:"role"`
	Name       *AXValue     `json:"name"`
	Value      *AXValue     `json:"value"`
	Properties []AXProperty `json:"properties"`
	ParentID   AXNodeID     `json:"parentId"`
	ChildIDs   []AXNodeID   `json:"childIds"`
	// BackendDOMNodeID is the DOM node behind it, which a ref names.
	BackendDOMNodeID BackendNodeID `json:"backendDOMNodeId"`
	FrameID          FrameID       `json:"frameId"`
}

// AXValue is Accessibility.AXValue.
type AXValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// AXProperty is Accessibility.AXProperty: "focusable", "checked",
// "disabled" and the rest.
type AXProperty struct {
	Name  string  `json:"name"`
	Value AXValue `json:"value"`
}
