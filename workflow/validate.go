package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/valbaudo/dawn/value"
)

func validateGraph(graph Graph, cleanup bool) error {
	if cleanup && !graph.outputs.Equal(value.EmptyContract()) {
		return fmt.Errorf("cleanup output contract must be explicitly empty")
	}
	if err := validateAcyclic(graph); err != nil {
		return err
	}
	nodes := append([]Node(nil), graph.nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].name < nodes[j].name })
	for _, node := range nodes {
		if err := validateNode(node, cleanup); err != nil {
			return fmt.Errorf("node %q: %w", node.name, err)
		}
	}
	if graph.cleanup != nil {
		if cleanup {
			return fmt.Errorf("finally cannot contain finally")
		}
		if err := validateGraph(graph.cleanup.graph, true); err != nil {
			return fmt.Errorf("finally: %w", err)
		}
	}
	return nil
}

func validateAcyclic(graph Graph) error {
	adjacent := make(map[string]map[string]struct{}, len(graph.nodes))
	for _, node := range graph.nodes {
		adjacent[node.name] = make(map[string]struct{})
	}
	for _, edge := range graph.edges {
		if edge.from.kind != Child || edge.to.kind != Child {
			continue
		}
		if _, ok := adjacent[edge.from.child]; !ok {
			return fmt.Errorf("edge source child %q is not in graph", edge.from.child)
		}
		if _, ok := adjacent[edge.to.child]; !ok {
			return fmt.Errorf("edge target child %q is not in graph", edge.to.child)
		}
		adjacent[edge.from.child][edge.to.child] = struct{}{}
	}

	names := make([]string, 0, len(adjacent))
	for name := range adjacent {
		names = append(names, name)
	}
	sort.Strings(names)

	const (
		unvisited uint8 = iota
		visiting
		visited
	)
	colors := make(map[string]uint8, len(names))
	stack := make([]string, 0, len(names))
	stackIndex := make(map[string]int, len(names))
	var visit func(string) error
	visit = func(name string) error {
		colors[name] = visiting
		stackIndex[name] = len(stack)
		stack = append(stack, name)
		neighbors := make([]string, 0, len(adjacent[name]))
		for neighbor := range adjacent[name] {
			neighbors = append(neighbors, neighbor)
		}
		sort.Strings(neighbors)
		for _, neighbor := range neighbors {
			switch colors[neighbor] {
			case unvisited:
				if err := visit(neighbor); err != nil {
					return err
				}
			case visiting:
				cycle := append([]string(nil), stack[stackIndex[neighbor]:]...)
				cycle = append(cycle, neighbor)
				return fmt.Errorf("dependency cycle: %s", strings.Join(normalizeCycle(cycle), " -> "))
			}
		}
		stack = stack[:len(stack)-1]
		delete(stackIndex, name)
		colors[name] = visited
		return nil
	}
	for _, name := range names {
		if colors[name] == unvisited {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeCycle(cycle []string) []string {
	if len(cycle) <= 2 {
		return cycle
	}
	body := cycle[:len(cycle)-1]
	first := 0
	for index := 1; index < len(body); index++ {
		if body[index] < body[first] {
			first = index
		}
	}
	normalized := append([]string(nil), body[first:]...)
	normalized = append(normalized, body[:first]...)
	return append(normalized, normalized[0])
}

func validateNode(node Node, cleanup bool) error {
	if node.leaf != nil {
		if node.leaf.kind == Gate {
			if cleanup {
				return fmt.Errorf("cleanup cannot contain gate")
			}
			return validateGate(*node.leaf)
		}
		return nil
	}
	if node.scope == nil {
		return fmt.Errorf("has no variant")
	}
	switch node.scope.kind {
	case GraphScope:
		if node.scope.graph == nil {
			return fmt.Errorf("graph scope is missing its graph")
		}
		inputs, outputs, ok := nodeContracts(node)
		if !ok || !inputs.Equal(node.scope.graph.inputs) || !outputs.Equal(node.scope.graph.outputs) {
			return fmt.Errorf("graph scope boundary contracts do not match its inner graph")
		}
		return validateGraph(*node.scope.graph, cleanup)
	case BranchScope:
		if node.scope.branch == nil {
			return fmt.Errorf("branch scope is missing its branch")
		}
		if err := validateBranch(*node.scope.branch); err != nil {
			return err
		}
		for _, branchCase := range node.scope.branch.cases {
			if err := validateGraph(branchCase.graph, cleanup); err != nil {
				return fmt.Errorf("branch case %q: %w", branchCase.name, err)
			}
		}
		return nil
	case MapScope:
		if node.scope.map_ == nil {
			return fmt.Errorf("map scope is missing its map")
		}
		if err := validateMap(*node.scope.map_); err != nil {
			return err
		}
		return validateGraph(node.scope.map_.body, cleanup)
	case LoopScope:
		if cleanup {
			return fmt.Errorf("cleanup cannot contain loop")
		}
		if node.scope.loop == nil {
			return fmt.Errorf("loop scope is missing its loop")
		}
		if err := validateLoop(*node.scope.loop); err != nil {
			return err
		}
		return validateGraph(node.scope.loop.body, false)
	default:
		return fmt.Errorf("unknown scope kind %q", node.scope.kind)
	}
}

func validateBranch(branch Branch) error {
	selector, ok := branch.inputs.Resolve(branch.selector)
	if !ok {
		return fmt.Errorf("branch selector %q is not an input", branch.selector)
	}
	wantCases := make([]string, 0)
	switch selector.Kind() {
	case value.BooleanKind:
		wantCases = []string{"false", "true"}
	case value.EnumKind:
		for _, member := range selector.EnumValues() {
			wantCases = append(wantCases, string(member.Bytes()))
		}
	default:
		return fmt.Errorf("branch selector %q must be boolean or enum", branch.selector)
	}
	gotCases := make([]string, 0, len(branch.cases))
	for _, branchCase := range branch.cases {
		if !branchCase.graph.inputs.Equal(branch.inputs) {
			return fmt.Errorf("branch case %q input contract does not match branch", branchCase.name)
		}
		if !branchCase.graph.outputs.Equal(branch.outputs) {
			return fmt.Errorf("branch case %q output contract does not match branch", branchCase.name)
		}
		gotCases = append(gotCases, branchCase.name)
	}
	sort.Strings(gotCases)
	if len(gotCases) != len(wantCases) {
		return fmt.Errorf("branch cases must be exactly %s", strings.Join(wantCases, ", "))
	}
	for index := range wantCases {
		if gotCases[index] != wantCases[index] {
			return fmt.Errorf("branch cases must be exactly %s", strings.Join(wantCases, ", "))
		}
	}
	return nil
}

func validateMap(mapped Map) error {
	inputs := mapped.inputs.Ports()
	if len(inputs) != 1 || inputs[0].Name() != mapped.collection || inputs[0].Type().Kind() != value.ListKind {
		return fmt.Errorf("map collection must be its only list input")
	}
	element, _ := inputs[0].Type().Element()
	item, err := value.Required("item", element)
	if err != nil {
		return err
	}
	index, err := value.Required("index", value.Integer())
	if err != nil {
		return err
	}
	wantInputs, err := value.NewContract(
		item,
		index,
	)
	if err != nil {
		return err
	}
	if !mapped.body.inputs.Equal(wantInputs) {
		return fmt.Errorf("map body input contract must be required item and index")
	}
	wantOutput, err := value.List(mapped.body.outputs.ObjectType())
	if err != nil {
		return err
	}
	result, err := value.Required(mapped.result, wantOutput)
	if err != nil {
		return fmt.Errorf("map result: %w", err)
	}
	wantOutputs, err := value.NewContract(result)
	if err != nil {
		return err
	}
	if !mapped.outputs.Equal(wantOutputs) {
		return fmt.Errorf("map result output contract must be list of the body output object")
	}
	return nil
}

func validateLoop(loop Loop) error {
	if loop.maximum < 1 {
		return fmt.Errorf("loop maximum must be a positive integer")
	}
	initial, err := value.Required("initial", loop.inputs.ObjectType())
	if err != nil {
		return err
	}
	previous, err := value.Optional("previous", loop.body.outputs.ObjectType())
	if err != nil {
		return err
	}
	iteration, err := value.Required("iteration", value.Integer())
	if err != nil {
		return err
	}
	wantInputs, err := value.NewContract(
		initial,
		previous,
		iteration,
	)
	if err != nil {
		return err
	}
	if !loop.body.inputs.Equal(wantInputs) {
		return fmt.Errorf("loop body input contract must be initial, previous, and iteration")
	}
	termination, ok := loop.body.outputs.Resolve(loop.termination...)
	if !ok || termination.Kind() != value.BooleanKind {
		return fmt.Errorf("loop termination path must resolve to boolean body output")
	}
	if !loop.outputs.Equal(loop.body.outputs) {
		return fmt.Errorf("loop output contract must equal its body output contract")
	}
	return nil
}

func validateGate(gate Leaf) error {
	passed, err := value.Required("passed", value.Boolean())
	if err != nil {
		return err
	}
	reason, err := value.Optional("reason", value.String())
	if err != nil {
		return err
	}
	wantInputs, err := value.NewContract(
		passed,
		reason,
	)
	if err != nil {
		return err
	}
	if !gate.inputs.Equal(wantInputs) {
		return fmt.Errorf("gate inputs must be required passed boolean and optional reason string")
	}
	if !gate.outputs.Equal(value.EmptyContract()) {
		return fmt.Errorf("gate outputs must be empty")
	}
	return nil
}
