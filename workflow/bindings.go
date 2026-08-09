package workflow

import (
	"fmt"
	"strings"

	"github.com/valbaudo/dawn/value"
)

type resolvedEndpoint struct {
	kind            EndpointKind
	child           int
	inputs, outputs value.Contract
}

type resolvedSource struct {
	typ      value.Type
	optional bool
}

func resolveEndpoint(graph Graph, endpoint EndpointDraft) (resolvedEndpoint, error) {
	switch endpoint.Kind {
	case Boundary:
		if endpoint.Child != "" {
			return resolvedEndpoint{}, fmt.Errorf("boundary endpoint must not name child %q", endpoint.Child)
		}
		return resolvedEndpoint{kind: Boundary, child: -1, inputs: graph.inputs, outputs: graph.outputs}, nil
	case Child:
		if endpoint.Child == "" {
			return resolvedEndpoint{}, fmt.Errorf("child endpoint has an empty child name")
		}
		if strings.Contains(endpoint.Child, "/") {
			return resolvedEndpoint{}, fmt.Errorf("unknown child %q", endpoint.Child)
		}
		for index, node := range graph.nodes {
			if node.name == endpoint.Child {
				inputs, outputs, ok := nodeContracts(node)
				if !ok {
					return resolvedEndpoint{}, fmt.Errorf("child %q has no boundary contracts", endpoint.Child)
				}
				return resolvedEndpoint{kind: Child, child: index, inputs: inputs, outputs: outputs}, nil
			}
		}
		return resolvedEndpoint{}, fmt.Errorf("unknown child %q", endpoint.Child)
	default:
		return resolvedEndpoint{}, fmt.Errorf("unknown endpoint kind %d", endpoint.Kind)
	}
}

func resolveSource(endpoint resolvedEndpoint, path []string) (resolvedSource, error) {
	contract := endpoint.outputs
	if endpoint.kind == Boundary {
		contract = endpoint.inputs
	}
	typ, ok := contract.Resolve(path...)
	if !ok {
		return resolvedSource{}, fmt.Errorf("unknown source port %q", strings.Join(path, "."))
	}
	return resolvedSource{typ: typ, optional: contractPathOptional(contract, path)}, nil
}

func resolveTarget(endpoint resolvedEndpoint, port string) (value.Type, error) {
	contract := endpoint.inputs
	if endpoint.kind == Boundary {
		contract = endpoint.outputs
	}
	typ, ok := contract.Resolve(port)
	if !ok {
		return value.Type{}, fmt.Errorf("unknown target port %q", port)
	}
	return typ, nil
}

type targetKey struct {
	endpoint Endpoint
	port     string
}

func bindTarget(bound map[targetKey]struct{}, key targetKey) error {
	if _, exists := bound[key]; exists {
		return fmt.Errorf("input %s is bound more than once", key.port)
	}
	bound[key] = struct{}{}
	return nil
}

func validateBindings(graph Graph, drafts []EdgeDraft, parallel bool) ([]Edge, error) {
	bound := make(map[targetKey]struct{})
	edges := make([]Edge, 0, len(drafts))
	for _, draft := range drafts {
		from, err := resolveEndpoint(graph, draft.From)
		if err != nil {
			if len(draft.Bindings) == 0 {
				return nil, bindingError(graph, "edge source endpoint %s: %v", describeEndpoint(draft.From), err)
			}
			return nil, bindingError(graph, "edge source endpoint %s port %q: %v", describeEndpoint(draft.From), endpointBindingPort(draft, true), err)
		}
		to, err := resolveEndpoint(graph, draft.To)
		if err != nil {
			if len(draft.Bindings) == 0 {
				return nil, bindingError(graph, "edge target endpoint %s: %v", describeEndpoint(draft.To), err)
			}
			return nil, bindingError(graph, "edge target endpoint %s port %q: %v", describeEndpoint(draft.To), endpointBindingPort(draft, false), err)
		}
		if parallel && from.kind == Child && to.kind == Child {
			if len(draft.Bindings) == 0 {
				return nil, bindingError(graph, "parallel edge from %s to %s: child-to-child ordering is not allowed", describeEndpoint(draft.From), describeEndpoint(draft.To))
			}
			return nil, bindingError(graph, "parallel edge from %s port %q to %s port %q: child-to-child ordering is not allowed", describeEndpoint(draft.From), endpointBindingPort(draft, true), describeEndpoint(draft.To), endpointBindingPort(draft, false))
		}

		edge := Edge{
			from: endpointFromDraft(draft.From),
			to:   endpointFromDraft(draft.To),
		}
		if len(draft.Bindings) == 0 {
			if from.kind != Child || to.kind != Child || from.child == to.child {
				return nil, bindingError(graph, "completion-only edge from %s to %s requires distinct child endpoints", describeEndpoint(draft.From), describeEndpoint(draft.To))
			}
			edges = append(edges, edge)
			continue
		}

		for _, draftBinding := range draft.Bindings {
			source, err := resolveSource(from, draftBinding.From)
			if err != nil {
				return nil, bindingError(graph, "edge source endpoint %s port %q: %v", describeEndpoint(draft.From), strings.Join(draftBinding.From, "."), err)
			}
			toType, err := resolveTarget(to, draftBinding.To)
			if err != nil {
				return nil, bindingError(graph, "edge target endpoint %s port %q: %v", describeEndpoint(draft.To), draftBinding.To, err)
			}
			if source.optional && !targetOptional(to, draftBinding.To) {
				return nil, bindingError(graph, "edge from %s port %q to %s port %q: optional source cannot satisfy required target", describeEndpoint(draft.From), strings.Join(draftBinding.From, "."), describeEndpoint(draft.To), draftBinding.To)
			}
			assignment, err := value.CheckAssignable(source.typ, toType)
			if err != nil {
				return nil, bindingError(graph, "edge from %s port %q to %s port %q: %v", describeEndpoint(draft.From), strings.Join(draftBinding.From, "."), describeEndpoint(draft.To), draftBinding.To, err)
			}
			key := targetKey{endpoint: edge.to, port: draftBinding.To}
			if err := bindTarget(bound, key); err != nil {
				return nil, bindingError(graph, "edge target endpoint %s port %q: %v", describeEndpoint(draft.To), draftBinding.To, err)
			}
			edge.bindings = append(edge.bindings, Binding{
				from:              append([]string(nil), draftBinding.From...),
				to:                draftBinding.To,
				runtimeValidation: assignment.RuntimeValidation,
			})
		}
		edges = append(edges, edge)
	}

	for child, node := range graph.nodes {
		endpoint := Endpoint{kind: Child, child: node.name}
		for _, literal := range node.literals {
			target, err := resolveTarget(resolvedEndpoint{kind: Child, child: child, inputs: mustNodeInputs(node)}, literal.input)
			if err != nil {
				return nil, bindingError(graph, "literal target endpoint %s port %q: %v", describeEndpointDraft(endpoint), literal.input, err)
			}
			if err := target.ValidateLiteral(literal.value); err != nil {
				return nil, bindingError(graph, "literal target endpoint %s port %q: %v", describeEndpointDraft(endpoint), literal.input, err)
			}
			key := targetKey{endpoint: endpoint, port: literal.input}
			if err := bindTarget(bound, key); err != nil {
				return nil, bindingError(graph, "literal target endpoint %s port %q: %v", describeEndpointDraft(endpoint), literal.input, err)
			}
		}
	}

	for _, node := range graph.nodes {
		inputs := mustNodeInputs(node)
		for _, field := range inputs.Ports() {
			if field.Optional() {
				continue
			}
			key := targetKey{endpoint: Endpoint{kind: Child, child: node.name}, port: field.Name()}
			if _, exists := bound[key]; !exists {
				return nil, bindingError(graph, "child endpoint %s port %q: required input is unbound", describeEndpointDraft(key.endpoint), field.Name())
			}
		}
	}
	for _, field := range graph.outputs.Ports() {
		if field.Optional() {
			continue
		}
		key := targetKey{endpoint: Endpoint{kind: Boundary}, port: field.Name()}
		if _, exists := bound[key]; !exists {
			return nil, bindingError(graph, "target endpoint boundary port %q: required output is unbound", field.Name())
		}
	}
	return edges, nil
}

