package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

// Environment owns all private filesystem state for one invocation.
type Environment struct {
	root      string
	workspace string
	manifest  Manifest
	outputs   value.Contract
	plans     map[string]outputPlan
	targets   map[string]Target
	members   map[string]string
	mu        sync.Mutex
	closed    bool
}

// Target is one writable declared file or tree output location.
type Target struct {
	valuePath        value.Path
	kind             value.Kind
	location         string
	relativeLocation string
}

// ValuePath returns a defensive copy of the concrete structured output path.
func (t Target) ValuePath() value.Path { return cloneValuePath(t.valuePath) }

// Kind returns FileKind or TreeKind.
func (t Target) Kind() value.Kind { return t.kind }

// Location returns the absolute writable output target.
func (t Target) Location() string { return t.location }

// RelativeLocation returns the deterministic slash-separated runtime layout.
func (t Target) RelativeLocation() string { return t.relativeLocation }

// Prepare allocates one fresh private filesystem environment.
func Prepare(ctx context.Context, repository *content.Repository, leaf workflow.Leaf, input value.Value) (_ *Environment, err error) {
	if ctx == nil {
		return nil, fmt.Errorf("workspace: context must not be nil")
	}
	if repository == nil {
		return nil, fmt.Errorf("workspace: content repository must not be nil")
	}
	if leaf.Kind() != workflow.Agent && leaf.Kind() != workflow.Script {
		return nil, fmt.Errorf("workspace: leaf kind %q has no filesystem role", leaf.Kind())
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("workspace: prepare: %w", err)
	}
	if err := leaf.Inputs().Validate(input); err != nil {
		return nil, fmt.Errorf("workspace: input does not satisfy leaf contract: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("workspace: prepare: %w", err)
	}
	root, err := os.MkdirTemp("", "dawn-workspace-")
	if err != nil {
		return nil, fmt.Errorf("workspace: allocate runtime root: %w", err)
	}
	prepared := false
	defer func() {
		if !prepared {
			if cleanupErr := removeRuntimeRoot(root); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("workspace: remove failed runtime root: %w", cleanupErr))
			}
		}
	}()
	working := filepath.Join(root, "workspace")
	inputRoot := filepath.Join(root, "inputs")
	if err := os.Mkdir(inputRoot, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create input root: %w", err)
	}
	outputRoot := filepath.Join(root, "outputs")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create output root: %w", err)
	}
	manifestInputs, baseFound, err := materializeInputs(ctx, repository, leaf.Inputs(), input, inputRoot, working, leaf.BaseTree())
	if err != nil {
		return nil, err
	}
	if len(leaf.BaseTree()) != 0 && !baseFound {
		return nil, fmt.Errorf("workspace: compiled base tree input was not present")
	}
	if !baseFound {
		if err := os.Mkdir(working, 0o700); err != nil {
			return nil, fmt.Errorf("workspace: create working directory: %w", err)
		}
	}
	manifestOutputs, plans, targets, err := prepareOutputs(ctx, leaf.Outputs(), leaf.PublishWorkspace(), outputRoot)
	if err != nil {
		return nil, err
	}
	prepared = true
	return &Environment{
		root: root, workspace: working,
		manifest: Manifest{inputs: manifestInputs, outputs: manifestOutputs},
		outputs:  leaf.Outputs(), plans: plans, targets: targets, members: make(map[string]string),
	}, nil
}

// Workspace returns the absolute path of the invocation's private writable
// working directory.
func (e *Environment) Workspace() string {
	if e == nil {
		return ""
	}
	return e.workspace
}

// Manifest returns the invocation's immutable manifest.
func (e *Environment) Manifest() Manifest {
	if e == nil {
		return Manifest{}
	}
	return e.manifest
}

