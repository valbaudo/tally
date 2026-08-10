package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
	"github.com/valbaudo/dawn/workspace"
)

func TestPrepareCreatesFreshEmptyWorkspace(t *testing.T) {
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), value.EmptyContract(), nil, nil)

	environment, err := workspace.Prepare(context.Background(), repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })

	entries, err := os.ReadDir(environment.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace entries = %v, want empty", entries)
	}
	if environment.Manifest().Inputs() != nil || environment.Manifest().Outputs() != nil {
		t.Fatal("empty leaf has materialized manifest records")
	}
}

func TestParallelPrepareKeepsWritableWorkspacesIndependent(t *testing.T) {
	ctx := context.Background()
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "shared.txt"), []byte("committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	inputs := mustContract(t, mustField(t, "source", value.Tree()))
	leaf := compileLeaf(t, workflow.Agent, inputs, value.EmptyContract(), []string{"source"}, nil)
	input := mustObject(t, mustEntry(t, "source", value.NewTreeValue(tree)))

	environments := make([]*workspace.Environment, 2)
	errorsByIndex := make([]error, 2)
	var wait sync.WaitGroup
	for index := range environments {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			environments[index], errorsByIndex[index] = workspace.Prepare(ctx, repository, leaf, input)
		}(index)
	}
	wait.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("Prepare[%d]: %v", index, err)
		}
		t.Cleanup(func() { _ = environments[index].Close() })
	}
	if environments[0].Workspace() == environments[1].Workspace() {
		t.Fatal("parallel preparations shared a writable directory")
	}
	for index, data := range []string{"first edit", "second edit"} {
		if err := os.WriteFile(filepath.Join(environments[index].Workspace(), "shared.txt"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for index, want := range []string{"first edit", "second edit"} {
		got, err := os.ReadFile(filepath.Join(environments[index].Workspace(), "shared.txt"))
		if err != nil || string(got) != want {
			t.Fatalf("workspace[%d] bytes = (%q, %v), want %q", index, got, err, want)
		}
	}
	rematerialized := filepath.Join(t.TempDir(), "committed")
	if err := repository.MaterializeTree(ctx, tree, rematerialized); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rematerialized, "shared.txt"))
	if err != nil || string(got) != "committed" {
		t.Fatalf("committed tree bytes = (%q, %v), want committed", got, err)
	}
}

func TestPrepareRollsBackCompleteRuntimeRootOnInputFailure(t *testing.T) {
	runtimeParent := t.TempDir()
	t.Setenv("TMPDIR", runtimeParent)
	store := &failCopyStore{Memory: content.NewMemory()}
	repository, err := content.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := repository.CaptureTree(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	file := ingestFile(t, repository, "side.txt", "text/plain", "side")
	fileType, err := value.File()
	if err != nil {
		t.Fatal(err)
	}
	inputs := mustContract(t, mustField(t, "base", value.Tree()), mustField(t, "side", fileType))
	leaf := compileLeaf(t, workflow.Script, inputs, value.EmptyContract(), []string{"base"}, nil)
	input := mustObject(t, mustEntry(t, "base", value.NewTreeValue(base)), mustEntry(t, "side", value.NewFileValue(file)))
	before := runtimeRoots(t)
	store.mu.Lock()
	store.failAt = 3 // base manifest, base member, then the staged side file
	store.mu.Unlock()

	environment, err := workspace.Prepare(ctx, repository, leaf, input)
	if err == nil {
		t.Fatal("Prepare accepted an injected missing/corrupt input")
	}
	if environment != nil {
		t.Fatal("failed Prepare returned a partial environment")
	}
	if after := runtimeRoots(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("runtime roots after rollback = %q, want %q", after, before)
	}
}

func TestPrepareRejectsLeafWithoutFilesystemRole(t *testing.T) {
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	leaf := compileLeaf(t, workflow.LLM, value.EmptyContract(), value.EmptyContract(), nil, nil)

	environment, err := workspace.Prepare(context.Background(), repository, leaf, mustObject(t))
	if err == nil {
		t.Fatal("Prepare accepted an LLM leaf")
	}
	if environment != nil {
		t.Fatal("rejected LLM preparation returned an environment")
	}
}

func TestPrepareCancellationReturnsNoEnvironment(t *testing.T) {
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), value.EmptyContract(), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare error = %v, want context cancellation", err)
	}
	if environment != nil {
		t.Fatal("cancelled preparation returned an environment")
	}
}

