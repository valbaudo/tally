// Package workflow defines Dawn's source-neutral workflow model.
package workflow

import "github.com/valbaudo/dawn/value"

// ProgramDraft is source-neutral input to workflow compilation.
type ProgramDraft struct {
	Root    string
	Modules []ModuleDraft
}

// ModuleDraft declares one reusable graph module.
type ModuleDraft struct {
	Name       string
	Graph      GraphDraft
	Provenance Provenance
}

// GraphDraft is source-neutral construction input for one graph.
type GraphDraft struct {
	Inputs     value.Contract
	Outputs    value.Contract
	Nodes      []NodeDraft
	Edges      []EdgeDraft
	Finally    *GraphDraft
	Provenance Provenance
}

// NodeDraft is one authored node variant.
type NodeDraft struct {
	Name       string
	Leaf       *LeafDraft
	Graph      *GraphDraft
	Branch     *BranchDraft
	Map        *MapDraft
	Loop       *LoopDraft
	Call       *CallDraft
	Parallel   *ParallelDraft
	Literals   []LiteralBindingDraft
	Provenance Provenance
}

// LeafDraft declares a closed leaf and its boundary contracts.
type LeafDraft struct {
	Kind             LeafKind
	Inputs           value.Contract
	Outputs          value.Contract
	BaseTree         []string
	PublishWorkspace []string
	Attachments      []AttachmentDraft
}

// CallDraft refers to a statically named module.
type CallDraft struct{ Module string }

// ParallelDraft provides author-facing static parallel grouping.
type ParallelDraft struct{ Graph GraphDraft }

// EndpointKind identifies one graph boundary or immediate child endpoint.
type EndpointKind uint8

const (
	Boundary EndpointKind = iota + 1
	Child
)

// EndpointDraft names an endpoint used by an edge draft.
type EndpointDraft struct {
	Kind  EndpointKind
	Child string
}

// BindingDraft maps one source path onto one target port.
type BindingDraft struct {
	From []string
	To   string
}

// EdgeDraft is the sole draft dependency relation.
type EdgeDraft struct {
	From, To EndpointDraft
	Bindings []BindingDraft
}

// LiteralBindingDraft supplies one child input directly.
type LiteralBindingDraft struct {
	Input string
	Value value.Literal
}

// CaseDraft is one named branch alternative.
type CaseDraft struct {
	Name  string
	Graph GraphDraft
}

// BranchDraft describes a closed selector and its alternatives.
type BranchDraft struct {
	Inputs, Outputs value.Contract
	Selector        string
	Cases           []CaseDraft
}

// MapDraft describes one collection-driven child graph.
type MapDraft struct {
	Inputs, Outputs    value.Contract
	Collection, Result string
	Body               GraphDraft
}

// LoopDraft describes one bounded sequential child graph.
type LoopDraft struct {
	Inputs, Outputs value.Contract
	Maximum         int
	Termination     []string
	Body            GraphDraft
}
