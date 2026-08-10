package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
	"github.com/valbaudo/dawn/workspace"
)

func TestCandidateFailuresReturnOnlyZeroValue(t *testing.T) {
	t.Run("after first file blob", func(t *testing.T) {
		ctx := context.Background()
		repository, _ := content.NewRepository(content.NewMemory())
		fileType, _ := value.File("text/plain")
		contract := mustContract(t, mustField(t, "report", fileType))
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		target, err := environment.Output(value.Path{}.Field("report"))
		if err != nil {
			t.Fatal(err)
		}
		mustWriteOutput(t, target, "stored before failure")
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			if _, err := outputs.Value(ctx, value.Path{}.Field("report")); err != nil {
				return value.Value{}, err
			}
			return value.NewString("must not escape"), errors.New("injected after first blob")
		})
		assertZeroCaptureError(t, candidate, err)
	})

	t.Run("during second tree manifest", func(t *testing.T) {
		ctx := context.Background()
		store := &failPutStore{Memory: content.NewMemory(), failAt: 2}
		repository, _ := content.NewRepository(store)
		contract := mustContract(t, mustField(t, "first", value.Tree()), mustField(t, "second", value.Tree()))
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		if _, err := environment.Output(value.Path{}.Field("first")); err != nil {
			t.Fatal(err)
		}
		if _, err := environment.Output(value.Path{}.Field("second")); err != nil {
			t.Fatal(err)
		}
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			first, err := outputs.Value(ctx, value.Path{}.Field("first"))
			if err != nil {
				return value.Value{}, err
			}
			second, err := outputs.Value(ctx, value.Path{}.Field("second"))
			if err != nil {
				return value.Value{}, err
			}
			return mustObject(t, mustEntry(t, "first", first), mustEntry(t, "second", second)), nil
		})
		assertZeroCaptureError(t, candidate, err)
	})

	t.Run("during workspace manifest", func(t *testing.T) {
		ctx := context.Background()
		store := &failPutStore{Memory: content.NewMemory(), failAt: 2}
		repository, _ := content.NewRepository(store)
		contract := mustContract(t, mustField(t, "continued", value.Tree()))
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, []string{"continued"})
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		if err := os.WriteFile(filepath.Join(environment.Workspace(), "file"), []byte("workspace bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			continued, err := outputs.Workspace(ctx)
			if err != nil {
				return value.Value{}, err
			}
			return mustObject(t, mustEntry(t, "continued", continued)), nil
		})
		assertZeroCaptureError(t, candidate, err)
	})

	t.Run("candidate assembly", func(t *testing.T) {
		ctx := context.Background()
		repository, _ := content.NewRepository(content.NewMemory())
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), value.EmptyContract(), nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		candidate, err := environment.Capture(ctx, func(workspace.Outputs) (value.Value, error) {
			return value.NewString("partial"), errors.New("injected assembly failure")
		})
		assertZeroCaptureError(t, candidate, err)
	})

	t.Run("final contract validation", func(t *testing.T) {
		ctx := context.Background()
		repository, _ := content.NewRepository(content.NewMemory())
		fileType, _ := value.File("text/plain")
		contract := mustContract(t, mustField(t, "report", fileType))
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		target, err := environment.Output(value.Path{}.Field("report"))
		if err != nil {
			t.Fatal(err)
		}
		mustWriteOutput(t, target, "complete")
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			if _, err := outputs.Value(ctx, value.Path{}.Field("report")); err != nil {
				return value.Value{}, err
			}
			return mustObject(t), nil
		})
		assertZeroCaptureError(t, candidate, err)
	})
}

