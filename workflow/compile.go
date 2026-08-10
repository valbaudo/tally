package workflow

import (
	"fmt"
	"strings"

	"github.com/valbaudo/dawn/value"
)

// compiler resolves static module calls while keeping the current expansion
// chain available for recursion diagnostics.
type compiler struct {
	modules  map[string]ModuleDraft
	active   []string
	graphs   map[*GraphDraft]struct{}
	branches map[*BranchDraft]struct{}
	maps     map[*MapDraft]struct{}
	loops    map[*LoopDraft]struct{}
}

// Compile lowers source-neutral workflow drafts into one immutable definition.
func Compile(draft ProgramDraft) (Definition, error) {
	c, err := newCompiler(draft.Modules)
	if err != nil {
		return Definition{}, err
	}
	root, err := c.compileModule(draft.Root)
	if err != nil {
		return Definition{}, err
	}
	if err := validateGraph(root, false, false); err != nil {
		return Definition{}, err
	}
	definition := Definition{root: root}
	normalizeDefinition(&definition)
	canonical, err := encodeDefinition(definition)
	if err != nil {
		return Definition{}, err
	}
	definition.canonical = canonical
	return definition, nil
}

func newCompiler(modules []ModuleDraft) (*compiler, error) {
	c := &compiler{
		modules:  make(map[string]ModuleDraft, len(modules)),
		graphs:   make(map[*GraphDraft]struct{}),
		branches: make(map[*BranchDraft]struct{}),
		maps:     make(map[*MapDraft]struct{}),
		loops:    make(map[*LoopDraft]struct{}),
	}
	for _, module := range modules {
		if module.Name == "" {
			return nil, fmt.Errorf("module name is empty")
		}
		if _, exists := c.modules[module.Name]; exists {
			return nil, fmt.Errorf("duplicate module %q", module.Name)
		}
		c.modules[module.Name] = module
	}
	return c, nil
}

func (c *compiler) compileModule(name string) (Graph, error) {
	frames := []*compileFrame{{kind: compileModuleFrame, moduleName: name}}
	for len(frames) != 0 {
		frame := frames[len(frames)-1]
		finished, err := c.stepCompileFrame(frame, &frames)
		if err != nil {
			err = wrapCompileContexts(frames, err)
			c.unwindCompileFrames(frames)
			return Graph{}, err
		}
		if !finished {
			continue
		}

		c.leaveCompileFrame(frame)
		frames = frames[:len(frames)-1]
		if len(frames) == 0 {
			return frame.graph, nil
		}
		frames[len(frames)-1].acceptCompiled(frame)
	}
	return Graph{}, fmt.Errorf("compiler finished without a root graph")
}

type compileFrameKind uint8

const (
	compileModuleFrame compileFrameKind = iota
	compileGraphFrame
	compileBranchFrame
	compileMapFrame
	compileLoopFrame
)

type compileNodeChildKind uint8

const (
	compileNodeComplete compileNodeChildKind = iota
	compileNodeGraph
	compileNodeBranch
	compileNodeMap
	compileNodeLoop
	compileNodeCall
	compileNodeParallel
)

type compileFrame struct {
	kind    compileFrameKind
	context string
	phase   uint8
	index   int
	marked  bool

	moduleName string
	module     ModuleDraft

	graphDraft     *GraphDraft
	graph          Graph
	childNames     map[string]struct{}
	fallbackSource bool
	pendingNode    Node
	pendingChild   compileNodeChildKind

	branchDraft *BranchDraft
	branch      Branch

	mapDraft *MapDraft
	mapped   Map

	loopDraft *LoopDraft
	loop      Loop

	childGraph  Graph
	childBranch Branch
	childMap    Map
	childLoop   Loop
}

