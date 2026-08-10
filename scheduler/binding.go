package scheduler

import (
	"fmt"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

// selectValue walks statically declared object fields without recursion.
func selectValue(root value.Value, path []string) (value.Value, bool) {
	current := root
	for _, segment := range path {
		if current.Kind() != value.ObjectKind {
			return value.Value{}, false
		}
		found := false
		for _, entry := range current.Entries() {
			if entry.Name() == segment {
				current = entry.Value()
				found = true
				break
			}
		}
		if !found {
			return value.Value{}, false
		}
	}
	if len(path) == 0 || !current.Valid() {
		return value.Value{}, false
	}
	return current, true
}

func objectValue(fields map[string]value.Value) (value.Value, error) {
	entries := make([]value.Entry, 0, len(fields))
	for name, fieldValue := range fields {
		entry, err := value.NewEntry(name, fieldValue)
		if err != nil {
			return value.Value{}, err
		}
		entries = append(entries, entry)
	}
	return value.NewObject(entries...)
}

type graphNodeState struct {
	node      workflow.Node
	inputs    map[string]value.Value
	remaining int
	started   bool
	done      bool
}

type graphState struct {
	graph     workflow.Graph
	input     value.Value
	nodes     []graphNodeState
	byName    map[string]int
	outputs   map[string]value.Value
	committed map[string]value.Value
}

// applyGraphInputs resolves boundary bindings and typed literals. Child-source
// bindings remain pending until that child's boundary has committed.
func applyGraphInputs(graph workflow.Graph, input value.Value) (*graphState, error) {
	state := &graphState{
		graph: graph, input: input, byName: make(map[string]int), outputs: make(map[string]value.Value), committed: make(map[string]value.Value),
	}
	for index, node := range graph.Nodes() {
		state.byName[node.Name()] = index
		state.nodes = append(state.nodes, graphNodeState{node: node, inputs: make(map[string]value.Value)})
	}
	for index := range state.nodes {
		node := state.nodes[index].node
		inputs, ok := nodeInputContract(node)
		if !ok {
			return nil, fmt.Errorf("node %q has no executable boundary", node.Name())
		}
		for _, literal := range node.Literals() {
			target, found := inputs.Resolve(literal.Input())
			if !found {
				return nil, fmt.Errorf("node %q literal has unknown target %q", node.Name(), literal.Input())
			}
			materialized, err := value.MaterializeLiteral(literal.Value(), target)
			if err != nil {
				return nil, fmt.Errorf("node %q literal %q: %w", node.Name(), literal.Input(), err)
			}
			state.nodes[index].inputs[literal.Input()] = materialized
		}
	}

	for _, edge := range graph.Edges() {
		from, to := edge.From(), edge.To()
		if from.Kind() == workflow.Child && to.Kind() == workflow.Child {
			index, ok := state.byName[to.Child()]
			if !ok {
				return nil, fmt.Errorf("unknown edge target %q", to.Child())
			}
			state.nodes[index].remaining++
		}
		if from.Kind() != workflow.Boundary {
			continue
		}
		for _, binding := range edge.Bindings() {
			selected, present := selectValue(input, binding.From())
			if !present {
				continue
			}
			if to.Kind() == workflow.Boundary {
				state.outputs[binding.To()] = selected
				continue
			}
			index, ok := state.byName[to.Child()]
			if !ok {
				return nil, fmt.Errorf("unknown edge target %q", to.Child())
			}
			state.nodes[index].inputs[binding.To()] = selected
		}
	}
	return state, nil
}

func (s *graphState) childInput(index int) (value.Value, error) {
	return objectValue(s.nodes[index].inputs)
}

func (s *graphState) applyChildOutput(name string, output value.Value) error {
	for _, edge := range s.graph.Edges() {
		if edge.From().Kind() != workflow.Child || edge.From().Child() != name {
			continue
		}
		to := edge.To()
		for _, binding := range edge.Bindings() {
			selected, present := selectValue(output, binding.From())
			if !present {
				continue
			}
			if to.Kind() == workflow.Boundary {
				s.outputs[binding.To()] = selected
			} else {
				index, ok := s.byName[to.Child()]
				if !ok {
					return fmt.Errorf("unknown edge target %q", to.Child())
				}
				s.nodes[index].inputs[binding.To()] = selected
			}
		}
		if to.Kind() == workflow.Child {
			index := s.byName[to.Child()]
			s.nodes[index].remaining--
		}
	}
	return nil
}

func (s *graphState) recordCommittedChild(name string, output value.Value) {
	if s == nil || !output.Valid() {
		return
	}
	s.committed[name] = output
}

func (s *graphState) committedChild(name string) (value.Value, bool) {
	if s == nil {
		return value.Value{}, false
	}
	output, present := s.committed[name]
	return output, present && output.Valid()
}

func nodeInputContract(node workflow.Node) (value.Contract, bool) {
	if leaf, ok := node.Leaf(); ok {
		return leaf.Inputs(), true
	}
	if scope, ok := node.Scope(); ok {
		switch scope.Kind() {
		case workflow.GraphScope:
			graph, present := scope.Graph()
			if present {
				return graph.Inputs(), true
			}
		case workflow.BranchScope:
			branch, present := scope.Branch()
			if present {
				return branch.Inputs(), true
			}
		case workflow.MapScope:
			mapped, present := scope.Map()
			if present {
				return mapped.Inputs(), true
			}
		case workflow.LoopScope:
			loop, present := scope.Loop()
			if present {
				return loop.Inputs(), true
			}
		}
	}
	return value.Contract{}, false
}
