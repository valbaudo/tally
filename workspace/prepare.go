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
	workspace             string
	workspaceIdentity     fs.FileInfo
	outputRoot            string
	outputRootIdentity    fs.FileInfo
	repository            *content.Repository
	publishedWorkspace    []string
	manifest              Manifest
	outputs               value.Contract
	plans                 map[string]outputPlan
	targets               map[string]Target
	members               map[string]*memberAllocation
	namespaces            []outputNamespaceCapability
	rootCapability        *pinnedInvocationRoot
	mu                    sync.Mutex
	closed                bool
	namespacesClosed      bool
	rootRemoved           bool
	rootContentsRemoved   bool
	rootCleanupErr        error
	rootCapabilityClosed  bool
	removeRootEntry       func() error
	beforeRootEntryRemove func()
}

// Target is one writable declared file or tree output location.
type Target struct {
	valuePath        value.Path
	kind             value.Kind
	location         string
	relativeLocation string
	slotIdentity     fs.FileInfo
	targetIdentity   fs.FileInfo
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
	rootCapability, err := openPinnedInvocationRoot(root)
	if err != nil {
		if cleanupErr := os.Remove(root); cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("workspace: remove empty unpinned runtime root: %w", cleanupErr))
		}
		return nil, err
	}
	prepared := false
	defer func() {
		if !prepared {
			if cleanupErr := removePinnedInvocationRoot(rootCapability); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("workspace: remove failed runtime root: %w", cleanupErr))
			}
			if closeErr := rootCapability.close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("workspace: close failed runtime root capability: %w", closeErr))
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
	workspaceIdentity, err := os.Lstat(working)
	if err != nil || !workspaceIdentity.IsDir() {
		if err != nil {
			return nil, fmt.Errorf("workspace: inspect working directory: %w", err)
		}
		return nil, fmt.Errorf("workspace: working directory is not a directory")
	}
	manifestOutputs, plans, targets, namespaces, err := prepareOutputs(ctx, leaf.Outputs(), leaf.PublishWorkspace(), outputRoot)
	if err != nil {
		return nil, err
	}
	outputRootIdentity, err := os.Lstat(outputRoot)
	if err != nil || !outputRootIdentity.IsDir() {
		if err != nil {
			return nil, fmt.Errorf("workspace: inspect output root: %w", err)
		}
		return nil, fmt.Errorf("workspace: output root is not a directory")
	}
	environment := &Environment{
		workspace:          working,
		workspaceIdentity:  workspaceIdentity,
		outputRoot:         outputRoot,
		outputRootIdentity: outputRootIdentity,
		repository:         repository,
		publishedWorkspace: append([]string(nil), leaf.PublishWorkspace()...),
		manifest:           Manifest{inputs: manifestInputs, outputs: manifestOutputs},
		outputs:            leaf.Outputs(),
		plans:              plans,
		targets:            targets,
		members:            make(map[string]*memberAllocation),
		namespaces:         namespaces,
		rootCapability:     rootCapability,
	}
	environment.removeRootEntry = rootCapability.removeEntry
	prepared = true
	return environment, nil
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
	physicalIdentity := plan.identity + "\x00" + memberID
	if prior, exists := e.members[physicalIdentity]; exists {
		if prior.concrete != key {
			return Target{}, fmt.Errorf("workspace: internal dynamic output slot identity collision")
		}
		if prior.failure == nil || !prior.cleanupPending {
			return Target{}, fmt.Errorf("workspace: duplicate dynamic output slot identity")
		}
		if cleanupErr := plan.namespace.removeMember(prior.memberID); cleanupErr != nil {
			return Target{}, errors.Join(prior.failure, fmt.Errorf("workspace: retry dynamic output member cleanup: %w", cleanupErr))
		}
		delete(e.members, physicalIdentity)
	}
	allocation := &memberAllocation{concrete: key, memberID: memberID}
	e.members[physicalIdentity] = allocation
	memberRoot, created, err := plan.namespace.createMemberRoot(memberID)
	if err != nil {
		return Target{}, e.failDynamicMember(plan, physicalIdentity, allocation, created, fmt.Errorf("workspace: create dynamic output member: %w", err))
	}
	if !created || memberRoot == nil {
		return Target{}, e.failDynamicMember(plan, physicalIdentity, allocation, created, fmt.Errorf("workspace: dynamic output member creation returned no directory"))
	}
	createdInfo, err := memberRoot.lstat(".")
	if err != nil || !createdInfo.IsDir() {
		if err == nil {
			err = fmt.Errorf("dynamic output member is not a directory")
		}
		cause := errors.Join(fmt.Errorf("workspace: inspect dynamic output member: %w", err), memberRoot.close())
		return Target{}, e.failDynamicMember(plan, physicalIdentity, allocation, true, cause)
	}
	targetPath := filepath.Join(plan.location, memberID, "value")
	var targetInfo fs.FileInfo
	if plan.kind == value.TreeKind {
		if err := memberRoot.mkdir("value", 0o700); err != nil {
			cause := errors.Join(fmt.Errorf("workspace: create dynamic tree output target: %w", err), memberRoot.close())
			return Target{}, e.failDynamicMember(plan, physicalIdentity, allocation, true, cause)
		}
		targetInfo, err = memberRoot.lstat("value")
		if err != nil || !targetInfo.IsDir() {
			if err == nil {
				err = fmt.Errorf("dynamic tree output target is not a directory")
			}
			cause := errors.Join(fmt.Errorf("workspace: inspect dynamic tree output target: %w", err), memberRoot.close())
			return Target{}, e.failDynamicMember(plan, physicalIdentity, allocation, true, cause)
		}
	}
	if err := memberRoot.close(); err != nil {
		return Target{}, e.failDynamicMember(plan, physicalIdentity, allocation, true, fmt.Errorf("workspace: close dynamic output member: %w", err))
	}
	if err := plan.namespace.verify(); err != nil {
		return Target{}, e.failDynamicMember(plan, physicalIdentity, allocation, true, fmt.Errorf("workspace: verify dynamic output namespace: %w", err))
	}
	target := Target{
		valuePath: cloneValuePath(path), kind: plan.kind, location: targetPath,
		relativeLocation: filepath.ToSlash(filepath.Join(plan.relativeLocation, memberID, "value")),
		slotIdentity:     createdInfo,
		targetIdentity:   targetInfo,
	}
	e.targets[key] = target
	return target, nil
}