func (c *compiler) stepCompileFrame(frame *compileFrame, frames *[]*compileFrame) (bool, error) {
	switch frame.kind {
	case compileModuleFrame:
		return c.stepModuleFrame(frame, frames)
	case compileGraphFrame:
		return c.stepGraphFrame(frame, frames)
	case compileBranchFrame:
		return c.stepBranchFrame(frame, frames)
	case compileMapFrame:
		return c.stepMapFrame(frame, frames)
	case compileLoopFrame:
		return c.stepLoopFrame(frame, frames)
	default:
		return false, fmt.Errorf("unknown compiler frame")
	}
}

func (c *compiler) stepModuleFrame(frame *compileFrame, frames *[]*compileFrame) (bool, error) {
	if frame.phase == 0 {
		for _, active := range c.active {
			if active == frame.moduleName {
				chain := append(append([]string(nil), c.active...), frame.moduleName)
				return false, fmt.Errorf("recursive module call: %s", strings.Join(chain, " -> "))
			}
		}
		module, exists := c.modules[frame.moduleName]
		if !exists {
			return false, fmt.Errorf("unknown module %q", frame.moduleName)
		}
		frame.module = module
		c.active = append(c.active, frame.moduleName)
		frame.marked = true
		frame.phase = 1
		*frames = append(*frames, &compileFrame{
			kind: compileGraphFrame, graphDraft: &frame.module.Graph,
			context: fmt.Sprintf("module %q", frame.moduleName),
		})
		return false, nil
	}
	frame.graph = frame.childGraph
	if frame.graph.provenance.Source == "" {
		frame.graph.provenance.Source = frame.module.Provenance.Source
	}
	frame.graph.provenance.Module = frame.moduleName
	frame.graph.provenance.Origin = OriginAuthored
	return true, nil
}