func TestPrepareCloseIsIdempotentAndRemovesAllPrivateState(t *testing.T) {
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	fileType, err := value.File()
	if err != nil {
		t.Fatal(err)
	}
	inputs := mustContract(t, mustField(t, "input", fileType))
	outputs := mustContract(t, mustField(t, "output", value.Tree()))
	file := ingestFile(t, repository, "input.txt", "text/plain", "input")
	leaf := compileLeaf(t, workflow.Script, inputs, outputs, nil, nil)
	environment, err := workspace.Prepare(context.Background(), repository, leaf, mustObject(t, mustEntry(t, "input", value.NewFileValue(file))))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(environment.Workspace())

	if err := environment.Close(); err != nil {
		t.Fatal(err)
	}
	if err := environment.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private runtime root status = %v, want absent", err)
	}
	if _, err := environment.Output(value.Path{}.Field("output")); err == nil {
		t.Fatal("closed environment resolved an output")
	}
}

func TestPrepareDeepFiniteInputAndOutputSchemasDoNotOverflowStack(t *testing.T) {
	const childCase = "DAWN_DEEP_WORKSPACE_PREPARE_CASE"
	if os.Getenv(childCase) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPrepareDeepFiniteInputAndOutputSchemasDoNotOverflowStack$", "-test.count=1")
		command.Env = append(os.Environ(), childCase+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("deep workspace preparation did not return an ordinary result: %v\n%s", err, output)
		}
		return
	}

	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	file := ingestFile(t, repository, "deep.txt", "text/plain", "deep")
	inputType, err := value.File()
	if err != nil {
		t.Fatal(err)
	}
	inputValue := value.NewFileValue(file)
	outputType := value.Tree()
	concreteOutput := value.Path{}.Field("output")
	for range 50_000 {
		inputType, err = value.List(inputType)
		if err != nil {
			t.Fatal(err)
		}
		inputValue = value.NewList(inputValue)
		outputType, err = value.List(outputType)
		if err != nil {
			t.Fatal(err)
		}
		concreteOutput = concreteOutput.ListIndex(0)
	}
	inputs := mustContract(t, mustField(t, "input", inputType))
	outputs := mustContract(t, mustField(t, "output", outputType))
	leaf := compileLeaf(t, workflow.Script, inputs, outputs, nil, nil)
	input := mustObject(t, mustEntry(t, "input", inputValue))

	debug.SetMaxStack(1 << 20)
	result := make(chan error, 1)
	go func() {
		environment, err := workspace.Prepare(context.Background(), repository, leaf, input)
		if err == nil {
			_, err = environment.Output(concreteOutput)
		}
		if environment != nil {
			err = errors.Join(err, environment.Close())
		}
		result <- err
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestPrepareDeepFiniteTreeUnderLowStackSubprocess(t *testing.T) {
	const childCase = "DAWN_DEEP_WORKSPACE_TREE_PREPARE_CASE"
	if os.Getenv(childCase) == "" {
		command := exec.Command(os.Args[0], "-test.run=^TestPrepareDeepFiniteTreeUnderLowStackSubprocess$", "-test.count=1")
		command.Env = append(os.Environ(), childCase+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("deep tree Prepare did not return ordinarily: %v\n%s", err, output)
		}
		return
	}

	const depth = 512
	source := t.TempDir()
	if err := createDeepDirectoryTree(source, depth); err != nil {
		t.Fatal(err)
	}
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repository.CaptureTree(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	inputs := mustContract(t, mustField(t, "tree", value.Tree()))
	leaf := compileLeaf(t, workflow.Script, inputs, value.EmptyContract(), nil, nil)
	input := mustObject(t, mustEntry(t, "tree", value.NewTreeValue(tree)))

	previous := debug.SetMaxStack(64 << 10)
	result := make(chan struct {
		environment *workspace.Environment
		err         error
	}, 1)
	go func() {
		environment, prepareErr := workspace.Prepare(context.Background(), repository, leaf, input)
		result <- struct {
			environment *workspace.Environment
			err         error
		}{environment: environment, err: prepareErr}
	}()
	prepared := <-result
	debug.SetMaxStack(previous)
	if prepared.environment != nil {
		defer func() {
			if err := prepared.environment.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
}

func TestEnvironmentCloseDeepFiniteTreeUnderLowStackSubprocess(t *testing.T) {
	const childCase = "DAWN_DEEP_WORKSPACE_CLOSE_CASE"
	if os.Getenv(childCase) == "" {
		command := exec.Command(os.Args[0], "-test.run=^TestEnvironmentCloseDeepFiniteTreeUnderLowStackSubprocess$", "-test.count=1")
		command.Env = append(os.Environ(), childCase+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("deep tree Environment.Close did not return ordinarily: %v\n%s", err, output)
		}
		return
	}

	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), value.EmptyContract(), nil, nil)
	environment, err := workspace.Prepare(context.Background(), repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := createDeepDirectoryTree(environment.Workspace(), 512); err != nil {
		t.Fatal(err)
	}

	debug.SetMaxStack(64 << 10)
	result := make(chan error, 1)
	go func() { result <- environment.Close() }()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func createDeepDirectoryTree(rootPath string, depth int) (err error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	for range depth {
		if err := root.Mkdir("d", 0o700); err != nil {
			return err
		}
		child, err := root.OpenRoot("d")
		if err != nil {
			return err
		}
		if err := root.Close(); err != nil {
			_ = child.Close()
			return err
		}
		root = child
	}
	return nil
}

func TestPrepareMaterializesBaseAndNamedInputs(t *testing.T) {
	ctx := context.Background()
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	baseSource := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseSource, "committed.txt"), []byte("base bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := repository.CaptureTree(ctx, baseSource)
	if err != nil {
		t.Fatal(err)
	}
	pdf := ingestFile(t, repository, "../report/final.pdf", "application/pdf", "%PDF-1.7\nimmutable")
	text := ingestFile(t, repository, "logical/name.txt", "text/plain", "committed input")
	treeSource := t.TempDir()
	if err := os.WriteFile(filepath.Join(treeSource, "nested"), []byte("tree input"), 0o600); err != nil {
		t.Fatal(err)
	}
	sideTree, err := repository.CaptureTree(ctx, treeSource)
	if err != nil {
		t.Fatal(err)
	}

	fileType, err := value.File("text/plain")
	if err != nil {
		t.Fatal(err)
	}
	pdfType, err := value.File("application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	objectType := mustTypeObject(t, mustField(t, "item", fileType))
	listType, err := value.List(fileType)
	if err != nil {
		t.Fatal(err)
	}
	mapType, err := value.Map(fileType)
	if err != nil {
		t.Fatal(err)
	}
	inputs := mustContract(t,
		mustField(t, "base", value.Tree()),
		mustField(t, "list", listType),
		mustField(t, "mapped", mapType),
		mustField(t, "object", objectType),
		mustField(t, "pdf", pdfType),
		mustField(t, "tree", value.Tree()),
	)
	adversarial := []string{"", "../x", "a/b", "a\\b", "e\u0301", "é", string([]byte{0xff})}
	mapEntries := make([]value.Entry, 0, len(adversarial))
	for _, key := range adversarial {
		mapEntries = append(mapEntries, mustEntry(t, key, value.NewFileValue(text)))
	}
	mapped, err := value.NewMap(mapEntries...)
	if err != nil {
		t.Fatal(err)
	}
	input := mustObject(t,
		mustEntry(t, "base", value.NewTreeValue(base)),
		mustEntry(t, "list", value.NewList(value.NewFileValue(text), value.NewFileValue(text))),
		mustEntry(t, "mapped", mapped),
		mustEntry(t, "object", mustObject(t, mustEntry(t, "item", value.NewFileValue(text)))),
		mustEntry(t, "pdf", value.NewFileValue(pdf)),
		mustEntry(t, "tree", value.NewTreeValue(sideTree)),
	)
	leaf := compileLeaf(t, workflow.Agent, inputs, value.EmptyContract(), []string{"base"}, nil)

	environment, err := workspace.Prepare(ctx, repository, leaf, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })

	workspaceEntries, err := os.ReadDir(environment.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(workspaceEntries); !reflect.DeepEqual(got, []string{"committed.txt"}) {
		t.Fatalf("workspace entries = %q, want selected base tree only", got)
	}
	manifestInputs := environment.Manifest().Inputs()
	if got, want := len(manifestInputs), 1+2+len(adversarial)+1+1+1; got != want {
		t.Fatalf("manifest inputs = %d, want %d", got, want)
	}
	if !manifestInputs[0].Workspace() || manifestInputs[0].Location() != environment.Workspace() {
		t.Fatalf("base record = (%v, %q), want workspace", manifestInputs[0].Workspace(), manifestInputs[0].Location())
	}
	if !base.Digest().Equal(manifestInputs[0].Digest()) {
		t.Fatal("base record lost its tree digest")
	}

	seenLocations := map[string]bool{environment.Workspace(): true}
	var stagedText workspace.Input
	var pdfRecord workspace.Input
	for _, record := range manifestInputs[1:] {
		if record.Workspace() {
			t.Fatalf("non-base input %s marked as workspace", record.ValuePath().String())
		}
		if filepath.Dir(filepath.Dir(filepath.Dir(record.Location()))) == filepath.Dir(environment.Workspace()) {
			// The location is beneath the runtime input root, a sibling of workspace.
		} else {
			t.Fatalf("input location %q is not in the invocation root", record.Location())
		}
		if pathWithin(record.Location(), environment.Workspace()) {
			t.Fatalf("staged input %q is inside writable workspace", record.Location())
		}
		if seenLocations[record.Location()] {
			t.Fatalf("duplicate staged location %q", record.Location())
		}
		seenLocations[record.Location()] = true
		if !regexp.MustCompile(`^inputs/[0-9a-f]{64}/value$`).MatchString(filepath.ToSlash(record.RelativeLocation())) {
			t.Fatalf("relative input location = %q, want fixed hashed slot", record.RelativeLocation())
		}
		if record.ValuePath().String() == `$.field("pdf")` {
			pdfRecord = record
		}
		name, fileRecord := record.Name()
		if fileRecord && name == text.Name() && record.Digest().Equal(text.Digest()) && stagedText.Location() == "" {
			media, mediaOK := record.Media()
			size, sizeOK := record.Size()
			if !mediaOK || media != text.Media() || !sizeOK || size != text.Size() {
				t.Fatalf("file metadata = (%q, %v, %d, %v), want (%q, true, %d, true)", media, mediaOK, size, sizeOK, text.Media(), text.Size())
			}
			stagedText = record
		}
	}
	if stagedText.Location() == "" {
		t.Fatal("manifest has no staged text file")
	}
	if pdfRecord.Kind() != value.FileKind || !pdfRecord.Digest().Equal(pdf.Digest()) {
		t.Fatal("PDF manifest record lost its kind or digest")
	}
	if name, ok := pdfRecord.Name(); !ok || name != pdf.Name() {
		t.Fatalf("PDF logical name = (%q, %v), want (%q, true)", name, ok, pdf.Name())
	}
	if media, ok := pdfRecord.Media(); !ok || media != pdf.Media() {
		t.Fatalf("PDF media = (%q, %v), want (%q, true)", media, ok, pdf.Media())
	}
	if size, ok := pdfRecord.Size(); !ok || size != pdf.Size() {
		t.Fatalf("PDF size = (%d, %v), want (%d, true)", size, ok, pdf.Size())
	}
	if info, err := os.Stat(stagedText.Location()); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("staged file mode = (%v, %v), want read-only presentation", info, err)
	}
	if err := os.WriteFile(stagedText.Location(), []byte("attempted edit"), 0o600); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write to read-only presentation error = %v, want permission error", err)
	}
	if err := os.Chmod(stagedText.Location(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedText.Location(), []byte("disposable edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	var committed bytes.Buffer
	if err := repository.CopyFile(ctx, text, &committed); err != nil {
		t.Fatal(err)
	}
	if committed.String() != "committed input" {
		t.Fatalf("committed content changed to %q", committed.String())
	}

	manifestInputs[0] = workspace.Input{}
	if environment.Manifest().Inputs()[0].ValuePath().String() != `$.field("base")` {
		t.Fatal("manifest input slice was not defensive")
	}
}

func compileLeaf(t *testing.T, kind workflow.LeafKind, inputs, outputs value.Contract, baseTree, publishWorkspace []string) workflow.Leaf {
	t.Helper()
	graph := workflow.GraphDraft{
		Inputs:  inputs,
		Outputs: outputs,
		Nodes: []workflow.NodeDraft{{
			Name: "leaf",
			Leaf: &workflow.LeafDraft{
				Kind: kind, Inputs: inputs, Outputs: outputs,
				BaseTree: baseTree, PublishWorkspace: publishWorkspace,
			},
		}},
	}
	if ports := inputs.Ports(); len(ports) != 0 {
		bindings := make([]workflow.BindingDraft, len(ports))
		for index, port := range ports {
			bindings[index] = workflow.BindingDraft{From: []string{port.Name()}, To: port.Name()}
		}
		graph.Edges = append(graph.Edges, workflow.EdgeDraft{
			From:     workflow.EndpointDraft{Kind: workflow.Boundary},
			To:       workflow.EndpointDraft{Kind: workflow.Child, Child: "leaf"},
			Bindings: bindings,
		})
	}
	if ports := outputs.Ports(); len(ports) != 0 {
		bindings := make([]workflow.BindingDraft, len(ports))
		for index, port := range ports {
			bindings[index] = workflow.BindingDraft{From: []string{port.Name()}, To: port.Name()}
		}
		graph.Edges = append(graph.Edges, workflow.EdgeDraft{
			From:     workflow.EndpointDraft{Kind: workflow.Child, Child: "leaf"},
			To:       workflow.EndpointDraft{Kind: workflow.Boundary},
			Bindings: bindings,
		})
	}
	definition, err := workflow.Compile(workflow.ProgramDraft{
		Root: "root",
		Modules: []workflow.ModuleDraft{{
			Name:  "root",
			Graph: graph,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := definition.Root().Nodes()
	leaf, ok := nodes[0].Leaf()
	if !ok {
		t.Fatal("compiled node is not a leaf")
	}
	return leaf
}

func mustObject(t *testing.T, entries ...value.Entry) value.Value {
	t.Helper()
	object, err := value.NewObject(entries...)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func ingestFile(t *testing.T, repository *content.Repository, name, media, data string) content.File {
	t.Helper()
	file, err := repository.IngestFile(context.Background(), name, media, bytes.NewBufferString(data))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func mustField(t *testing.T, name string, typ value.Type) value.Field {
	t.Helper()
	field, err := value.Required(name, typ)
	if err != nil {
		t.Fatal(err)
	}
	return field
}

func mustContract(t *testing.T, fields ...value.Field) value.Contract {
	t.Helper()
	contract, err := value.NewContract(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func mustTypeObject(t *testing.T, fields ...value.Field) value.Type {
	t.Helper()
	typ, err := value.Object(fields...)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func mustEntry(t *testing.T, name string, member value.Value) value.Entry {
	t.Helper()
	entry, err := value.NewEntry(name, member)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func names(entries []os.DirEntry) []string {
	result := make([]string, len(entries))
	for index, entry := range entries {
		result[index] = entry.Name()
	}
	return result
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && (relative == "." || len(relative) < 3 || relative[:3] != "../")
}

type failCopyStore struct {
	*content.Memory
	mu     sync.Mutex
	copies int
	failAt int
}

func (s *failCopyStore) Copy(ctx context.Context, digest content.Digest, destination io.Writer) (int64, error) {
	s.mu.Lock()
	s.copies++
	copyNumber := s.copies
	failAt := s.failAt
	s.mu.Unlock()
	if failAt != 0 && copyNumber == failAt {
		return 0, errors.New("injected missing committed content")
	}
	return s.Memory.Copy(ctx, digest, destination)
}

func runtimeRoots(t *testing.T) []string {
	t.Helper()
	roots, err := filepath.Glob(filepath.Join(os.TempDir(), "dawn-workspace-*"))
	if err != nil {
		t.Fatal(err)
	}
	return roots
}