type memberAllocation struct {
	concrete       string
	memberID       string
	failure        error
	cleanupPending bool
}

func (e *Environment) failDynamicMember(plan outputPlan, physicalIdentity string, allocation *memberAllocation, created bool, cause error) error {
	allocation.failure = cause
	if !created {
		delete(e.members, physicalIdentity)
		return cause
	}
	if cleanupErr := plan.namespace.removeMember(allocation.memberID); cleanupErr != nil {
		allocation.cleanupPending = true
		return errors.Join(cause, fmt.Errorf("workspace: clean failed dynamic output member: %w", cleanupErr))
	}
	delete(e.members, physicalIdentity)
	return cause
}

// Close removes all private state. It is safe to call repeatedly.
func (e *Environment) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.namespacesClosed && e.rootRemoved && e.rootCapabilityClosed {
		return nil
	}
	e.closed = true
	var err error
	if !e.namespacesClosed {
		closeFailed := false
		for _, namespace := range e.namespaces {
			if closeErr := namespace.close(); closeErr != nil {
				closeFailed = true
				err = errors.Join(err, fmt.Errorf("workspace: close dynamic output namespace: %w", closeErr))
			}
		}
		e.namespacesClosed = !closeFailed
	}
	if !e.rootRemoved && e.rootCleanupErr == nil {
		cleanupErr := e.cleanupRuntimeRoot()
		if e.rootCleanupErr == nil {
			err = errors.Join(err, cleanupErr)
		}
	}
	if e.rootCleanupErr != nil {
		err = errors.Join(err, e.rootCleanupErr)
	}
	if (e.rootRemoved || e.rootCleanupErr != nil) && !e.rootCapabilityClosed {
		if e.rootCapability == nil {
			e.rootCapabilityClosed = true
		} else if closeErr := e.rootCapability.close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("workspace: close runtime root capability: %w", closeErr))
		} else {
			e.rootCapabilityClosed = true
		}
	}
	return err
}