func (c *compiler) stepGraphFrame(frame *compileFrame, frames *[]*compileFrame) (bool, error) {
	switch frame.phase {
	case 0:
		if frame.graphDraft == nil {
			return false, fmt.Errorf("graph draft is nil")
		}
		if _, active := c.graphs[frame.graphDraft]; active {
			return false, fmt.Errorf("recursive graph draft pointer")
		}
		c.graphs[frame.graphDraft] = struct{}{}
		frame.marked = true
		inputs, err := cloneContract(frame.graphDraft.Inputs)
		if err != nil {
			return false, fmt.Errorf("graph inputs: %w", err)
		}
		outputs, err := cloneContract(frame.graphDraft.Outputs)
		if err != nil {
			return false, fmt.Errorf("graph outputs: %w", err)
		}
		frame.graph = Graph{
			inputs: inputs, outputs: outputs,
			provenance: authoredProvenance(frame.graphDraft.Provenance, c.active),
		}
		frame.fallbackSource = frame.graph.provenance.Source == "" && len(c.active) > 0
		if frame.fallbackSource {
			frame.graph.provenance.Source = c.modules[c.active[len(c.active)-1]].Provenance.Source
		}
		frame.childNames = make(map[string]struct{}, len(frame.graphDraft.Nodes))
		frame.phase = 1
		return false, nil
	case 1:
		if frame.index < len(frame.graphDraft.Nodes) {
			draft := frame.graphDraft.Nodes[frame.index]
			node, child, err := c.startCompiledNode(draft)
			if err != nil {
				return false, err
			}
			if child == compileNodeComplete {
				if err := appendCompiledNode(frame, node); err != nil {
					return false, err
				}
				frame.index++
				return false, nil
			}
			frame.pendingNode = node
			frame.pendingChild = child
			frame.phase = 2
			context := fmt.Sprintf("node %q", draft.Name)
			switch child {
			case compileNodeGraph:
				*frames = append(*frames, &compileFrame{kind: compileGraphFrame, graphDraft: draft.Graph, context: context + " graph"})
			case compileNodeBranch:
				*frames = append(*frames, &compileFrame{kind: compileBranchFrame, branchDraft: draft.Branch, context: context + " branch"})
			case compileNodeMap:
				*frames = append(*frames, &compileFrame{kind: compileMapFrame, mapDraft: draft.Map, context: context + " map"})
			case compileNodeLoop:
				*frames = append(*frames, &compileFrame{kind: compileLoopFrame, loopDraft: draft.Loop, context: context + " loop"})
			case compileNodeCall:
				*frames = append(*frames, &compileFrame{kind: compileModuleFrame, moduleName: draft.Call.Module, context: context + " call"})
			case compileNodeParallel:
				*frames = append(*frames, &compileFrame{kind: compileGraphFrame, graphDraft: &draft.Parallel.Graph, context: context + " parallel"})
			}
			return false, nil
		}

		edges, err := validateBindings(frame.graph, frame.graphDraft.Edges)
		if err != nil {
			return false, err
		}
		frame.graph.edges = edges
		if frame.fallbackSource {
			frame.graph.provenance.Source = ""
		}
		if frame.graphDraft.Finally == nil {
			return true, nil
		}
		frame.phase = 3
		*frames = append(*frames, &compileFrame{
			kind: compileGraphFrame, graphDraft: &frame.graphDraft.Finally.Graph, context: "finally",
		})
		return false, nil
	case 2:
		node := frame.pendingNode
		switch frame.pendingChild {
		case compileNodeGraph:
			graph := frame.childGraph
			node.scope = &Scope{kind: GraphScope, graph: &graph}
		case compileNodeBranch:
			branch := frame.childBranch
			node.scope = &Scope{kind: BranchScope, branch: &branch}
		case compileNodeMap:
			mapped := frame.childMap
			node.scope = &Scope{kind: MapScope, map_: &mapped}
		case compileNodeLoop:
			loop := frame.childLoop
			node.scope = &Scope{kind: LoopScope, loop: &loop}
		case compileNodeCall:
			graph := frame.childGraph
			graph.provenance.Origin = OriginCall
			graph.provenance.Module = frame.graphDraft.Nodes[frame.index].Call.Module
			node.provenance.Origin = OriginCall
			node.provenance.Module = frame.graphDraft.Nodes[frame.index].Call.Module
			node.scope = &Scope{kind: GraphScope, graph: &graph}
		case compileNodeParallel:
			graph := frame.childGraph
			graph.provenance.Origin = OriginParallel
			node.provenance.Origin = OriginParallel
			node.scope = &Scope{kind: GraphScope, graph: &graph}
		}
		if err := appendCompiledNode(frame, node); err != nil {
			return false, err
		}
		frame.index++
		frame.phase = 1
		return false, nil
	case 3:
		bindings, err := validateCleanupBindings(frame.graph, frame.childGraph, frame.graphDraft.Finally.Bindings)
		if err != nil {
			return false, fmt.Errorf("finally: %w", err)
		}
		frame.graph.cleanup = &Finally{graph: frame.childGraph, bindings: bindings}
		return true, nil
	default:
		return false, fmt.Errorf("invalid graph compiler phase")
	}
}