// Output resolves one concrete declared file or tree output path.
func (e *Environment) Output(path value.Path) (Target, error) {
	if e == nil {
		return Target{}, fmt.Errorf("workspace: environment is nil")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Target{}, fmt.Errorf("workspace: environment is closed")
	}
	typ, schema, err := resolveOutputSchema(e.outputs, path)
	if err != nil {
		return Target{}, err
	}
	if typ.Kind() != value.FileKind && typ.Kind() != value.TreeKind {
		return Target{}, fmt.Errorf("workspace: output path %s does not select a file or tree", path.String())
	}
	plan, exists := e.plans[string(canonicalSchema(schema))]
	if !exists || plan.kind != typ.Kind() {
		return Target{}, fmt.Errorf("workspace: output path %s has no named output slot", path.String())
	}
	concrete := canonicalPath(path)
	key := string(concrete)
	if target, exists := e.targets[key]; exists {
		return target, nil
	}
	if !plan.dynamic {
		return Target{}, fmt.Errorf("workspace: static output target is unavailable")
	}
	memberID := pathSlotIdentity("dawn.workspace.output.member/1", concrete)
	physicalIdentity := plan.location + "\x00" + memberID
	if prior, exists := e.members[physicalIdentity]; exists {
		if prior != key {
			return Target{}, fmt.Errorf("workspace: internal dynamic output slot identity collision")
		}
		return Target{}, fmt.Errorf("workspace: duplicate dynamic output slot identity")
	}
	e.members[physicalIdentity] = key
	memberRoot := filepath.Join(plan.location, memberID)
	if err := os.Mkdir(memberRoot, 0o700); err != nil {
		delete(e.members, physicalIdentity)
		return Target{}, fmt.Errorf("workspace: create dynamic output member: %w", err)
	}
	targetPath := filepath.Join(memberRoot, "value")
	if plan.kind == value.TreeKind {
		if err := os.Mkdir(targetPath, 0o700); err != nil {
			delete(e.members, physicalIdentity)
			_ = os.RemoveAll(memberRoot)
			return Target{}, fmt.Errorf("workspace: create dynamic tree output target: %w", err)
		}
	}
	target := Target{
		valuePath: cloneValuePath(path), kind: plan.kind, location: targetPath,
		relativeLocation: filepath.ToSlash(filepath.Join(plan.relativeLocation, memberID, "value")),
	}
	e.targets[key] = target
	return target, nil
}

// Close removes all private state. It is safe to call repeatedly.
func (e *Environment) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return removeRuntimeRoot(e.root)
}

type inputTask struct {
	typ   value.Type
	value value.Value
	path  value.Path
}

