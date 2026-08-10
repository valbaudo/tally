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

func validateCleanupBindings(protected, cleanup Graph, drafts []CleanupBindingDraft) ([]CleanupBinding, error) {
	bound := make(map[string]struct{}, len(drafts))
	bindings := make([]CleanupBinding, 0, len(drafts))
	for _, draft := range drafts {
		source, err := resolveCleanupSource(protected, draft.From)
		if err != nil {
			return nil, bindingError(protected, "cleanup source: %v", err)
		}
		target, ok := cleanup.inputs.Resolve(draft.To)
		if !ok {
			return nil, bindingError(protected, "cleanup target port %q: unknown target port %q", draft.To, draft.To)
		}
		if _, exists := bound[draft.To]; exists {
			return nil, bindingError(protected, "cleanup target port %q: input %s is bound more than once", draft.To, draft.To)
		}
		targetOptional := contractPathOptional(cleanup.inputs, []string{draft.To})
		if (draft.From.Kind == CleanupChild || source.optional) && !targetOptional {
			return nil, bindingError(protected, "cleanup source cannot satisfy required target %q", draft.To)
		}
		if draft.From.Kind == CleanupOutcome && !target.Equal(cleanupOutcomeType()) {
			return nil, bindingError(protected, "cleanup target port %q must use the exact outcome enum", draft.To)
		}
		assignment, err := value.CheckAssignable(source.typ, target)
		if err != nil {
			return nil, bindingError(protected, "cleanup source to target port %q cannot assign: %v", draft.To, err)
		}
		bound[draft.To] = struct{}{}
		bindings = append(bindings, CleanupBinding{
			from: CleanupSource{
				kind: draft.From.Kind, child: draft.From.Child,
				path: append([]string(nil), draft.From.Path...),
			},
			to: draft.To, runtimeValidation: assignment.RuntimeValidation,
		})
	}
	for _, target := range cleanup.inputs.Ports() {
		if target.Optional() {
			continue
		}
		if _, exists := bound[target.Name()]; !exists {
			return nil, bindingError(protected, "cleanup target port %q: required input is unbound", target.Name())
		}
	}
	return bindings, nil
}

func resolveCleanupSource(protected Graph, draft CleanupSourceDraft) (resolvedSource, error) {
	switch draft.Kind {
	case CleanupInput:
		if draft.Child != "" {
			return resolvedSource{}, fmt.Errorf("input source must not name a child")
		}
		if len(draft.Path) == 0 {
			return resolvedSource{}, fmt.Errorf("input source path is empty")
		}
		typ, ok := protected.inputs.Resolve(draft.Path...)
		if !ok {
			return resolvedSource{}, fmt.Errorf("unknown protected input %q", strings.Join(draft.Path, "."))
		}
		return resolvedSource{typ: typ, optional: contractPathOptional(protected.inputs, draft.Path)}, nil
	case CleanupOutcome:
		if draft.Child != "" {
			return resolvedSource{}, fmt.Errorf("outcome source must not name a child")
		}
		if len(draft.Path) != 0 {
			return resolvedSource{}, fmt.Errorf("outcome source must not have a path")
		}
		return resolvedSource{typ: cleanupOutcomeType()}, nil
	case CleanupChild:
		if draft.Child == "" {
			return resolvedSource{}, fmt.Errorf("child source has an empty child name")
		}
		if len(draft.Path) == 0 {
			return resolvedSource{}, fmt.Errorf("child source path is empty")
		}
		for _, node := range protected.nodes {
			if node.name != draft.Child {
				continue
			}
			_, outputs, ok := nodeContracts(node)
			if !ok {
				return resolvedSource{}, fmt.Errorf("child %q has no output contract", draft.Child)
			}
			typ, ok := outputs.Resolve(draft.Path...)
			if !ok {
				return resolvedSource{}, fmt.Errorf("unknown child %q output %q", draft.Child, strings.Join(draft.Path, "."))
			}
			return resolvedSource{typ: typ, optional: true}, nil
		}
		return resolvedSource{}, fmt.Errorf("unknown child %q", draft.Child)
	default:
		return resolvedSource{}, fmt.Errorf("unknown cleanup source kind %d", draft.Kind)
	}
}

func cleanupOutcomeType() value.Type {
	members := make([]value.Literal, 0, 4)
	for _, status := range []Status{Succeeded, Rejected, Failed, Cancelled} {
		literal, err := value.ParseLiteral([]byte(fmt.Sprintf("%q", status)))
		if err != nil {
			panic("fixed cleanup outcome literal is invalid")
		}
		members = append(members, literal)
	}
	typ, err := value.Enum(members...)
	if err != nil {
		panic("fixed cleanup outcome enum is invalid")
	}
	return typ
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

func validateBindings(graph Graph, drafts []EdgeDraft) ([]Edge, error) {
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