func contractPathOptional(contract value.Contract, path []string) bool {
	if len(path) == 0 {
		return false
	}
	field, ok := fieldNamed(contract.Ports(), path[0])
	if !ok {
		return false
	}
	optional := field.Optional()
	typ := field.Type()
	for _, name := range path[1:] {
		field, ok = fieldNamed(typ.Fields(), name)
		if !ok {
			return optional
		}
		optional = optional || field.Optional()
		typ = field.Type()
	}
	return optional
}

func targetOptional(endpoint resolvedEndpoint, port string) bool {
	contract := endpoint.inputs
	if endpoint.kind == Boundary {
		contract = endpoint.outputs
	}
	field, ok := fieldNamed(contract.Ports(), port)
	return ok && field.Optional()
}

func fieldNamed(fields []value.Field, name string) (value.Field, bool) {
	for _, field := range fields {
		if field.Name() == name {
			return field, true
		}
	}
	return value.Field{}, false
}

func nodeContracts(node Node) (value.Contract, value.Contract, bool) {
	if node.leaf != nil {
		return node.leaf.inputs, node.leaf.outputs, true
	}
	if node.scope == nil {
		return value.Contract{}, value.Contract{}, false
	}
	switch node.scope.kind {
	case GraphScope:
		if node.scope.graph != nil {
			return node.scope.graph.inputs, node.scope.graph.outputs, true
		}
	case BranchScope:
		if node.scope.branch != nil {
			return node.scope.branch.inputs, node.scope.branch.outputs, true
		}
	case MapScope:
		if node.scope.map_ != nil {
			return node.scope.map_.inputs, node.scope.map_.outputs, true
		}
	case LoopScope:
		if node.scope.loop != nil {
			return node.scope.loop.inputs, node.scope.loop.outputs, true
		}
	}
	return value.Contract{}, value.Contract{}, false
}

func mustNodeInputs(node Node) value.Contract {
	inputs, _, ok := nodeContracts(node)
	if !ok {
		panic("compiled node has no input contract")
	}
	return inputs
}

func endpointFromDraft(draft EndpointDraft) Endpoint {
	return Endpoint{kind: draft.Kind, child: draft.Child}
}

func endpointBindingPort(draft EdgeDraft, source bool) string {
	if len(draft.Bindings) == 0 {
		return ""
	}
	if source {
		return strings.Join(draft.Bindings[0].From, ".")
	}
	return draft.Bindings[0].To
}

func describeEndpoint(draft EndpointDraft) string {
	return describeEndpointDraft(endpointFromDraft(draft))
}

func describeEndpointDraft(endpoint Endpoint) string {
	if endpoint.kind == Boundary {
		return "boundary"
	}
	return fmt.Sprintf("child %q", endpoint.child)
}

func bindingError(graph Graph, format string, args ...any) error {
	return fmt.Errorf("graph source %q module %q: %s", graph.provenance.Source, graph.provenance.Module, fmt.Sprintf(format, args...))
}