func materializeInputs(ctx context.Context, repository *content.Repository, contract value.Contract, input value.Value, inputRoot, working string, base []string) ([]Input, bool, error) {
	entries := input.Entries()
	ports := contract.Ports()
	tasks := make([]inputTask, 0, len(entries))
	entryIndex := 0
	for _, port := range ports {
		if entryIndex == len(entries) || entries[entryIndex].Name() != port.Name() {
			continue
		}
		tasks = append(tasks, inputTask{typ: port.Type(), value: entries[entryIndex].Value(), path: value.Path{}.Field(port.Name())})
		entryIndex++
	}
	for left, right := 0, len(tasks)-1; left < right; left, right = left+1, right-1 {
		tasks[left], tasks[right] = tasks[right], tasks[left]
	}

	basePath := pathFromFields(base)
	baseIdentity := canonicalPath(basePath)
	baseFound := false
	records := make([]Input, 0)
	identities := make(map[string]string)
	for len(tasks) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, false, fmt.Errorf("workspace: prepare inputs: %w", err)
		}
		last := len(tasks) - 1
		task := tasks[last]
		tasks = tasks[:last]
		switch task.typ.Kind() {
		case value.ObjectKind:
			children, err := objectInputTasks(task)
			if err != nil {
				return nil, false, err
			}
			for index := len(children) - 1; index >= 0; index-- {
				tasks = append(tasks, children[index])
			}
		case value.MapKind:
			element, ok := task.typ.Element()
			if !ok {
				return nil, false, fmt.Errorf("workspace: invalid compiled map input type")
			}
			entries := task.value.Entries()
			for index := len(entries) - 1; index >= 0; index-- {
				tasks = append(tasks, inputTask{typ: element, value: entries[index].Value(), path: task.path.MapKey(entries[index].Name())})
			}
		case value.ListKind:
			element, ok := task.typ.Element()
			if !ok {
				return nil, false, fmt.Errorf("workspace: invalid compiled list input type")
			}
			items := task.value.Items()
			for index := len(items) - 1; index >= 0; index-- {
				tasks = append(tasks, inputTask{typ: element, value: items[index], path: task.path.ListIndex(index)})
			}
		case value.FileKind:
			file, ok := task.value.File()
			if !ok {
				return nil, false, fmt.Errorf("workspace: validated file input is unavailable")
			}
			if len(base) != 0 && string(canonicalPath(task.path)) == string(baseIdentity) {
				return nil, false, fmt.Errorf("workspace: compiled base tree path resolved to a file")
			}
			record, err := materializeInputFile(ctx, repository, file, task.path, inputRoot, identities)
			if err != nil {
				return nil, false, err
			}
			records = append(records, record)
		case value.TreeKind:
			tree, ok := task.value.Tree()
			if !ok {
				return nil, false, fmt.Errorf("workspace: validated tree input is unavailable")
			}
			if len(base) != 0 && string(canonicalPath(task.path)) == string(baseIdentity) {
				if baseFound {
					return nil, false, fmt.Errorf("workspace: duplicate base tree input")
				}
				if err := repository.MaterializeTree(ctx, tree, working); err != nil {
					return nil, false, fmt.Errorf("workspace: materialize base tree: %w", err)
				}
				copy := tree
				records = append(records, Input{valuePath: cloneValuePath(task.path), kind: value.TreeKind, location: working, relativeLocation: "workspace", workspace: true, tree: &copy})
				baseFound = true
				continue
			}
			record, err := materializeInputTree(ctx, repository, tree, task.path, inputRoot, identities)
			if err != nil {
				return nil, false, err
			}
			records = append(records, record)
		}
	}
	return records, baseFound, nil
}

func objectInputTasks(parent inputTask) ([]inputTask, error) {
	fields := parent.typ.Fields()
	entries := parent.value.Entries()
	children := make([]inputTask, 0, len(entries))
	entryIndex := 0
	for _, field := range fields {
		if entryIndex == len(entries) || entries[entryIndex].Name() != field.Name() {
			continue
		}
		children = append(children, inputTask{typ: field.Type(), value: entries[entryIndex].Value(), path: parent.path.Field(field.Name())})
		entryIndex++
	}
	if entryIndex != len(entries) {
		return nil, fmt.Errorf("workspace: validated object input did not match its contract")
	}
	return children, nil
}

func materializeInputFile(ctx context.Context, repository *content.Repository, file content.File, path value.Path, inputRoot string, identities map[string]string) (Input, error) {
	slot, relative, err := allocateFixedSlot(inputRoot, "inputs", "dawn.workspace.input/1", path, identities)
	if err != nil {
		return Input{}, err
	}
	target := filepath.Join(slot, "value")
	if err := repository.MaterializeFile(ctx, file, target); err != nil {
		return Input{}, fmt.Errorf("workspace: materialize file input %s: %w", path.String(), err)
	}
	bestEffortReadOnly(target)
	copy := file
	return Input{valuePath: cloneValuePath(path), kind: value.FileKind, location: target, relativeLocation: filepath.ToSlash(filepath.Join(relative, "value")), file: &copy}, nil
}

