package scheduler

import (
	"container/heap"
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
	queued    bool
	started   bool
	done      bool
}

type graphExecutionBinding struct {
	from []string
	to   string
}

type graphExecutionEdge struct {
	target   int
	bindings []graphExecutionBinding
}

type graphReadyNode struct {
	index int
	name  string
}

type graphReadyHeap []graphReadyNode

func (h graphReadyHeap) Len() int { return len(h) }

func (h graphReadyHeap) Less(left, right int) bool { return h[left].name < h[right].name }

func (h graphReadyHeap) Swap(left, right int) { h[left], h[right] = h[right], h[left] }

func (h *graphReadyHeap) Push(item any) { *h = append(*h, item.(graphReadyNode)) }

func (h *graphReadyHeap) Pop() any {
	old := *h
	last := len(old) - 1
	item := old[last]
	old[last] = graphReadyNode{}
	*h = old[:last]
	return item
}

type graphState struct {
	nodes      []graphNodeState
	byName     map[string]int
	outputs    map[string]value.Value
	committed  map[string]value.Value
	outgoing   [][]graphExecutionEdge
	ready      graphReadyHeap
	unfinished int
}

// applyGraphInputs resolves boundary bindings and typed literals. Child-source
// bindings remain pending until that child's boundary has committed.
func applyGraphInputs(graph workflow.Graph, input value.Value) (*graphState, error) {
	state := &graphState{
		byName: make(map[string]int), outputs: make(map[string]value.Value), committed: make(map[string]value.Value),
	}
	for index, node := range graph.Nodes() {
		state.byName[node.Name()] = index
		state.nodes = append(state.nodes, graphNodeState{node: node, inputs: make(map[string]value.Value)})
	}
	state.outgoing = make([][]graphExecutionEdge, len(state.nodes))
	state.unfinished = len(state.nodes)
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
		target := -1
		if to.Kind() == workflow.Child {
			index, ok := state.byName[to.Child()]
			if !ok {
				return nil, fmt.Errorf("unknown edge target %q", to.Child())
			}
			target = index
		}
		bindings := make([]graphExecutionBinding, 0, len(edge.Bindings()))
		for _, binding := range edge.Bindings() {
			bindings = append(bindings, graphExecutionBinding{from: binding.From(), to: binding.To()})
		}
		if from.Kind() == workflow.Child {
			source, ok := state.byName[from.Child()]
			if !ok {
				return nil, fmt.Errorf("unknown edge source %q", from.Child())
			}
			state.outgoing[source] = append(state.outgoing[source], graphExecutionEdge{target: target, bindings: bindings})
			if target >= 0 {
				state.nodes[target].remaining++
			}
			continue
		}
		for _, binding := range bindings {
			selected, present := selectValue(input, binding.from)
			if !present {
				continue
			}
			if target < 0 {
				state.outputs[binding.to] = selected
				continue
			}
			state.nodes[target].inputs[binding.to] = selected
		}
	}
	for index := range state.nodes {
		if state.nodes[index].remaining == 0 {
			state.enqueueReady(index)
		}
	}
	return state, nil
}

func (s *graphState) childInput(index int) (value.Value, error) {
	return objectValue(s.nodes[index].inputs)
}

func (s *graphState) applyChildOutput(index int, output value.Value) error {
	for _, edge := range s.outgoing[index] {
		for _, binding := range edge.bindings {
			selected, present := selectValue(output, binding.from)
			if !present {
				continue
			}
			if edge.target < 0 {
				s.outputs[binding.to] = selected
			} else {
				s.nodes[edge.target].inputs[binding.to] = selected
			}
		}
		if edge.target >= 0 {
			s.nodes[edge.target].remaining--
			if s.nodes[edge.target].remaining == 0 {
				s.enqueueReady(edge.target)
			}
		}
	}
	return nil
}

func (s *graphState) enqueueReady(index int) {
	node := &s.nodes[index]
	if node.queued || node.started || node.done || node.remaining != 0 {
		return
	}
	node.queued = true
	heap.Push(&s.ready, graphReadyNode{index: index, name: node.node.Name()})
}

func (s *graphState) popReady() (int, bool) {
	if len(s.ready) == 0 {
		return 0, false
	}
	ready := heap.Pop(&s.ready).(graphReadyNode)
	s.nodes[ready.index].queued = false
	return ready.index, true
}

func (s *graphState) markDone(index int) {
	if s.nodes[index].done {
		return
	}
	s.nodes[index].done = true
	s.unfinished--
}

func (s *graphState) allDone() bool { return s.unfinished == 0 }

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
