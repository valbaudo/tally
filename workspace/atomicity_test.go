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
			if _, err := outputs.File(ctx, value.Path{}.Field("report"), "report.txt", "text/plain"); err != nil {
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
			first, err := outputs.Tree(ctx, value.Path{}.Field("first"))
			if err != nil {
				return value.Value{}, err
			}
			second, err := outputs.Tree(ctx, value.Path{}.Field("second"))
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
			if _, err := outputs.File(ctx, value.Path{}.Field("report"), "report.txt", "text/plain"); err != nil {
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
			if _, err := outputs.File(ctx, value.Path{}.Field("optional"), "optional.txt", "text/plain"); err != nil {
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
				first, err := outputs.File(ctx, value.Path{}.Field("first"), "first.txt", "text/plain")
				if err != nil {
					return value.Value{}, err
				}
				second, err := outputs.File(ctx, value.Path{}.Field("second"), "second.txt", "text/plain")
				if err != nil {
					return value.Value{}, err
				}
				return test.build(t, outputs, first, second)
			})
			assertZeroCaptureError(t, candidate, err)
		})
	}
}

func TestCandidateRejectsEqualContentCapturesSwappedBetweenPaths(t *testing.T) {
	ctx := context.Background()
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	fileType, err := value.File("text/plain")
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t, mustField(t, "first", fileType), mustField(t, "second", fileType))
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
	environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	for _, name := range []string{"first", "second"} {
		target, err := environment.Output(value.Path{}.Field(name))
		if err != nil {
			t.Fatal(err)
		}
		mustWriteOutput(t, target, "identical bytes")
	}

	semanticallyEqual := false
	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		first, err := outputs.File(ctx, value.Path{}.Field("first"), "equal.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		second, err := outputs.File(ctx, value.Path{}.Field("second"), "equal.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		semanticallyEqual = first.Equal(second)
		return mustObject(t, mustEntry(t, "first", second), mustEntry(t, "second", first)), nil
	})
	if !semanticallyEqual {
		t.Fatal("equal-content captures were not semantically equal")
	}
	assertZeroCaptureError(t, candidate, err)
}

func TestCandidateRejectsReconstructedEqualContentHandles(t *testing.T) {
	t.Run("file", func(t *testing.T) {
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
		mustWriteOutput(t, target, "same semantic file")

		semanticallyEqual := false
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			captured, err := outputs.File(ctx, value.Path{}.Field("report"), "report.txt", "text/plain")
			if err != nil {
				return value.Value{}, err
			}
			file, _ := captured.File()
			reconstructed := value.NewFileValue(file)
			semanticallyEqual = captured.Equal(reconstructed)
			return mustObject(t, mustEntry(t, "report", reconstructed)), nil
		})
		if !semanticallyEqual {
			t.Fatal("reconstructed file was not semantically equal")
		}
		assertZeroCaptureError(t, candidate, err)
	})

	t.Run("tree", func(t *testing.T) {
		ctx := context.Background()
		repository, _ := content.NewRepository(content.NewMemory())
		contract := mustContract(t, mustField(t, "result", value.Tree()))
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		if _, err := environment.Output(value.Path{}.Field("result")); err != nil {
			t.Fatal(err)
		}

		semanticallyEqual := false
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			captured, err := outputs.Tree(ctx, value.Path{}.Field("result"))
			if err != nil {
				return value.Value{}, err
			}
			tree, _ := captured.Tree()
			reconstructed := value.NewTreeValue(tree)
			semanticallyEqual = captured.Equal(reconstructed)
			return mustObject(t, mustEntry(t, "result", reconstructed)), nil
		})
		if !semanticallyEqual {
			t.Fatal("reconstructed tree was not semantically equal")
		}
		assertZeroCaptureError(t, candidate, err)
	})
}

func TestCandidateRejectsEqualHandleFromPriorCaptureSession(t *testing.T) {
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
	path := value.Path{}.Field("report")
	target, err := environment.Output(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteOutput(t, target, "unchanged between sessions")
	prior, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		report, err := outputs.File(ctx, path, "report.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "report", report)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	priorReport := objectMember(t, prior, "report")

	semanticallyEqual := false
	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		current, err := outputs.File(ctx, path, "report.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		semanticallyEqual = current.Equal(priorReport)
		return mustObject(t, mustEntry(t, "report", priorReport)), nil
	})
	if !semanticallyEqual {
		t.Fatal("prior-session handle was not semantically equal")
	}
	assertZeroCaptureError(t, candidate, err)
}

func TestAtomicCaptureRejectsSameSizeFileMutationDuringIngest(t *testing.T) {
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
	store.replacement = append(bytes.Repeat([]byte("c"), 512), bytes.Repeat([]byte("b"), 88)...)

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		report, err := outputs.File(ctx, value.Path{}.Field("report"), "report.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "report", report)), nil
	})
	assertZeroCaptureError(t, candidate, err)
}

func TestAtomicCaptureRejectsSameSizeTreeMutationDuringFirstRead(t *testing.T) {
	ctx := context.Background()
	store := &mutatingPutStore{Memory: content.NewMemory()}
	repository, err := content.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t, mustField(t, "result", value.Tree()))
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
	environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	target, err := environment.Output(value.Path{}.Field("result"))
	if err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(target.Location(), "member.bin")
	if err := os.WriteFile(member, bytes.Repeat([]byte("a"), 600), 0o600); err != nil {
		t.Fatal(err)
	}
	store.target = member
	store.replacement = append(bytes.Repeat([]byte("c"), 512), bytes.Repeat([]byte("b"), 88)...)

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		result, err := outputs.Tree(ctx, value.Path{}.Field("result"))
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "result", result)), nil
	})
	assertZeroCaptureError(t, candidate, err)
}

func TestAtomicCaptureRejectsSameSizeWorkspaceMutationDuringFirstRead(t *testing.T) {
	ctx := context.Background()
	store := &mutatingPutStore{Memory: content.NewMemory()}
	repository, err := content.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t, mustField(t, "continued", value.Tree()))
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, []string{"continued"})
	environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	member := filepath.Join(environment.Workspace(), "member.bin")
	if err := os.WriteFile(member, bytes.Repeat([]byte("a"), 600), 0o600); err != nil {
		t.Fatal(err)
	}
	store.target = member
	store.replacement = append(bytes.Repeat([]byte("c"), 512), bytes.Repeat([]byte("b"), 88)...)

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		continued, err := outputs.Workspace(ctx)
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "continued", continued)), nil
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
		prefix := make([]byte, 512)
		read, readErr := io.ReadFull(source, prefix)
		if readErr != nil {
			s.err = readErr
			return
		}
		s.err = os.WriteFile(s.target, s.replacement, 0o600)
		source = io.MultiReader(bytes.NewReader(prefix[:read]), source)
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