func materializeInputTree(ctx context.Context, repository *content.Repository, tree content.Tree, path value.Path, inputRoot string, identities map[string]string) (Input, error) {
	slot, relative, err := allocateFixedSlot(inputRoot, "inputs", "dawn.workspace.input/1", path, identities)
	if err != nil {
		return Input{}, err
	}
	target := filepath.Join(slot, "value")
	if err := repository.MaterializeTree(ctx, tree, target); err != nil {
		return Input{}, fmt.Errorf("workspace: materialize tree input %s: %w", path.String(), err)
	}
	bestEffortReadOnly(target)
	copy := tree
	return Input{valuePath: cloneValuePath(path), kind: value.TreeKind, location: target, relativeLocation: filepath.ToSlash(filepath.Join(relative, "value")), tree: &copy}, nil
}

func allocateFixedSlot(parent, relativeParent, domain string, path value.Path, identities map[string]string) (string, string, error) {
	canonical := canonicalPath(path)
	id := pathSlotIdentity(domain, canonical)
	if prior, exists := identities[id]; exists {
		if prior == string(canonical) {
			return "", "", fmt.Errorf("workspace: duplicate materialized value path %s", path.String())
		}
		return "", "", fmt.Errorf("workspace: internal slot identity collision")
	}
	identities[id] = string(canonical)
	slot := filepath.Join(parent, id)
	if err := os.Mkdir(slot, 0o700); err != nil {
		return "", "", fmt.Errorf("workspace: create materialization slot: %w", err)
	}
	return slot, filepath.Join(relativeParent, id), nil
}

func pathFromFields(fields []string) value.Path {
	var path value.Path
	for _, field := range fields {
		path = path.Field(field)
	}
	return path
}

func canonicalPath(path value.Path) []byte {
	segments := path.Segments()
	encoded := make([]byte, 0, 8+len(segments)*10)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(segments)))
	for _, segment := range segments {
		encoded = append(encoded, byte(segment.Kind()))
		switch segment.Kind() {
		case value.FieldSegment, value.MapKeySegment:
			name, _ := segment.Name()
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(name)))
			encoded = append(encoded, name...)
		case value.ListIndexSegment:
			index, _ := segment.Index()
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(int64(index)))
		}
	}
	return encoded
}

func pathSlotIdentity(domain string, canonical []byte) string {
	hasher := sha256.New()
	hasher.Write([]byte(domain))
	hasher.Write([]byte{0})
	hasher.Write(canonical)
	return hex.EncodeToString(hasher.Sum(nil))
}

// Input copies are made read-only as a best-effort presentation property. A
// process with the same user authority can change these modes, so this is not
// an isolation boundary; the committed repository content remains immutable.
func bestEffortReadOnly(root string) {
	paths := make([]string, 0)
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, path)
		}
		return nil
	})
	for index := len(paths) - 1; index >= 0; index-- {
		info, err := os.Lstat(paths[index])
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		_ = os.Chmod(paths[index], info.Mode().Perm()&^0o222)
	}
}