func (e *Environment) cleanupRuntimeRoot() error {
	if e.rootCapability == nil {
		e.rootCleanupErr = fmt.Errorf("workspace: runtime root cleanup integrity: missing retained identity")
		return e.rootCleanupErr
	}
	present, inspectErr := e.rootCapability.inspectEntry()
	if inspectErr != nil {
		return e.runtimeRootInspectionError("inspect runtime root for cleanup", inspectErr)
	}
	if !present {
		e.rootRemoved = true
		return nil
	}
	if !e.rootContentsRemoved {
		if e.rootCapability.rootClosed {
			if reopenErr := e.rootCapability.reopenRoot(); reopenErr != nil {
				return e.runtimeRootInspectionError("reopen runtime root for cleanup", reopenErr)
			}
		}
		if contentsErr := e.rootCapability.removeContents(); contentsErr != nil {
			return e.runtimeRootInspectionError("remove runtime root contents", contentsErr)
		}
		e.rootContentsRemoved = true
	}
	if !e.rootCapability.rootClosed {
		if closeErr := e.rootCapability.closeRoot(); closeErr != nil {
			return fmt.Errorf("workspace: close runtime root before entry removal: %w", closeErr)
		}
	}
	present, inspectErr = e.rootCapability.inspectEntry()
	if inspectErr != nil {
		return e.runtimeRootInspectionError("verify runtime root identity before entry removal", inspectErr)
	}
	if !present {
		e.rootRemoved = true
		return nil
	}
	if beforeRemove := e.beforeRootEntryRemove; beforeRemove != nil {
		e.beforeRootEntryRemove = nil
		beforeRemove()
	}
	remove := e.removeRootEntry
	if remove == nil {
		remove = e.rootCapability.removeEntry
	}
	removeErr := remove()
	present, inspectErr = e.rootCapability.inspectEntry()
	if inspectErr != nil {
		return e.runtimeRootInspectionError("verify runtime root entry removal", inspectErr)
	}
	if !present {
		e.rootRemoved = true
		return nil
	}
	if removeErr != nil {
		e.rootContentsRemoved = false
		return fmt.Errorf("workspace: remove empty runtime root entry: %w", removeErr)
	}
	e.rootContentsRemoved = false
	return fmt.Errorf("workspace: remove empty runtime root entry: cleanup completed without removing the retained directory")
}

func (e *Environment) runtimeRootInspectionError(action string, err error) error {
	if errors.Is(err, errInvocationRootIdentityChanged) {
		e.rootCleanupErr = fmt.Errorf("workspace: runtime root cleanup integrity: %w", err)
		return e.rootCleanupErr
	}
	return fmt.Errorf("workspace: %s: %w", action, err)
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
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if !info.IsDir() {
		_ = os.Chmod(root, info.Mode().Perm()&^0o222)
		return
	}
	capability, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	type frame struct {
		root       *os.Root
		entries    []os.DirEntry
		position   int
		enumerated bool
	}
	stack := []frame{{root: capability}}
	for len(stack) != 0 {
		current := &stack[len(stack)-1]
		if !current.enumerated {
			current.entries, _ = fs.ReadDir(current.root.FS(), ".")
			current.enumerated = true
			continue
		}
		if current.position == len(current.entries) {
			if currentInfo, statErr := current.root.Lstat("."); statErr == nil {
				_ = current.root.Chmod(".", currentInfo.Mode().Perm()&^0o222)
			}
			_ = current.root.Close()
			stack = stack[:len(stack)-1]
			continue
		}
		entry := current.entries[current.position]
		current.position++
		childInfo, statErr := current.root.Lstat(entry.Name())
		if statErr != nil || childInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !childInfo.IsDir() {
			_ = current.root.Chmod(entry.Name(), childInfo.Mode().Perm()&^0o222)
			continue
		}
		child, openErr := current.root.OpenRoot(entry.Name())
		if openErr != nil {
			_ = current.root.Chmod(entry.Name(), childInfo.Mode().Perm()&^0o222)
			continue
		}
		stack = append(stack, frame{root: child})
	}
}

