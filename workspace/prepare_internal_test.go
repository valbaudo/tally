package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func TestOutputRetriesCleanupAfterFailedDynamicTreeAllocation(t *testing.T) {
	environment := prepareDynamicTreeEnvironment(t)
	t.Cleanup(func() { _ = environment.Close() })
	var key string
	var plan outputPlan
	for candidateKey, candidate := range environment.plans {
		if candidate.dynamic {
			key, plan = candidateKey, candidate
			break
		}
	}
	if plan.namespace == nil {
		t.Fatal("prepared environment has no dynamic namespace capability")
	}
	fault := &failingOutputNamespace{
		outputNamespaceCapability: plan.namespace,
		failAfterCreate:           1,
		failRemove:                1,
	}
	plan.namespace = fault
	environment.plans[key] = plan
	path := value.Path{}.Field("trees").ListIndex(4)

	_, err := environment.Output(path)
	if err == nil || !strings.Contains(err.Error(), "injected target creation failure") || !strings.Contains(err.Error(), "injected member cleanup failure") {
		t.Fatalf("first Output error = %v, want joined allocation and cleanup failures", err)
	}
	entries, err := os.ReadDir(plan.location)
	if err != nil || len(entries) != 1 {
		t.Fatalf("namespace after failed cleanup = (%v, %v), want one tracked partial member", entries, err)
	}

	target, err := environment.Output(path)
	if err != nil {
		t.Fatalf("second Output did not recover the tracked partial member: %v", err)
	}
	if info, err := os.Stat(target.Location()); err != nil || !info.IsDir() {
		t.Fatalf("retried dynamic tree target = (%v, %v), want directory", info, err)
	}
	entries, err = os.ReadDir(plan.location)
	if err != nil || len(entries) != 1 {
		t.Fatalf("namespace after successful retry = (%v, %v), want one complete member", entries, err)
	}
}

func TestCloseRetriesTransientRootRemovalWhileOutputsStayClosed(t *testing.T) {
	environment := prepareDynamicTreeEnvironment(t)
	t.Cleanup(func() { _ = environment.Close() })
	root := filepath.Dir(environment.Workspace())
	removeCalls := 0
	realRemove := environment.removeRootEntry
	environment.removeRootEntry = func() error {
		removeCalls++
		if removeCalls == 1 {
			return errors.New("injected transient root removal failure")
		}
		return realRemove()
	}

	if err := environment.Close(); err == nil || !strings.Contains(err.Error(), "injected transient root removal failure") {
		t.Fatalf("first Close error = %v, want injected removal failure", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("runtime root after failed Close = %v, want retained for retry", err)
	}
	if _, err := environment.Output(value.Path{}.Field("trees").ListIndex(0)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Output after failed Close = %v, want permanently closed", err)
	}
	if err := environment.Close(); err != nil {
		t.Fatalf("second Close did not retry root cleanup: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime root after successful retry = %v, want absent", err)
	}
	if err := environment.Close(); err != nil {
		t.Fatalf("third idempotent Close = %v, want nil", err)
	}
	if removeCalls != 2 {
		t.Fatalf("root removal calls = %d, want one failure and one retry", removeCalls)
	}
}

func TestCloseRejectsReusedRuntimeRootPathWithoutTouchingReplacement(t *testing.T) {
	environment := prepareDynamicTreeEnvironment(t)
	root := filepath.Dir(environment.Workspace())
	movedRoot := root + ".moved"
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(movedRoot)
	})
	removeCalls := 0
	realRemove := environment.removeRootEntry
	environment.removeRootEntry = func() error {
		removeCalls++
		if removeCalls == 1 {
			return errors.New("injected transient root removal failure")
		}
		return realRemove()
	}

	if err := environment.Close(); err == nil || !strings.Contains(err.Error(), "injected transient root removal failure") {
		t.Fatalf("first Close error = %v, want injected removal failure", err)
	}
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatalf("move original runtime root: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create replacement runtime root: %v", err)
	}
	sentinel := filepath.Join(root, "replacement-sentinel")
	const sentinelContents = "replacement must survive"
	if err := os.WriteFile(sentinel, []byte(sentinelContents), 0o600); err != nil {
		t.Fatalf("write replacement sentinel: %v", err)
	}

	secondErr := environment.Close()
	contents, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(contents) != sentinelContents {
		t.Fatalf("replacement sentinel after second Close = (%q, %v), want preserved bytes", contents, readErr)
	}
	if secondErr == nil || !strings.Contains(secondErr.Error(), "cleanup integrity") || !strings.Contains(secondErr.Error(), "identity changed") {
		t.Fatalf("second Close error = %v, want runtime-root identity error", secondErr)
	}
	thirdErr := environment.Close()
	if thirdErr == nil || !strings.Contains(thirdErr.Error(), "cleanup integrity") || !strings.Contains(thirdErr.Error(), "identity changed") {
		t.Fatalf("third Close error = %v, want retained runtime-root integrity error", thirdErr)
	}
	contents, readErr = os.ReadFile(sentinel)
	if readErr != nil || string(contents) != sentinelContents {
		t.Fatalf("replacement sentinel after repeated Close = (%q, %v), want preserved bytes", contents, readErr)
	}
	if removeCalls != 1 {
		t.Fatalf("root removal calls = %d, want only the initial failed attempt", removeCalls)
	}
}