func TestCandidateRejectsCapturedLeavesMovedOmittedOrForged(t *testing.T) {
	ctx := context.Background()
	fileType, err := value.File("text/plain")
	if err != nil {
		t.Fatal(err)
	}
	optional, err := value.Optional("optional", fileType)
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t, mustField(t, "first", fileType), mustField(t, "second", fileType), optional)

	for _, test := range []struct {
		name  string
		build func(*testing.T, workspace.Outputs, value.Value, value.Value) (value.Value, error)
	}{
		{"swapped", func(t *testing.T, _ workspace.Outputs, first, second value.Value) (value.Value, error) {
			return mustObject(t, mustEntry(t, "first", second), mustEntry(t, "second", first)), nil
		}},
		{"requested optional omitted", func(t *testing.T, outputs workspace.Outputs, first, second value.Value) (value.Value, error) {
			if _, err := outputs.Value(ctx, value.Path{}.Field("optional")); err != nil {
				return value.Value{}, err
			}
			return mustObject(t, mustEntry(t, "first", first), mustEntry(t, "second", second)), nil
		}},
		{"forged uncaptured", func(t *testing.T, _ workspace.Outputs, first, second value.Value) (value.Value, error) {
			forgedFile, err := content.NewFile(content.Digest{1}, 0, "value", "text/plain")
			if err != nil {
				return value.Value{}, err
			}
			return mustObject(t, mustEntry(t, "first", first), mustEntry(t, "second", second), mustEntry(t, "optional", value.NewFileValue(forgedFile))), nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, _ := content.NewRepository(content.NewMemory())
			leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
			environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = environment.Close() })
			for _, name := range []string{"first", "second", "optional"} {
				target, err := environment.Output(value.Path{}.Field(name))
				if err != nil {
					t.Fatal(err)
				}
				mustWriteOutput(t, target, name)
			}
			candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
				first, err := outputs.Value(ctx, value.Path{}.Field("first"))
				if err != nil {
					return value.Value{}, err
				}
				second, err := outputs.Value(ctx, value.Path{}.Field("second"))
				if err != nil {
					return value.Value{}, err
				}
				return test.build(t, outputs, first, second)
			})
			assertZeroCaptureError(t, candidate, err)
		})
	}
}

func TestAtomicCaptureRejectsFileChangedAfterInspection(t *testing.T) {
	ctx := context.Background()
	store := &mutatingPutStore{Memory: content.NewMemory()}
	repository, err := content.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	fileType, err := value.File("text/plain")
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t, mustField(t, "report", fileType))
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
	environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	target, err := environment.Output(value.Path{}.Field("report"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteOutput(t, target, string(bytes.Repeat([]byte("a"), 600)))
	store.target = target.Location()
	store.replacement = append(bytes.Repeat([]byte("a"), 512), bytes.Repeat([]byte("b"), 288)...)

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		report, err := outputs.Value(ctx, value.Path{}.Field("report"))
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "report", report)), nil
	})
	assertZeroCaptureError(t, candidate, err)
}

type failPutStore struct {
	*content.Memory
	mu     sync.Mutex
	puts   int
	failAt int
}

type mutatingPutStore struct {
	*content.Memory
	target      string
	replacement []byte
	once        sync.Once
	err         error
}

func (s *mutatingPutStore) Put(ctx context.Context, source io.Reader) (content.Object, error) {
	s.once.Do(func() {
		s.err = os.WriteFile(s.target, s.replacement, 0o600)
	})
	if s.err != nil {
		return content.Object{}, s.err
	}
	return s.Memory.Put(ctx, source)
}

func (s *mutatingPutStore) Copy(ctx context.Context, digest content.Digest, destination io.Writer) (int64, error) {
	return s.Memory.Copy(ctx, digest, destination)
}

func (s *failPutStore) Put(ctx context.Context, source io.Reader) (content.Object, error) {
	s.mu.Lock()
	s.puts++
	put := s.puts
	s.mu.Unlock()
	if put == s.failAt {
		_, _ = io.Copy(io.Discard, source)
		return content.Object{}, errors.New("injected store failure")
	}
	return s.Memory.Put(ctx, source)
}

func (s *failPutStore) Copy(ctx context.Context, digest content.Digest, destination io.Writer) (int64, error) {
	return s.Memory.Copy(ctx, digest, destination)
}