var errInvocationRootIdentityChanged = errors.New("runtime root path identity changed")

// pinnedInvocationRoot retains the directory allocated by Prepare and its
// parent directory capability. Cleanup resolves the final path component
// relative to that retained parent and never follows a replacement symlink.
type pinnedInvocationRoot struct {
	parent       *os.Root
	root         *os.Root
	name         string
	identity     fs.FileInfo
	parentClosed bool
	rootClosed   bool
}

func openPinnedInvocationRoot(location string) (*pinnedInvocationRoot, error) {
	clean := filepath.Clean(location)
	parent, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return nil, fmt.Errorf("workspace: pin runtime root parent: %w", err)
	}
	name := filepath.Base(clean)
	identity, err := parent.Lstat(name)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("workspace: inspect allocated runtime root: %w", err), parent.Close())
	}
	if !identity.IsDir() {
		return nil, errors.Join(fmt.Errorf("workspace: allocated runtime root is not a directory"), parent.Close())
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("workspace: pin allocated runtime root: %w", err), parent.Close())
	}
	opened, err := root.Lstat(".")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("workspace: inspect pinned runtime root: %w", err), root.Close(), parent.Close())
	}
	if !opened.IsDir() || !os.SameFile(identity, opened) {
		return nil, errors.Join(fmt.Errorf("workspace: allocated runtime root identity changed while opening"), root.Close(), parent.Close())
	}
	return &pinnedInvocationRoot{parent: parent, root: root, name: name, identity: identity}, nil
}

func (r *pinnedInvocationRoot) inspectEntry() (bool, error) {
	if r == nil || r.parent == nil || r.parentClosed {
		return false, fmt.Errorf("workspace: runtime root parent capability is closed")
	}
	info, err := r.parent.Lstat(r.name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || !os.SameFile(r.identity, info) {
		return false, errInvocationRootIdentityChanged
	}
	return true, nil
}

func (r *pinnedInvocationRoot) inspectRoot() (bool, error) {
	present, err := r.inspectEntry()
	if err != nil || !present {
		return present, err
	}
	if r.root == nil || r.rootClosed {
		return false, fmt.Errorf("workspace: runtime root capability is closed")
	}
	opened, err := r.root.Lstat(".")
	if err != nil {
		return false, err
	}
	if !opened.IsDir() || !os.SameFile(r.identity, opened) {
		return false, fmt.Errorf("workspace: pinned runtime root identity changed: %w", errInvocationRootIdentityChanged)
	}
	return true, nil
}

func (r *pinnedInvocationRoot) reopenRoot() error {
	present, err := r.inspectEntry()
	if err != nil || !present {
		return err
	}
	root, err := r.parent.OpenRoot(r.name)
	if err != nil {
		return err
	}
	opened, err := root.Lstat(".")
	if err != nil {
		return errors.Join(err, root.Close())
	}
	if !opened.IsDir() || !os.SameFile(r.identity, opened) {
		return errors.Join(errInvocationRootIdentityChanged, root.Close())
	}
	r.root = root
	r.rootClosed = false
	return nil
}

func (r *pinnedInvocationRoot) removeContents() error {
	present, err := r.inspectRoot()
	if err != nil || !present {
		return err
	}
	_ = r.root.Chmod(".", 0o700)
	entries, err := fs.ReadDir(r.root.FS(), ".")
	if err != nil {
		return err
	}
	var removeErr error
	root := osRootedDirectory{root: r.root}
	for _, entry := range entries {
		if err := removeRootedEntry(root, entry.Name()); err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove runtime root child: %w", err))
		}
	}
	if removeErr != nil {
		return removeErr
	}
	remaining, err := fs.ReadDir(r.root.FS(), ".")
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("runtime root contents changed during cleanup")
	}
	return nil
}