func (c *compiler) startCompiledNode(draft NodeDraft) (Node, compileNodeChildKind, error) {
	if draft.Name == "" {
		return Node{}, compileNodeComplete, fmt.Errorf("node name is empty")
	}
	variants := 0
	for _, present := range []bool{
		draft.Leaf != nil,
		draft.Graph != nil,
		draft.Branch != nil,
		draft.Map != nil,
		draft.Loop != nil,
		draft.Call != nil,
		draft.Parallel != nil,
	} {
		if present {
			variants++
		}
	}
	if variants != 1 {
		return Node{}, compileNodeComplete, fmt.Errorf("node %q has %d variants, want exactly one", draft.Name, variants)
	}

	node := Node{name: draft.Name, provenance: authoredProvenance(draft.Provenance, c.active)}
	for _, draftLiteral := range draft.Literals {
		literal, err := cloneLiteral(draftLiteral.Value)
		if err != nil {
			return Node{}, compileNodeComplete, fmt.Errorf("node %q literal %q: %w", draft.Name, draftLiteral.Input, err)
		}
		node.literals = append(node.literals, LiteralBinding{input: draftLiteral.Input, value: literal})
	}

	switch {
	case draft.Leaf != nil:
		if !validLeafKind(draft.Leaf.Kind) {
			return Node{}, compileNodeComplete, fmt.Errorf("node %q has unknown leaf kind %q", draft.Name, draft.Leaf.Kind)
		}
		inputs, err := cloneContract(draft.Leaf.Inputs)
		if err != nil {
			return Node{}, compileNodeComplete, fmt.Errorf("node %q leaf inputs: %w", draft.Name, err)
		}
		outputs, err := cloneContract(draft.Leaf.Outputs)
		if err != nil {
			return Node{}, compileNodeComplete, fmt.Errorf("node %q leaf outputs: %w", draft.Name, err)
		}
		delivery, err := compileDelivery(*draft.Leaf, inputs, outputs)
		if err != nil {
			return Node{}, compileNodeComplete, fmt.Errorf("node %q leaf delivery: %w", draft.Name, err)
		}
		node.leaf = &Leaf{
			kind: draft.Leaf.Kind, inputs: inputs, outputs: outputs,
			baseTree: delivery.baseTree, publishWorkspace: delivery.publishWorkspace, attachments: delivery.attachments,
		}
		return node, compileNodeComplete, nil
	case draft.Graph != nil:
		return node, compileNodeGraph, nil
	case draft.Branch != nil:
		return node, compileNodeBranch, nil
	case draft.Map != nil:
		return node, compileNodeMap, nil
	case draft.Loop != nil:
		return node, compileNodeLoop, nil
	case draft.Call != nil:
		return node, compileNodeCall, nil
	case draft.Parallel != nil:
		if len(draft.Parallel.Graph.Nodes) < 2 {
			return Node{}, compileNodeComplete, fmt.Errorf("node %q parallel requires at least two immediate children", draft.Name)
		}
		return node, compileNodeParallel, nil
	}
	return node, compileNodeComplete, nil
}

func appendCompiledNode(frame *compileFrame, node Node) error {
	if _, exists := frame.childNames[node.name]; exists {
		return bindingError(frame.graph, "duplicate child name %q", node.name)
	}
	frame.childNames[node.name] = struct{}{}
	frame.graph.nodes = append(frame.graph.nodes, node)
	return nil
}

func (c *compiler) stepBranchFrame(frame *compileFrame, frames *[]*compileFrame) (bool, error) {
	switch frame.phase {
	case 0:
		if _, active := c.branches[frame.branchDraft]; active {
			return false, fmt.Errorf("recursive branch draft pointer")
		}
		c.branches[frame.branchDraft] = struct{}{}
		frame.marked = true
		inputs, err := cloneContract(frame.branchDraft.Inputs)
		if err != nil {
			return false, fmt.Errorf("inputs: %w", err)
		}
		outputs, err := cloneContract(frame.branchDraft.Outputs)
		if err != nil {
			return false, fmt.Errorf("outputs: %w", err)
		}
		frame.branch = Branch{inputs: inputs, outputs: outputs, selector: frame.branchDraft.Selector}
		frame.phase = 1
		return false, nil
	case 1:
		if frame.index == len(frame.branchDraft.Cases) {
			return true, nil
		}
		draftCase := &frame.branchDraft.Cases[frame.index]
		frame.phase = 2
		*frames = append(*frames, &compileFrame{
			kind: compileGraphFrame, graphDraft: &draftCase.Graph,
			context: fmt.Sprintf("case %q", draftCase.Name),
		})
		return false, nil
	case 2:
		draftCase := &frame.branchDraft.Cases[frame.index]
		frame.branch.cases = append(frame.branch.cases, Case{name: draftCase.Name, graph: frame.childGraph})
		frame.index++
		frame.phase = 1
		return false, nil
	default:
		return false, fmt.Errorf("invalid branch compiler phase")
	}
}