func TestCloseDoesNotRecursivelyDeleteReplacementSwappedBeforeFinalRemove(t *testing.T) {
	environment := prepareDynamicTreeEnvironment(t)
	root := filepath.Dir(environment.Workspace())
	movedRoot := root + ".moved"
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(movedRoot)
	})
	const sentinelContents = "replacement must survive final removal"
	sentinel := filepath.Join(root, "replacement-sentinel")
	var swapErr error
	swapCalls := 0
	environment.beforeRootEntryRemove = func() {
		swapCalls++
		if err := os.Rename(root, movedRoot); err != nil {
			swapErr = errors.Join(swapErr, fmt.Errorf("move original runtime root: %w", err))
			return
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			swapErr = errors.Join(swapErr, fmt.Errorf("create replacement runtime root: %w", err))
			return
		}
		if err := os.WriteFile(sentinel, []byte(sentinelContents), 0o600); err != nil {
			swapErr = errors.Join(swapErr, fmt.Errorf("write replacement sentinel: %w", err))
		}
	}

	closeErr := environment.Close()
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	movedEntries, movedErr := os.ReadDir(movedRoot)
	if movedErr != nil || len(movedEntries) != 0 {
		t.Fatalf("original runtime root at final removal seam = (%v, %v), want empty directory", movedEntries, movedErr)
	}
	contents, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(contents) != sentinelContents {
		t.Fatalf("replacement sentinel after final removal gap = (%q, %v), want preserved bytes", contents, readErr)
	}
	if closeErr == nil || !strings.Contains(closeErr.Error(), "cleanup integrity") || !strings.Contains(closeErr.Error(), "identity changed") {
		t.Fatalf("Close error = %v, want runtime-root cleanup integrity error", closeErr)
	}
	if err := environment.Close(); err == nil || !strings.Contains(err.Error(), "cleanup integrity") {
		t.Fatalf("repeated Close error = %v, want retained cleanup integrity error", err)
	}
	contents, readErr = os.ReadFile(sentinel)
	if readErr != nil || string(contents) != sentinelContents {
		t.Fatalf("replacement sentinel after repeated Close = (%q, %v), want preserved bytes", contents, readErr)
	}
	if swapCalls != 1 {
		t.Fatalf("final removal seam calls = %d, want exactly one", swapCalls)
	}
}

type failingOutputNamespace struct {
	outputNamespaceCapability
	failAfterCreate int
	failRemove      int
}

func (n *failingOutputNamespace) createMemberRoot(name string) (rootedDirectory, bool, error) {
	member, created, err := n.outputNamespaceCapability.createMemberRoot(name)
	if err != nil || n.failAfterCreate == 0 {
		return member, created, err
	}
	n.failAfterCreate--
	var closeErr error
	if member != nil {
		closeErr = member.close()
	}
	return nil, created, errors.Join(errors.New("injected target creation failure"), closeErr)
}

func (n *failingOutputNamespace) removeMember(name string) error {
	if n.failRemove != 0 {
		n.failRemove--
		return errors.New("injected member cleanup failure")
	}
	return n.outputNamespaceCapability.removeMember(name)
}

func prepareDynamicTreeEnvironment(t *testing.T) *Environment {
	t.Helper()
	trees, err := value.List(value.Tree())
	if err != nil {
		t.Fatal(err)
	}
	field, err := value.Required("trees", trees)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := value.NewContract(field)
	if err != nil {
		t.Fatal(err)
	}
	graph := workflow.GraphDraft{
		Inputs:  value.EmptyContract(),
		Outputs: outputs,
		Nodes: []workflow.NodeDraft{{
			Name: "leaf",
			Leaf: &workflow.LeafDraft{Kind: workflow.Script, Inputs: value.EmptyContract(), Outputs: outputs},
		}},
		Edges: []workflow.EdgeDraft{{
			From:     workflow.EndpointDraft{Kind: workflow.Child, Child: "leaf"},
			To:       workflow.EndpointDraft{Kind: workflow.Boundary},
			Bindings: []workflow.BindingDraft{{From: []string{"trees"}, To: "trees"}},
		}},
	}
	definition, err := workflow.Compile(workflow.ProgramDraft{
		Root:    "root",
		Modules: []workflow.ModuleDraft{{Name: "root", Graph: graph}},
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, ok := definition.Root().Nodes()[0].Leaf()
	if !ok {
		t.Fatal("compiled node is not a leaf")
	}
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	input, err := value.NewObject()
	if err != nil {
		t.Fatal(err)
	}
	environment, err := Prepare(context.Background(), repository, leaf, input)
	if err != nil {
		t.Fatal(err)
	}
	return environment
}