func (r *pinnedInvocationRoot) removeEntry() error {
	if r == nil || r.parent == nil || r.parentClosed {
		return fmt.Errorf("workspace: runtime root parent capability is closed")
	}
	if !r.rootClosed {
		return fmt.Errorf("workspace: runtime root capability must be closed before entry removal")
	}
	return r.parent.Remove(r.name)
}

func (r *pinnedInvocationRoot) closeRoot() error {
	if r == nil || r.root == nil || r.rootClosed {
		return nil
	}
	if err := r.root.Close(); err != nil {
		return err
	}
	r.rootClosed = true
	return nil
}

func (r *pinnedInvocationRoot) close() error {
	if r == nil {
		return nil
	}
	var err error
	if closeErr := r.closeRoot(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if r.parent != nil && !r.parentClosed {
		if closeErr := r.parent.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		} else {
			r.parentClosed = true
		}
	}
	return err
}

func removePinnedInvocationRoot(root *pinnedInvocationRoot) error {
	present, err := root.inspectRoot()
	if err != nil || !present {
		return err
	}
	if err := root.removeContents(); err != nil {
		return err
	}
	if err := root.closeRoot(); err != nil {
		return err
	}
	present, err = root.inspectEntry()
	if err != nil || !present {
		return err
	}
	return root.removeEntry()
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

type schemaCursor struct {
	parent  *schemaCursor
	segment schemaSegment
	length  int
}

type outputTask struct {
	typ     value.Type
	schema  *schemaCursor
	static  value.Path
	dynamic bool
}

type rootedDirectory interface {
	lstat(string) (fs.FileInfo, error)
	readDir() ([]fs.DirEntry, error)
	chmod(string, fs.FileMode) error
	mkdir(string, fs.FileMode) error
	openRoot(string) (rootedDirectory, error)
	remove(string) error
	close() error
}

type osRootedDirectory struct{ root *os.Root }

func (r osRootedDirectory) lstat(name string) (fs.FileInfo, error) {
	return r.root.Lstat(name)
}

func (r osRootedDirectory) readDir() ([]fs.DirEntry, error) {
	return fs.ReadDir(r.root.FS(), ".")
}

func (r osRootedDirectory) chmod(name string, mode fs.FileMode) error {
	return r.root.Chmod(name, mode)
}

func (r osRootedDirectory) mkdir(name string, mode fs.FileMode) error {
	return r.root.Mkdir(name, mode)
}

func (r osRootedDirectory) openRoot(name string) (rootedDirectory, error) {
	root, err := r.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return osRootedDirectory{root: root}, nil
}

func (r osRootedDirectory) remove(name string) error { return r.root.Remove(name) }
func (r osRootedDirectory) close() error             { return r.root.Close() }

type rootedRemovalPath struct {
	parent *rootedRemovalPath
	name   string
	depth  int
}

type rootedRemovalTask struct {
	path   *rootedRemovalPath
	remove bool
}

func removeRootedEntry(anchor rootedDirectory, name string) error {
	info, err := anchor.lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return anchor.remove(name)
	}
	_ = anchor.chmod(name, 0o700)
	rootPath := &rootedRemovalPath{name: name, depth: 1}
	stack := []rootedRemovalTask{{path: rootPath}}
	for len(stack) != 0 {
		task := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if task.remove {
			if err := removeRootedRemovalPath(anchor, task.path); err != nil {
				return err
			}
			continue
		}

		root, _, err := openRootedRemovalPath(anchor, task.path)
		if err != nil {
			return err
		}
		_ = root.chmod(".", 0o700)
		entries, readErr := root.readDir()
		if readErr != nil {
			return errors.Join(readErr, root.close())
		}
		children := make([]*rootedRemovalPath, 0, len(entries))
		for _, entry := range entries {
			childInfo, statErr := root.lstat(entry.Name())
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil {
				return errors.Join(statErr, root.close())
			}
			if !childInfo.IsDir() || childInfo.Mode()&os.ModeSymlink != 0 {
				if removeErr := root.remove(entry.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return errors.Join(removeErr, root.close())
				}
				continue
			}
			_ = root.chmod(entry.Name(), 0o700)
			children = append(children, &rootedRemovalPath{parent: task.path, name: entry.Name(), depth: task.path.depth + 1})
		}
		if closeErr := root.close(); closeErr != nil {
			return closeErr
		}
		stack = append(stack, rootedRemovalTask{path: task.path, remove: true})
		for index := len(children) - 1; index >= 0; index-- {
			stack = append(stack, rootedRemovalTask{path: children[index]})
		}
	}
	return nil
}