func removeRuntimeRoot(root string) error {
	if root == "" {
		return nil
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
	return os.RemoveAll(root)
}

type schemaSegmentKind uint8

const (
	schemaField schemaSegmentKind = iota + 1
	schemaListMember
	schemaMapMember
)

type schemaSegment struct {
	kind schemaSegmentKind
	name string
}

type outputTask struct {
	typ     value.Type
	schema  []schemaSegment
	static  value.Path
	dynamic bool
}

type outputPlan struct {
	kind             value.Kind
	dynamic          bool
	location         string
	relativeLocation string
}

func prepareOutputs(ctx context.Context, contract value.Contract, published []string, outputRoot string) ([]Output, map[string]outputPlan, map[string]Target, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("workspace: prepare outputs: %w", err)
	}
	ports := contract.Ports()
	tasks := make([]outputTask, 0, len(ports))
	for index := len(ports) - 1; index >= 0; index-- {
		port := ports[index]
		tasks = append(tasks, outputTask{
			typ: port.Type(), schema: []schemaSegment{{kind: schemaField, name: port.Name()}},
			static: value.Path{}.Field(port.Name()),
		})
	}
	publishedCanonical := canonicalPath(pathFromFields(published))
	records := make([]Output, 0)
	plans := make(map[string]outputPlan)
	targets := make(map[string]Target)
	identities := make(map[string]string)
	for len(tasks) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, fmt.Errorf("workspace: prepare outputs: %w", err)
		}
		last := len(tasks) - 1
		task := tasks[last]
		tasks = tasks[:last]
		switch task.typ.Kind() {
		case value.ObjectKind:
			fields := task.typ.Fields()
			for index := len(fields) - 1; index >= 0; index-- {
				field := fields[index]
				child := outputTask{
					typ: field.Type(), schema: appendSchema(task.schema, schemaSegment{kind: schemaField, name: field.Name()}),
					static: cloneValuePath(task.static), dynamic: task.dynamic,
				}
				if !child.dynamic {
					child.static = child.static.Field(field.Name())
				}
				tasks = append(tasks, child)
			}
		case value.ListKind, value.MapKind:
			element, ok := task.typ.Element()
			if !ok {
				return nil, nil, nil, fmt.Errorf("workspace: invalid compiled dynamic output type")
			}
			kind := schemaListMember
			if task.typ.Kind() == value.MapKind {
				kind = schemaMapMember
			}
			task.typ = element
			task.schema = appendSchema(task.schema, schemaSegment{kind: kind})
			task.dynamic = true
			tasks = append(tasks, task)
		case value.FileKind, value.TreeKind:
			if !task.dynamic && len(published) != 0 && string(canonicalPath(task.static)) == string(publishedCanonical) {
				continue
			}
			schemaCanonical := canonicalSchema(task.schema)
			schemaKey := string(schemaCanonical)
			if _, exists := plans[schemaKey]; exists {
				return nil, nil, nil, fmt.Errorf("workspace: duplicate output schema position")
			}
			var id, identity string
			if task.dynamic {
				id = pathSlotIdentity("dawn.workspace.output.namespace/1", schemaCanonical)
				identity = schemaKey
			} else {
				exact := canonicalPath(task.static)
				id = pathSlotIdentity("dawn.workspace.output.static/1", exact)
				identity = string(exact)
			}
			if prior, exists := identities[id]; exists {
				if prior == identity {
					return nil, nil, nil, fmt.Errorf("workspace: duplicate output slot identity")
				}
				return nil, nil, nil, fmt.Errorf("workspace: internal output slot identity collision")
			}
			identities[id] = identity
			slot := filepath.Join(outputRoot, id)
			if err := os.Mkdir(slot, 0o700); err != nil {
				return nil, nil, nil, fmt.Errorf("workspace: create output slot: %w", err)
			}
			relativeSlot := filepath.ToSlash(filepath.Join("outputs", id))
			plan := outputPlan{kind: task.typ.Kind(), dynamic: task.dynamic, location: slot, relativeLocation: relativeSlot}
			record := Output{
				valuePath: cloneValuePath(task.static), schemaPath: renderSchema(task.schema), kind: task.typ.Kind(),
				dynamic: task.dynamic, location: slot, relativeLocation: relativeSlot,
			}
			if !task.dynamic {
				targetPath := filepath.Join(slot, "value")
				if task.typ.Kind() == value.TreeKind {
					if err := os.Mkdir(targetPath, 0o700); err != nil {
						return nil, nil, nil, fmt.Errorf("workspace: create static tree output target: %w", err)
					}
				}
				relativeTarget := filepath.ToSlash(filepath.Join(relativeSlot, "value"))
				record.location = targetPath
				record.relativeLocation = relativeTarget
				targets[string(canonicalPath(task.static))] = Target{
					valuePath: cloneValuePath(task.static), kind: task.typ.Kind(),
					location: targetPath, relativeLocation: relativeTarget,
				}
			}
			plans[schemaKey] = plan
			records = append(records, record)
		case value.StringKind, value.IntegerKind, value.NumberKind, value.BooleanKind, value.NullKind, value.EnumKind, value.AnyKind:
		default:
			return nil, nil, nil, fmt.Errorf("workspace: invalid compiled output type")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("workspace: prepare outputs: %w", err)
	}
	return records, plans, targets, nil
}