func (c *compiler) stepMapFrame(frame *compileFrame, frames *[]*compileFrame) (bool, error) {
	if frame.phase == 0 {
		if _, active := c.maps[frame.mapDraft]; active {
			return false, fmt.Errorf("recursive map draft pointer")
		}
		c.maps[frame.mapDraft] = struct{}{}
		frame.marked = true
		inputs, err := cloneContract(frame.mapDraft.Inputs)
		if err != nil {
			return false, fmt.Errorf("inputs: %w", err)
		}
		outputs, err := cloneContract(frame.mapDraft.Outputs)
		if err != nil {
			return false, fmt.Errorf("outputs: %w", err)
		}
		frame.mapped = Map{
			inputs: inputs, outputs: outputs,
			collection: frame.mapDraft.Collection, result: frame.mapDraft.Result,
		}
		frame.phase = 1
		*frames = append(*frames, &compileFrame{
			kind: compileGraphFrame, graphDraft: &frame.mapDraft.Body, context: "body",
		})
		return false, nil
	}
	frame.mapped.body = frame.childGraph
	return true, nil
}

func (c *compiler) stepLoopFrame(frame *compileFrame, frames *[]*compileFrame) (bool, error) {
	if frame.phase == 0 {
		if _, active := c.loops[frame.loopDraft]; active {
			return false, fmt.Errorf("recursive loop draft pointer")
		}
		c.loops[frame.loopDraft] = struct{}{}
		frame.marked = true
		inputs, err := cloneContract(frame.loopDraft.Inputs)
		if err != nil {
			return false, fmt.Errorf("inputs: %w", err)
		}
		outputs, err := cloneContract(frame.loopDraft.Outputs)
		if err != nil {
			return false, fmt.Errorf("outputs: %w", err)
		}
		frame.loop = Loop{
			inputs: inputs, outputs: outputs, maximum: frame.loopDraft.Maximum,
			termination: append([]string(nil), frame.loopDraft.Termination...),
		}
		frame.phase = 1
		*frames = append(*frames, &compileFrame{
			kind: compileGraphFrame, graphDraft: &frame.loopDraft.Body, context: "body",
		})
		return false, nil
	}
	frame.loop.body = frame.childGraph
	return true, nil
}

func (frame *compileFrame) acceptCompiled(child *compileFrame) {
	switch child.kind {
	case compileModuleFrame, compileGraphFrame:
		frame.childGraph = child.graph
	case compileBranchFrame:
		frame.childBranch = child.branch
	case compileMapFrame:
		frame.childMap = child.mapped
	case compileLoopFrame:
		frame.childLoop = child.loop
	}
}

func wrapCompileContexts(frames []*compileFrame, err error) error {
	for index := len(frames) - 1; index >= 0; index-- {
		if frames[index].context != "" {
			err = fmt.Errorf("%s: %w", frames[index].context, err)
		}
	}
	return err
}

func (c *compiler) unwindCompileFrames(frames []*compileFrame) {
	for index := len(frames) - 1; index >= 0; index-- {
		c.leaveCompileFrame(frames[index])
	}
}

func (c *compiler) leaveCompileFrame(frame *compileFrame) {
	if !frame.marked {
		return
	}
	switch frame.kind {
	case compileModuleFrame:
		c.active = c.active[:len(c.active)-1]
	case compileGraphFrame:
		delete(c.graphs, frame.graphDraft)
	case compileBranchFrame:
		delete(c.branches, frame.branchDraft)
	case compileMapFrame:
		delete(c.maps, frame.mapDraft)
	case compileLoopFrame:
		delete(c.loops, frame.loopDraft)
	}
	frame.marked = false
}

func authoredProvenance(provenance Provenance, active []string) Provenance {
	module := ""
	if len(active) > 0 {
		module = active[len(active)-1]
	}
	return Provenance{Source: provenance.Source, Module: module, Origin: OriginAuthored}
}

func cloneContract(contract value.Contract) (value.Contract, error) {
	if !contract.Valid() {
		return value.Contract{}, fmt.Errorf("contract is invalid")
	}
	copy, err := value.NewContract(contract.Ports()...)
	if err != nil {
		return value.Contract{}, err
	}
	return copy, nil
}

func cloneLiteral(literal value.Literal) (value.Literal, error) {
	return value.ParseLiteral(literal.Bytes())
}