func openRootedRemovalPath(anchor rootedDirectory, path *rootedRemovalPath) (root rootedDirectory, owned bool, err error) {
	if path == nil {
		return anchor, false, nil
	}
	segments := make([]string, path.depth)
	for current := path; current != nil; current = current.parent {
		segments[current.depth-1] = current.name
	}
	current := anchor
	currentOwned := false
	for _, segment := range segments {
		next, openErr := current.openRoot(segment)
		if openErr != nil {
			if next != nil {
				openErr = errors.Join(openErr, next.close())
			}
			if currentOwned {
				openErr = errors.Join(openErr, current.close())
			}
			return nil, false, openErr
		}
		if next == nil {
			openErr = fmt.Errorf("workspace: open cleanup directory returned no capability")
			if currentOwned {
				openErr = errors.Join(openErr, current.close())
			}
			return nil, false, openErr
		}
		if currentOwned {
			if closeErr := current.close(); closeErr != nil {
				return nil, false, errors.Join(closeErr, next.close())
			}
		}
		current = next
		currentOwned = true
	}
	return current, currentOwned, nil
}

func removeRootedRemovalPath(anchor rootedDirectory, path *rootedRemovalPath) error {
	parent, owned, err := openRootedRemovalPath(anchor, path.parent)
	if err != nil {
		return err
	}
	removeErr := parent.remove(path.name)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if owned {
		removeErr = errors.Join(removeErr, parent.close())
	}
	return removeErr
}

type outputNamespaceCapability interface {
	createMemberRoot(string) (rootedDirectory, bool, error)
	removeMember(string) error
	verify() error
	close() error
}

// pinnedOutputNamespace confines Dawn's own dynamic-slot creation to the
// directory opened during Prepare. It is not a sandbox for the invoked process.
type pinnedOutputNamespace struct {
	location string
	root     rootedDirectory
	identity fs.FileInfo
	closed   bool
}

func openPinnedOutputNamespace(location string) (outputNamespaceCapability, error) {
	identity, err := os.Lstat(location)
	if err != nil {
		return nil, fmt.Errorf("workspace: inspect dynamic output namespace: %w", err)
	}
	if !identity.IsDir() {
		return nil, fmt.Errorf("workspace: dynamic output namespace is not a directory")
	}
	root, err := os.OpenRoot(location)
	if err != nil {
		return nil, fmt.Errorf("workspace: pin dynamic output namespace: %w", err)
	}
	namespace := &pinnedOutputNamespace{location: location, root: osRootedDirectory{root: root}, identity: identity}
	if err := namespace.verify(); err != nil {
		return nil, errors.Join(err, namespace.close())
	}
	return namespace, nil
}

