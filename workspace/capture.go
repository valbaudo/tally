package workspace

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
)

// Outputs is the invocation-scoped capability for capturing declared file,
// tree, and published-workspace outputs while one candidate is assembled.
// Its methods stop working when the Capture callback returns.
type Outputs struct {
	state *captureState
}

type captureState struct {
	mu        sync.Mutex
	env       *Environment
	active    bool
	published value.Path
	captured  map[string]capturedOutput
	failure   error
}

type capturedOutput struct {
	path  value.Path
	value value.Value
}

// File captures one declared file target with its inherent logical metadata.
// The metadata is semantic data and is independent of the fixed physical slot.
func (o Outputs) File(ctx context.Context, path value.Path, logicalName, concreteMedia string) (value.Value, error) {
	if o.state == nil {
		return value.Value{}, fmt.Errorf("workspace: outputs capability is invalid")
	}
	return o.state.captureOutput(ctx, path, value.FileKind, logicalName, concreteMedia)
}

// Tree captures one declared tree target.
func (o Outputs) Tree(ctx context.Context, path value.Path) (value.Value, error) {
	if o.state == nil {
		return value.Value{}, fmt.Errorf("workspace: outputs capability is invalid")
	}
	return o.state.captureOutput(ctx, path, value.TreeKind, "", "")
}

// Workspace captures the private workspace only when the compiled leaf
// declares a workspace-publication output.
func (o Outputs) Workspace(ctx context.Context) (value.Value, error) {
	if o.state == nil {
		return value.Value{}, fmt.Errorf("workspace: outputs capability is invalid")
	}
	return o.state.captureWorkspace(ctx)
}

// Capture assembles and validates one complete output candidate. Any error
// returns the zero Value, even when immutable bytes were already stored.
func (e *Environment) Capture(ctx context.Context, assemble func(Outputs) (value.Value, error)) (value.Value, error) {
	if e == nil {
		return value.Value{}, fmt.Errorf("workspace: environment is nil")
	}
	if ctx == nil {
		return value.Value{}, fmt.Errorf("workspace: context must not be nil")
	}
	if assemble == nil {
		return value.Value{}, fmt.Errorf("workspace: capture callback must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return value.Value{}, fmt.Errorf("workspace: capture: %w", err)
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return value.Value{}, fmt.Errorf("workspace: environment is closed")
	}
	if e.repository == nil {
		e.mu.Unlock()
		return value.Value{}, fmt.Errorf("workspace: content repository is unavailable")
	}
	publishedFields := append([]string(nil), e.publishedWorkspace...)
	e.mu.Unlock()

	state := &captureState{
		env: e, active: true, published: pathFromFields(publishedFields),
		captured: make(map[string]capturedOutput),
	}
	if len(publishedFields) == 0 {
		state.published = value.Path{}
	}
	defer state.stop()

	candidate, err := assemble(Outputs{state: state})
	state.stop()
	captureErr := state.captureFailure()
	if err != nil || captureErr != nil {
		return value.Value{}, fmt.Errorf("workspace: assemble output candidate: %w", errors.Join(err, captureErr))
	}
	if err := ctx.Err(); err != nil {
		return value.Value{}, fmt.Errorf("workspace: capture: %w", err)
	}
	if err := e.outputs.Validate(candidate); err != nil {
		return value.Value{}, fmt.Errorf("workspace: output candidate does not satisfy leaf contract: %w", err)
	}
	captured := state.snapshot()
	if err := validateCandidateCaptures(e.outputs, candidate, captured); err != nil {
		return value.Value{}, err
	}
	if err := e.validatePhysicalOutputs(ctx); err != nil {
		return value.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return value.Value{}, fmt.Errorf("workspace: capture: %w", err)
	}
	return candidate, nil
}

func (s *captureState) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
}

func (s *captureState) snapshot() map[string]capturedOutput {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]capturedOutput, len(s.captured))
	for key, captured := range s.captured {
		result[key] = captured
	}
	return result
}