func resolveOutputSchema(contract value.Contract, path value.Path) (value.Type, []schemaSegment, error) {
	segments := path.Segments()
	if len(segments) == 0 || segments[0].Kind() != value.FieldSegment {
		return value.Type{}, nil, fmt.Errorf("workspace: output path %s must begin with a declared field", path.String())
	}
	fieldName, _ := segments[0].Name()
	field, ok := findOutputField(contract.Ports(), fieldName)
	if !ok {
		return value.Type{}, nil, fmt.Errorf("workspace: output path %s begins with an undeclared field", path.String())
	}
	typ := field.Type()
	schema := []schemaSegment{{kind: schemaField, name: fieldName}}
	for _, segment := range segments[1:] {
		switch typ.Kind() {
		case value.ObjectKind:
			if segment.Kind() != value.FieldSegment {
				return value.Type{}, nil, fmt.Errorf("workspace: output path %s uses the wrong structured segment", path.String())
			}
			name, _ := segment.Name()
			field, ok := findOutputField(typ.Fields(), name)
			if !ok {
				return value.Type{}, nil, fmt.Errorf("workspace: output path %s selects an undeclared field", path.String())
			}
			typ = field.Type()
			schema = append(schema, schemaSegment{kind: schemaField, name: name})
		case value.ListKind:
			if segment.Kind() != value.ListIndexSegment {
				return value.Type{}, nil, fmt.Errorf("workspace: output path %s requires a list index", path.String())
			}
			index, _ := segment.Index()
			if index < 0 {
				return value.Type{}, nil, fmt.Errorf("workspace: output path %s has a negative list index", path.String())
			}
			element, ok := typ.Element()
			if !ok {
				return value.Type{}, nil, fmt.Errorf("workspace: invalid compiled list output type")
			}
			typ = element
			schema = append(schema, schemaSegment{kind: schemaListMember})
		case value.MapKind:
			if segment.Kind() != value.MapKeySegment {
				return value.Type{}, nil, fmt.Errorf("workspace: output path %s requires a map key", path.String())
			}
			element, ok := typ.Element()
			if !ok {
				return value.Type{}, nil, fmt.Errorf("workspace: invalid compiled map output type")
			}
			typ = element
			schema = append(schema, schemaSegment{kind: schemaMapMember})
		default:
			return value.Type{}, nil, fmt.Errorf("workspace: output path %s continues beyond a leaf", path.String())
		}
	}
	return typ, schema, nil
}

func findOutputField(fields []value.Field, name string) (value.Field, bool) {
	for _, field := range fields {
		if field.Name() == name {
			return field, true
		}
	}
	return value.Field{}, false
}

func appendSchema(schema []schemaSegment, segment schemaSegment) []schemaSegment {
	cloned := make([]schemaSegment, len(schema), len(schema)+1)
	copy(cloned, schema)
	cloned = append(cloned, segment)
	return cloned
}

func canonicalSchema(schema []schemaSegment) []byte {
	encoded := make([]byte, 0, len(schema)*10+8)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(schema)))
	for _, segment := range schema {
		encoded = append(encoded, byte(segment.kind))
		if segment.kind == schemaField {
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(segment.name)))
			encoded = append(encoded, segment.name...)
		}
	}
	return encoded
}

func renderSchema(schema []schemaSegment) string {
	result := "$"
	for _, segment := range schema {
		switch segment.kind {
		case schemaField:
			result += ".field(" + strconv.Quote(segment.name) + ")"
		case schemaListMember:
			result += "[*]"
		case schemaMapMember:
			result += ".key(*)"
		}
	}
	return result
}