func (n *pinnedOutputNamespace) verify() error {
	if n == nil || n.root == nil || n.closed {
		return fmt.Errorf("workspace: dynamic output namespace capability is closed")
	}
	pathInfo, err := os.Lstat(n.location)
	if err != nil {
		return fmt.Errorf("workspace: inspect dynamic output namespace path: %w", err)
	}
	if !pathInfo.IsDir() || !os.SameFile(n.identity, pathInfo) {
		return fmt.Errorf("workspace: dynamic output namespace path identity changed")
	}
	rootInfo, err := n.root.lstat(".")
	if err != nil {
		return fmt.Errorf("workspace: inspect pinned dynamic output namespace: %w", err)
	}
	if !rootInfo.IsDir() || !os.SameFile(n.identity, rootInfo) {
		return fmt.Errorf("workspace: pinned dynamic output namespace identity changed")
	}
	return nil
}

func (n *pinnedOutputNamespace) createMemberRoot(name string) (rootedDirectory, bool, error) {
	if err := n.verify(); err != nil {
		return nil, false, err
	}
	if err := n.root.mkdir(name, 0o700); err != nil {
		return nil, false, err
	}
	created, err := n.root.lstat(name)
	if err != nil {
		return nil, true, err
	}
	if !created.IsDir() {
		return nil, true, fmt.Errorf("workspace: dynamic output member is not a directory")
	}
	member, err := n.root.openRoot(name)
	if err != nil {
		return nil, true, err
	}
	opened, err := member.lstat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(created, opened) {
		closeErr := member.close()
		if err != nil {
			return nil, true, errors.Join(err, closeErr)
		}
		return nil, true, errors.Join(fmt.Errorf("workspace: dynamic output member identity changed while opening"), closeErr)
	}
	pathInfo, err := os.Lstat(filepath.Join(n.location, name))
	if err != nil || !pathInfo.IsDir() || !os.SameFile(created, pathInfo) {
		closeErr := member.close()
		if err != nil {
			return nil, true, errors.Join(err, closeErr)
		}
		return nil, true, errors.Join(fmt.Errorf("workspace: dynamic output member path identity changed"), closeErr)
	}
	if err := n.verify(); err != nil {
		return nil, true, errors.Join(err, member.close())
	}
	return member, true, nil
}

func (n *pinnedOutputNamespace) removeMember(name string) error {
	if n == nil || n.root == nil || n.closed {
		return fmt.Errorf("workspace: dynamic output namespace capability is closed")
	}
	return removeRootedEntry(n.root, name)
}

func (n *pinnedOutputNamespace) close() error {
	if n == nil || n.closed {
		return nil
	}
	err := n.root.close()
	if err == nil {
		n.closed = true
	}
	return err
}

type outputPlan struct {
	kind              value.Kind
	dynamic           bool
	identity          string
	location          string
	relativeLocation  string
	directoryIdentity fs.FileInfo
	namespace         outputNamespaceCapability
}