func (s *captureState) captureFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *captureState) captureOutput(ctx context.Context, path value.Path, expected value.Kind, logicalName, concreteMedia string) (result value.Value, err error) {
	s.mu.Lock()
	defer func() {
		if err != nil && s.active {
			s.failure = errors.Join(s.failure, err)
		}
		s.mu.Unlock()
	}()
	if !s.active {
		return value.Value{}, fmt.Errorf("workspace: outputs capability is no longer active")
	}
	if ctx == nil {
		return value.Value{}, fmt.Errorf("workspace: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return value.Value{}, fmt.Errorf("workspace: capture output %s: %w", path.String(), err)
	}
	key := string(canonicalPath(path))
	typ, _, err := resolveOutputSchema(s.env.outputs, path)
	if err != nil {
		return value.Value{}, err
	}
	if typ.Kind() != expected {
		return value.Value{}, fmt.Errorf("workspace: output path %s selects %v, not %v", path.String(), typ.Kind(), expected)
	}
	if prior, ok := s.captured[key]; ok {
		if expected == value.FileKind {
			file, ok := prior.value.File()
			if !ok {
				return value.Value{}, fmt.Errorf("workspace: prior file capture is invalid")
			}
			requested, metadataErr := content.NewFile(file.Digest(), file.Size(), logicalName, concreteMedia)
			if metadataErr != nil {
				return value.Value{}, metadataErr
			}
			if !file.Equal(requested) {
				return value.Value{}, fmt.Errorf("workspace: repeated file capture metadata differs from the first capture")
			}
		}
		return prior.value, nil
	}
	target, err := s.env.Output(path)
	if err != nil {
		return value.Value{}, err
	}
	captured, err := s.captureTarget(ctx, target, logicalName, concreteMedia)
	if err != nil {
		return value.Value{}, fmt.Errorf("workspace: capture output %s: %w", path.String(), err)
	}
	if err := typ.Validate(captured); err != nil {
		return value.Value{}, fmt.Errorf("workspace: captured output %s does not satisfy its contract: %w", path.String(), err)
	}
	s.captured[key] = capturedOutput{path: cloneValuePath(path), value: captured}
	return captured, nil
}

func (s *captureState) captureWorkspace(ctx context.Context) (result value.Value, err error) {
	s.mu.Lock()
	defer func() {
		if err != nil && s.active {
			s.failure = errors.Join(s.failure, err)
		}
		s.mu.Unlock()
	}()
	if !s.active {
		return value.Value{}, fmt.Errorf("workspace: outputs capability is no longer active")
	}
	if ctx == nil {
		return value.Value{}, fmt.Errorf("workspace: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return value.Value{}, fmt.Errorf("workspace: capture workspace: %w", err)
	}
	if len(s.env.publishedWorkspace) == 0 {
		return value.Value{}, fmt.Errorf("workspace: leaf does not publish its workspace")
	}
	key := string(canonicalPath(s.published))
	if prior, ok := s.captured[key]; ok {
		return prior.value, nil
	}

	s.env.mu.Lock()
	if s.env.closed {
		s.env.mu.Unlock()
		return value.Value{}, fmt.Errorf("workspace: environment is closed")
	}
	location := s.env.workspace
	identity := s.env.workspaceIdentity
	repository := s.env.repository
	s.env.mu.Unlock()

	root, err := openVerifiedDirectory(location, identity)
	if err != nil {
		return value.Value{}, fmt.Errorf("workspace: open private workspace: %w", err)
	}
	first, captureErr := repository.CaptureTreeRoot(ctx, root)
	var tree content.Tree
	if captureErr == nil {
		tree, captureErr = repository.CaptureTreeRoot(ctx, root)
	}
	if captureErr == nil && !first.Equal(tree) {
		captureErr = fmt.Errorf("private workspace was not stable across verified captures")
	}
	if captureErr == nil {
		captureErr = verifyDirectoryLocation(root, location, identity)
	}
	if closeErr := root.Close(); closeErr != nil {
		captureErr = errors.Join(captureErr, fmt.Errorf("close private workspace: %w", closeErr))
	}
	if captureErr != nil {
		return value.Value{}, fmt.Errorf("workspace: capture workspace: %w", captureErr)
	}
	captured := value.NewTreeValue(tree)
	if !captured.Valid() {
		return value.Value{}, fmt.Errorf("workspace: captured workspace is invalid")
	}
	s.captured[key] = capturedOutput{path: cloneValuePath(s.published), value: captured}
	return captured, nil
}

func (s *captureState) captureTarget(ctx context.Context, target Target, logicalName, concreteMedia string) (result value.Value, err error) {
	slotLocation := filepath.Dir(target.location)
	root, err := openVerifiedDirectory(slotLocation, target.slotIdentity)
	if err != nil {
		return value.Value{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			result = value.Value{}
			err = errors.Join(err, fmt.Errorf("close output slot: %w", closeErr))
		}
	}()

	entryInfo, err := inspectTargetSlot(root, target, false)
	if err != nil {
		return value.Value{}, err
	}
	switch target.kind {
	case value.FileKind:
		first, err := s.ingestOutputFile(ctx, root, target, entryInfo, logicalName, concreteMedia)
		if err != nil {
			return value.Value{}, err
		}
		second, err := s.ingestOutputFile(ctx, root, target, entryInfo, logicalName, concreteMedia)
		if err != nil {
			return value.Value{}, err
		}
		if !first.Equal(second) {
			return value.Value{}, fmt.Errorf("output file was not stable across verified captures")
		}
		return value.NewFileValue(second), nil
	case value.TreeKind:
		targetRoot, err := root.OpenRoot(filepath.Base(target.location))
		if err != nil {
			return value.Value{}, fmt.Errorf("open output tree capability: %w", err)
		}
		openedInfo, inspectErr := targetRoot.Lstat(".")
		if inspectErr != nil || !openedInfo.IsDir() || !os.SameFile(entryInfo, openedInfo) {
			closeErr := targetRoot.Close()
			if inspectErr != nil {
				return value.Value{}, errors.Join(fmt.Errorf("inspect output tree capability: %w", inspectErr), closeErr)
			}
			return value.Value{}, errors.Join(fmt.Errorf("output tree identity changed while opening"), closeErr)
		}
		first, captureErr := s.env.repository.CaptureTreeRoot(ctx, targetRoot)
		var tree content.Tree
		if captureErr == nil {
			tree, captureErr = s.env.repository.CaptureTreeRoot(ctx, targetRoot)
		}
		if captureErr == nil && !first.Equal(tree) {
			captureErr = fmt.Errorf("output tree was not stable across verified captures")
		}
		afterInfo, afterErr := targetRoot.Lstat(".")
		closeErr := targetRoot.Close()
		if captureErr != nil || afterErr != nil || closeErr != nil {
			if afterErr != nil {
				afterErr = fmt.Errorf("inspect captured output tree capability: %w", afterErr)
			}
			return value.Value{}, errors.Join(captureErr, afterErr, closeErr)
		}
		if !afterInfo.IsDir() || !os.SameFile(entryInfo, afterInfo) {
			return value.Value{}, fmt.Errorf("output tree identity changed while being captured")
		}
		postInfo, err := inspectTargetSlot(root, target, false)
		if err != nil || !os.SameFile(entryInfo, postInfo) {
			if err != nil {
				return value.Value{}, err
			}
			return value.Value{}, fmt.Errorf("output tree identity changed while being captured")
		}
		if err := verifyDirectoryLocation(root, slotLocation, target.slotIdentity); err != nil {
			return value.Value{}, err
		}
		return value.NewTreeValue(tree), nil
	default:
		return value.Value{}, fmt.Errorf("output target has unsupported kind %v", target.kind)
	}
}

func (s *captureState) ingestOutputFile(ctx context.Context, root *os.Root, target Target, expected fs.FileInfo, logicalName, concreteMedia string) (content.File, error) {
	file, err := root.Open(filepath.Base(target.location))
	if err != nil {
		return content.File{}, fmt.Errorf("open output file: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(expected, openedInfo) {
		closeErr := file.Close()
		if statErr != nil {
			return content.File{}, errors.Join(fmt.Errorf("inspect opened output file: %w", statErr), closeErr)
		}
		return content.File{}, errors.Join(fmt.Errorf("output file identity changed while opening"), closeErr)
	}
	semantic, ingestErr := s.env.repository.IngestFile(ctx, logicalName, concreteMedia, file)
	afterInfo, afterErr := file.Stat()
	closeErr := file.Close()
	if ingestErr != nil || afterErr != nil || closeErr != nil {
		if afterErr != nil {
			afterErr = fmt.Errorf("inspect captured output file: %w", afterErr)
		}
		return content.File{}, errors.Join(ingestErr, afterErr, closeErr)
	}
	if !afterInfo.Mode().IsRegular() || !os.SameFile(expected, afterInfo) || expected.Size() != afterInfo.Size() || afterInfo.Size() != semantic.Size() {
		return content.File{}, fmt.Errorf("output file changed while being captured")
	}
	postInfo, err := inspectTargetSlot(root, target, false)
	if err != nil || !os.SameFile(expected, postInfo) {
		if err != nil {
			return content.File{}, err
		}
		return content.File{}, fmt.Errorf("output file identity changed while being captured")
	}
	if err := verifyDirectoryLocation(root, filepath.Dir(target.location), target.slotIdentity); err != nil {
		return content.File{}, err
	}
	return semantic, nil
}

func openVerifiedDirectory(location string, expected fs.FileInfo) (*os.Root, error) {
	if location == "" || expected == nil || !expected.IsDir() {
		return nil, fmt.Errorf("prepared directory identity is invalid")
	}
	pathInfo, err := os.Lstat(location)
	if err != nil {
		return nil, fmt.Errorf("inspect prepared directory: %w", err)
	}
	if !pathInfo.IsDir() || !os.SameFile(expected, pathInfo) {
		return nil, fmt.Errorf("prepared directory path identity changed")
	}
	root, err := os.OpenRoot(location)
	if err != nil {
		return nil, fmt.Errorf("open prepared directory: %w", err)
	}
	if err := verifyDirectoryLocation(root, location, expected); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}

func verifyDirectoryLocation(root *os.Root, location string, expected fs.FileInfo) error {
	opened, err := root.Lstat(".")
	if err != nil {
		return fmt.Errorf("inspect opened prepared directory: %w", err)
	}
	pathInfo, err := os.Lstat(location)
	if err != nil {
		return fmt.Errorf("inspect prepared directory path: %w", err)
	}
	if !opened.IsDir() || !pathInfo.IsDir() || !os.SameFile(expected, opened) || !os.SameFile(expected, pathInfo) {
		return fmt.Errorf("prepared directory identity changed")
	}
	return nil
}

func inspectTargetSlot(root *os.Root, target Target, allowMissingFile bool) (fs.FileInfo, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("enumerate output slot: %w", err)
	}
	if allowMissingFile && target.kind == value.FileKind && len(entries) == 0 {
		return nil, nil
	}
	name := filepath.Base(target.location)
	if len(entries) != 1 || entries[0].Name() != name {
		return nil, fmt.Errorf("output slot must contain exactly the declared %q entry", name)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect output target: %w", err)
	}
	switch target.kind {
	case value.FileKind:
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("file output target must be one regular file")
		}
	case value.TreeKind:
		if !info.IsDir() || target.targetIdentity == nil || !os.SameFile(target.targetIdentity, info) {
			return nil, fmt.Errorf("tree output target identity changed")
		}
	default:
		return nil, fmt.Errorf("output target has unsupported kind %v", target.kind)
	}
	return info, nil
}

type candidatePath struct {
	parent *candidatePath
	kind   value.SegmentKind
	name   string
	index  int
	length int
}

type candidateTask struct {
	typ   value.Type
	value value.Value
	path  *candidatePath
}

func childCandidatePath(parent *candidatePath, kind value.SegmentKind, name string, index int) *candidatePath {
	length := 1
	if parent != nil {
		length += parent.length
	}
	return &candidatePath{parent: parent, kind: kind, name: name, index: index, length: length}
}

func canonicalCandidatePath(path *candidatePath) []byte {
	if path == nil {
		return binary.BigEndian.AppendUint64(nil, 0)
	}
	segments := make([]*candidatePath, path.length)
	for index := len(segments) - 1; index >= 0; index-- {
		segments[index] = path
		path = path.parent
	}
	encoded := make([]byte, 0, 8+len(segments)*10)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(segments)))
	for _, segment := range segments {
		encoded = append(encoded, byte(segment.kind))
		switch segment.kind {
		case value.FieldSegment, value.MapKeySegment:
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(segment.name)))
			encoded = append(encoded, segment.name...)
		case value.ListIndexSegment:
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(int64(segment.index)))
		}
	}
	return encoded
}

