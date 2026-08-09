package workflow

import (
	"fmt"
	"strings"

	"github.com/valbaudo/dawn/value"
)

// compiler resolves static module calls while keeping the current expansion
// chain available for recursion diagnostics.
type compiler struct {
	modules map[string]ModuleDraft
	active  []string
	graphs  map[*GraphDraft]struct{}
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
	return Definition{root: root}, nil
}

func newCompiler(modules []ModuleDraft) (*compiler, error) {
	c := &compiler{
		modules: make(map[string]ModuleDraft, len(modules)),
		graphs:  make(map[*GraphDraft]struct{}),
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
	for _, active := range c.active {
		if active == name {
			chain := append(append([]string(nil), c.active...), name)
			return Graph{}, fmt.Errorf("recursive module call: %s", strings.Join(chain, " -> "))
		}
	}
	module, exists := c.modules[name]
	if !exists {
		return Graph{}, fmt.Errorf("unknown module %q", name)
	}

	c.active = append(c.active, name)
	defer func() { c.active = c.active[:len(c.active)-1] }()

	graph, err := c.compileGraph(&module.Graph)
	if err != nil {
		return Graph{}, fmt.Errorf("module %q: %w", name, err)
	}
	if graph.provenance.Source == "" {
		graph.provenance.Source = module.Provenance.Source
	}
	graph.provenance.Module = name
	graph.provenance.Origin = OriginAuthored
	return graph, nil
}

func (c *compiler) compileGraph(draft *GraphDraft) (Graph, error) {
	if draft == nil {
		return Graph{}, fmt.Errorf("graph draft is nil")
	}
	if _, active := c.graphs[draft]; active {
		return Graph{}, fmt.Errorf("recursive graph draft pointer")
	}
	c.graphs[draft] = struct{}{}
	defer delete(c.graphs, draft)

	inputs, err := cloneContract(draft.Inputs)
	if err != nil {
		return Graph{}, fmt.Errorf("graph inputs: %w", err)
	}
	outputs, err := cloneContract(draft.Outputs)
	if err != nil {
		return Graph{}, fmt.Errorf("graph outputs: %w", err)
	}
	graph := Graph{
		inputs:     inputs,
		outputs:    outputs,
		provenance: authoredProvenance(draft.Provenance, c.active),
	}
	fallbackSource := graph.provenance.Source == "" && len(c.active) > 0
	if fallbackSource {
		graph.provenance.Source = c.modules[c.active[len(c.active)-1]].Provenance.Source
	}
	childNames := make(map[string]struct{}, len(draft.Nodes))
	for _, draftNode := range draft.Nodes {
		node, err := c.compileNode(draftNode)
		if err != nil {
			return Graph{}, err
		}
		if _, exists := childNames[node.name]; exists {
			return Graph{}, bindingError(graph, "duplicate child name %q", node.name)
		}
		childNames[node.name] = struct{}{}
		graph.nodes = append(graph.nodes, node)
	}
	edges, err := validateBindings(graph, draft.Edges)
	if err != nil {
		return Graph{}, err
	}
	graph.edges = edges
	if fallbackSource {
		graph.provenance.Source = ""
	}
	if draft.Finally != nil {
		cleanup, err := c.compileGraph(draft.Finally)
		if err != nil {
			return Graph{}, fmt.Errorf("finally: %w", err)
		}
		graph.cleanup = &Finally{graph: cleanup}
	}
	return graph, nil
}

func (c *compiler) compileNode(draft NodeDraft) (Node, error) {
	if draft.Name == "" {
		return Node{}, fmt.Errorf("node name is empty")
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
		return Node{}, fmt.Errorf("node %q has %d variants, want exactly one", draft.Name, variants)
	}

	node := Node{name: draft.Name, provenance: authoredProvenance(draft.Provenance, c.active)}
	for _, draftLiteral := range draft.Literals {
		literal, err := cloneLiteral(draftLiteral.Value)
		if err != nil {
			return Node{}, fmt.Errorf("node %q literal %q: %w", draft.Name, draftLiteral.Input, err)
		}
		node.literals = append(node.literals, LiteralBinding{input: draftLiteral.Input, value: literal})
	}

	switch {
	case draft.Leaf != nil:
		if !validLeafKind(draft.Leaf.Kind) {
			return Node{}, fmt.Errorf("node %q has unknown leaf kind %q", draft.Name, draft.Leaf.Kind)
		}
		inputs, err := cloneContract(draft.Leaf.Inputs)
		if err != nil {
			return Node{}, fmt.Errorf("node %q leaf inputs: %w", draft.Name, err)
		}
		outputs, err := cloneContract(draft.Leaf.Outputs)
		if err != nil {
			return Node{}, fmt.Errorf("node %q leaf outputs: %w", draft.Name, err)
		}
		node.leaf = &Leaf{kind: draft.Leaf.Kind, inputs: inputs, outputs: outputs}
	case draft.Graph != nil:
		graph, err := c.compileGraph(draft.Graph)
		if err != nil {
			return Node{}, fmt.Errorf("node %q graph: %w", draft.Name, err)
		}
		node.scope = &Scope{kind: GraphScope, graph: &graph}
	case draft.Branch != nil:
		branch, err := c.compileBranch(*draft.Branch)
		if err != nil {
			return Node{}, fmt.Errorf("node %q branch: %w", draft.Name, err)
		}
		node.scope = &Scope{kind: BranchScope, branch: &branch}
	case draft.Map != nil:
		mapped, err := c.compileMap(*draft.Map)
		if err != nil {
			return Node{}, fmt.Errorf("node %q map: %w", draft.Name, err)
		}
		node.scope = &Scope{kind: MapScope, map_: &mapped}
	case draft.Loop != nil:
		loop, err := c.compileLoop(*draft.Loop)
		if err != nil {
			return Node{}, fmt.Errorf("node %q loop: %w", draft.Name, err)
		}
		node.scope = &Scope{kind: LoopScope, loop: &loop}
	case draft.Call != nil:
		graph, err := c.compileModule(draft.Call.Module)
		if err != nil {
			return Node{}, fmt.Errorf("node %q call: %w", draft.Name, err)
		}
		graph.provenance.Origin = OriginCall
		graph.provenance.Module = draft.Call.Module
		node.provenance.Origin = OriginCall
		node.provenance.Module = draft.Call.Module
		node.scope = &Scope{kind: GraphScope, graph: &graph}
	case draft.Parallel != nil:
		if len(draft.Parallel.Graph.Nodes) < 2 {
			return Node{}, fmt.Errorf("node %q parallel requires at least two immediate children", draft.Name)
		}
		graph, err := c.compileGraph(&draft.Parallel.Graph)
		if err != nil {
			return Node{}, fmt.Errorf("node %q parallel: %w", draft.Name, err)
		}
		graph.provenance.Origin = OriginParallel
		node.provenance.Origin = OriginParallel
		node.scope = &Scope{kind: GraphScope, graph: &graph}
	}
	return node, nil
}

func (c *compiler) compileBranch(draft BranchDraft) (Branch, error) {
	inputs, err := cloneContract(draft.Inputs)
	if err != nil {
		return Branch{}, fmt.Errorf("inputs: %w", err)
	}
	outputs, err := cloneContract(draft.Outputs)
	if err != nil {
		return Branch{}, fmt.Errorf("outputs: %w", err)
	}
	branch := Branch{inputs: inputs, outputs: outputs, selector: draft.Selector}
	for _, draftCase := range draft.Cases {
		graph, err := c.compileGraph(&draftCase.Graph)
		if err != nil {
			return Branch{}, fmt.Errorf("case %q: %w", draftCase.Name, err)
		}
		branch.cases = append(branch.cases, Case{name: draftCase.Name, graph: graph})
	}
	return branch, nil
}

func (c *compiler) compileMap(draft MapDraft) (Map, error) {
	inputs, err := cloneContract(draft.Inputs)
	if err != nil {
		return Map{}, fmt.Errorf("inputs: %w", err)
	}
	outputs, err := cloneContract(draft.Outputs)
	if err != nil {
		return Map{}, fmt.Errorf("outputs: %w", err)
	}
	body, err := c.compileGraph(&draft.Body)
	if err != nil {
		return Map{}, fmt.Errorf("body: %w", err)
	}
	return Map{inputs: inputs, outputs: outputs, collection: draft.Collection, result: draft.Result, body: body}, nil
}

func (c *compiler) compileLoop(draft LoopDraft) (Loop, error) {
	inputs, err := cloneContract(draft.Inputs)
	if err != nil {
		return Loop{}, fmt.Errorf("inputs: %w", err)
	}
	outputs, err := cloneContract(draft.Outputs)
	if err != nil {
		return Loop{}, fmt.Errorf("outputs: %w", err)
	}
	body, err := c.compileGraph(&draft.Body)
	if err != nil {
		return Loop{}, fmt.Errorf("body: %w", err)
	}
	return Loop{
		inputs: inputs, outputs: outputs, maximum: draft.Maximum,
		termination: append([]string(nil), draft.Termination...), body: body,
	}, nil
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