func prepareOutputs(ctx context.Context, contract value.Contract, published []string, outputRoot string) (records []Output, plans map[string]outputPlan, targets map[string]Target, namespaces []outputNamespaceCapability, err error) {
	defer func() {
		if err == nil {
			return
		}
		for _, namespace := range namespaces {
			if closeErr := namespace.close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("workspace: close failed output namespace: %w", closeErr))
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("workspace: prepare outputs: %w", err)
	}
	ports := contract.Ports()
	tasks := make([]outputTask, 0, len(ports))
	for index := len(ports) - 1; index >= 0; index-- {
		port := ports[index]
		tasks = append(tasks, outputTask{
			typ: port.Type(), schema: appendSchema(nil, schemaSegment{kind: schemaField, name: port.Name()}),
			static: value.Path{}.Field(port.Name()),
		})
	}
	publishedCanonical := canonicalPath(pathFromFields(published))
	records = make([]Output, 0)
	plans = make(map[string]outputPlan)
	targets = make(map[string]Target)
	identities := make(map[string]string)
	for len(tasks) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("workspace: prepare outputs: %w", err)
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
				return nil, nil, nil, nil, fmt.Errorf("workspace: invalid compiled dynamic output type")
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
			schema := materializeSchema(task.schema)
			schemaCanonical := canonicalSchema(schema)
			schemaKey := string(schemaCanonical)
			if _, exists := plans[schemaKey]; exists {
				return nil, nil, nil, nil, fmt.Errorf("workspace: duplicate output schema position")
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
					return nil, nil, nil, nil, fmt.Errorf("workspace: duplicate output slot identity")
				}
				return nil, nil, nil, nil, fmt.Errorf("workspace: internal output slot identity collision")
			}
			identities[id] = identity
			slot := filepath.Join(outputRoot, id)
			if err := os.Mkdir(slot, 0o700); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("workspace: create output slot: %w", err)
			}
			slotIdentity, err := os.Lstat(slot)
			if err != nil || !slotIdentity.IsDir() {
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("workspace: inspect output slot: %w", err)
				}
				return nil, nil, nil, nil, fmt.Errorf("workspace: output slot is not a directory")
			}
			relativeSlot := filepath.ToSlash(filepath.Join("outputs", id))
			plan := outputPlan{
				kind: task.typ.Kind(), dynamic: task.dynamic, identity: schemaKey,
				location: slot, relativeLocation: relativeSlot, directoryIdentity: slotIdentity,
			}
			if task.dynamic {
				namespace, openErr := openPinnedOutputNamespace(slot)
				if openErr != nil {
					return nil, nil, nil, nil, openErr
				}
				plan.namespace = namespace
				namespaces = append(namespaces, namespace)
			}
			record := Output{
				valuePath: cloneValuePath(task.static), schemaPath: renderSchema(schema), kind: task.typ.Kind(),
				dynamic: task.dynamic, location: slot, relativeLocation: relativeSlot,
			}
			if !task.dynamic {
				targetPath := filepath.Join(slot, "value")
				var targetIdentity fs.FileInfo
				if task.typ.Kind() == value.TreeKind {
					if err := os.Mkdir(targetPath, 0o700); err != nil {
						return nil, nil, nil, nil, fmt.Errorf("workspace: create static tree output target: %w", err)
					}
					targetIdentity, err = os.Lstat(targetPath)
					if err != nil || !targetIdentity.IsDir() {
						if err != nil {
							return nil, nil, nil, nil, fmt.Errorf("workspace: inspect static tree output target: %w", err)
						}
						return nil, nil, nil, nil, fmt.Errorf("workspace: static tree output target is not a directory")
					}
				}
				relativeTarget := filepath.ToSlash(filepath.Join(relativeSlot, "value"))
				record.location = targetPath
				record.relativeLocation = relativeTarget
				targets[string(canonicalPath(task.static))] = Target{
					valuePath: cloneValuePath(task.static), kind: task.typ.Kind(),
					location: targetPath, relativeLocation: relativeTarget,
					slotIdentity: slotIdentity, targetIdentity: targetIdentity,
				}
			}
			plans[schemaKey] = plan
			records = append(records, record)
		case value.StringKind, value.IntegerKind, value.NumberKind, value.BooleanKind, value.NullKind, value.EnumKind, value.AnyKind:
		default:
			return nil, nil, nil, nil, fmt.Errorf("workspace: invalid compiled output type")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("workspace: prepare outputs: %w", err)
	}
	return records, plans, targets, namespaces, nil
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

func appendSchema(schema *schemaCursor, segment schemaSegment) *schemaCursor {
	length := 1
	if schema != nil {
		length += schema.length
	}
	return &schemaCursor{parent: schema, segment: segment, length: length}
}

func materializeSchema(schema *schemaCursor) []schemaSegment {
	if schema == nil {
		return nil
	}
	segments := make([]schemaSegment, schema.length)
	for index := len(segments) - 1; index >= 0; index-- {
		segments[index] = schema.segment
		schema = schema.parent
	}
	return segments
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