func validateCandidateCaptures(contract value.Contract, candidate value.Value, captured map[string]capturedOutput) error {
	ports := contract.Ports()
	entries := candidate.Entries()
	tasks := make([]candidateTask, 0, len(entries))
	entryIndex := 0
	for _, port := range ports {
		if entryIndex == len(entries) || entries[entryIndex].Name() != port.Name() {
			continue
		}
		tasks = append(tasks, candidateTask{
			typ: port.Type(), value: entries[entryIndex].Value(),
			path: childCandidatePath(nil, value.FieldSegment, port.Name(), 0),
		})
		entryIndex++
	}
	seen := make(map[string]bool, len(captured))
	for len(tasks) != 0 {
		last := len(tasks) - 1
		task := tasks[last]
		tasks = tasks[:last]
		switch task.typ.Kind() {
		case value.ObjectKind:
			fields := task.typ.Fields()
			members := task.value.Entries()
			children := make([]candidateTask, 0, len(members))
			memberIndex, fieldIndex := 0, 0
			for memberIndex < len(members) && fieldIndex < len(fields) {
				member, field := members[memberIndex], fields[fieldIndex]
				switch {
				case member.Name() < field.Name():
					memberIndex++
				case field.Name() < member.Name():
					fieldIndex++
				default:
					children = append(children, candidateTask{
						typ: field.Type(), value: member.Value(),
						path: childCandidatePath(task.path, value.FieldSegment, field.Name(), 0),
					})
					memberIndex++
					fieldIndex++
				}
			}
			for index := len(children) - 1; index >= 0; index-- {
				tasks = append(tasks, children[index])
			}
		case value.MapKind:
			element, _ := task.typ.Element()
			members := task.value.Entries()
			for index := len(members) - 1; index >= 0; index-- {
				tasks = append(tasks, candidateTask{
					typ: element, value: members[index].Value(),
					path: childCandidatePath(task.path, value.MapKeySegment, members[index].Name(), 0),
				})
			}
		case value.ListKind:
			element, _ := task.typ.Element()
			items := task.value.Items()
			for index := len(items) - 1; index >= 0; index-- {
				tasks = append(tasks, candidateTask{
					typ: element, value: items[index],
					path: childCandidatePath(task.path, value.ListIndexSegment, "", index),
				})
			}
		case value.FileKind, value.TreeKind:
			key := string(canonicalCandidatePath(task.path))
			expected, ok := captured[key]
			if !ok {
				return fmt.Errorf("workspace: output candidate has a file or tree that was not captured for its semantic path")
			}
			if !task.value.Equal(expected.value) {
				return fmt.Errorf("workspace: output candidate %s is not its captured immutable value", expected.path.String())
			}
			if !task.value.SameRuntimeHandle(expected.value) {
				return fmt.Errorf("workspace: output candidate %s was not constructed from its current capture", expected.path.String())
			}
			if seen[key] {
				return fmt.Errorf("workspace: captured output %s appears more than once", expected.path.String())
			}
			seen[key] = true
		}
	}
	for key, expected := range captured {
		if !seen[key] {
			return fmt.Errorf("workspace: requested capture %s is absent from its candidate path", expected.path.String())
		}
	}
	return nil
}

func (e *Environment) validatePhysicalOutputs(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("workspace: environment is closed")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("workspace: validate output slots: %w", err)
	}

	root, err := openVerifiedDirectory(e.outputRoot, e.outputRootIdentity)
	if err != nil {
		return fmt.Errorf("workspace: open output root: %w", err)
	}
	expectedSlots := make(map[string]bool, len(e.plans))
	for _, plan := range e.plans {
		expectedSlots[filepath.Base(plan.location)] = true
	}
	if err := verifyExactDirectoryNames(root, expectedSlots); err != nil {
		_ = root.Close()
		return fmt.Errorf("workspace: validate output root: %w", err)
	}
	if err := verifyDirectoryLocation(root, e.outputRoot, e.outputRootIdentity); err != nil {
		_ = root.Close()
		return fmt.Errorf("workspace: validate output root: %w", err)
	}
	if err := root.Close(); err != nil {
		return fmt.Errorf("workspace: close output root: %w", err)
	}

	targetsByPlan := make(map[string][]Target, len(e.plans))
	for _, target := range e.targets {
		slot := filepath.Dir(target.location)
		planLocation := slot
		if filepath.Dir(slot) != e.outputRoot {
			planLocation = filepath.Dir(slot)
		}
		targetsByPlan[planLocation] = append(targetsByPlan[planLocation], target)
	}
	for _, plan := range e.plans {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("workspace: validate output slots: %w", err)
		}
		planTargets := targetsByPlan[plan.location]
		if plan.dynamic {
			if plan.namespace == nil {
				return fmt.Errorf("workspace: dynamic output namespace is unavailable")
			}
			if err := plan.namespace.verify(); err != nil {
				return fmt.Errorf("workspace: verify dynamic output namespace: %w", err)
			}
			namespace, err := openVerifiedDirectory(plan.location, plan.directoryIdentity)
			if err != nil {
				return fmt.Errorf("workspace: open dynamic output namespace: %w", err)
			}
			expectedMembers := make(map[string]bool, len(planTargets))
			for _, target := range planTargets {
				expectedMembers[filepath.Base(filepath.Dir(target.location))] = true
			}
			inspectErr := verifyExactDirectoryNames(namespace, expectedMembers)
			inspectErr = errors.Join(inspectErr, verifyDirectoryLocation(namespace, plan.location, plan.directoryIdentity))
			inspectErr = errors.Join(inspectErr, namespace.Close())
			if inspectErr != nil {
				return fmt.Errorf("workspace: validate dynamic output namespace: %w", inspectErr)
			}
		} else if len(planTargets) != 1 {
			return fmt.Errorf("workspace: static output slot does not have exactly one prepared target")
		}
		for _, target := range planTargets {
			slot, err := openVerifiedDirectory(filepath.Dir(target.location), target.slotIdentity)
			if err != nil {
				return fmt.Errorf("workspace: open output slot: %w", err)
			}
			_, inspectErr := inspectTargetSlot(slot, target, true)
			inspectErr = errors.Join(inspectErr, verifyDirectoryLocation(slot, filepath.Dir(target.location), target.slotIdentity))
			inspectErr = errors.Join(inspectErr, slot.Close())
			if inspectErr != nil {
				return fmt.Errorf("workspace: validate output slot: %w", inspectErr)
			}
		}
	}
	return nil
}

func verifyExactDirectoryNames(root *os.Root, expected map[string]bool) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("enumerate directory: %w", err)
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("directory has %d entries; want exactly %d declared entries", len(entries), len(expected))
	}
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return fmt.Errorf("directory contains undeclared entry %q", entry.Name())
		}
	}
	return nil
}
