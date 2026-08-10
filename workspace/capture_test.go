package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
	"github.com/valbaudo/dawn/workspace"
)

func TestCaptureExactSlotsAndStructuredConcretePaths(t *testing.T) {
	ctx := context.Background()
	store := content.NewMemory()
	repository, err := content.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	textFile, err := value.File("text/plain")
	if err != nil {
		t.Fatal(err)
	}
	nested := mustTypeObject(t, mustField(t, "document", textFile))
	files, err := value.List(textFile)
	if err != nil {
		t.Fatal(err)
	}
	trees, err := value.Map(value.Tree())
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t,
		mustField(t, "empty", value.Tree()),
		mustField(t, "files", files),
		mustField(t, "nested", nested),
		mustField(t, "report", textFile),
		mustField(t, "trees", trees),
	)
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
	environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })

	reportPath := value.Path{}.Field("report")
	listPath := value.Path{}.Field("files").ListIndex(0)
	nestedPath := value.Path{}.Field("nested").Field("document")
	treePath := value.Path{}.Field("trees").MapKey("raw/../key")
	for _, item := range []struct {
		path value.Path
		data string
	}{
		{reportPath, "complete report\n"},
		{listPath, "list member\n"},
		{nestedPath, "nested member\n"},
	} {
		target, outputErr := environment.Output(item.path)
		if outputErr != nil {
			t.Fatal(outputErr)
		}
		if writeErr := os.WriteFile(target.Location(), []byte(item.data), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if _, err := environment.Output(value.Path{}.Field("empty")); err != nil {
		t.Fatal(err)
	}
	treeTarget, err := environment.Output(treePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(treeTarget.Location(), "member.txt"), []byte("tree member"), 0o600); err != nil {
		t.Fatal(err)
	}

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		report, err := outputs.Value(ctx, reportPath)
		if err != nil {
			return value.Value{}, err
		}
		again, err := outputs.Value(ctx, reportPath)
		if err != nil {
			return value.Value{}, err
		}
		if !report.Equal(again) {
			return value.Value{}, errors.New("repeated capture was not memoized")
		}
		listed, err := outputs.Value(ctx, listPath)
		if err != nil {
			return value.Value{}, err
		}
		document, err := outputs.Value(ctx, nestedPath)
		if err != nil {
			return value.Value{}, err
		}
		empty, err := outputs.Value(ctx, value.Path{}.Field("empty"))
		if err != nil {
			return value.Value{}, err
		}
		tree, err := outputs.Value(ctx, treePath)
		if err != nil {
			return value.Value{}, err
		}
		mapped, err := value.NewMap(mustEntry(t, "raw/../key", tree))
		if err != nil {
			return value.Value{}, err
		}
		return value.NewObject(
			mustEntry(t, "empty", empty),
			mustEntry(t, "files", value.NewList(listed)),
			mustEntry(t, "nested", mustObject(t, mustEntry(t, "document", document))),
			mustEntry(t, "report", report),
			mustEntry(t, "trees", mapped),
		)
	})
	if err != nil {
		t.Fatal(err)
	}

	reportValue := objectMember(t, candidate, "report")
	report, ok := reportValue.File()
	if !ok {
		t.Fatalf("report kind = %v, want file", reportValue.Kind())
	}
	if report.Name() != "value" || report.Media() != "text/plain; charset=utf-8" || report.Size() != int64(len("complete report\n")) {
		t.Fatalf("report facts = (%q, %q, %d), want (value, text/plain; charset=utf-8, %d)", report.Name(), report.Media(), report.Size(), len("complete report\n"))
	}
	var copied bytes.Buffer
	if err := repository.CopyFile(ctx, report, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.String() != "complete report\n" {
		t.Fatalf("report bytes = %q", copied.String())
	}
	empty, ok := objectMember(t, candidate, "empty").Tree()
	if !ok {
		t.Fatal("empty output is not a tree")
	}
	entries, err := repository.Entries(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty tree entries = %#v, want none", entries)
	}
}

func TestCaptureMemoizesImmutableFileSnapshot(t *testing.T) {
	ctx := context.Background()
	repository, err := content.NewRepository(content.NewMemory())
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
	path := value.Path{}.Field("report")
	target, err := environment.Output(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Location(), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		first, err := outputs.Value(ctx, path)
		if err != nil {
			return value.Value{}, err
		}
		if err := os.WriteFile(target.Location(), []byte("second"), 0o600); err != nil {
			return value.Value{}, err
		}
		second, err := outputs.Value(ctx, path)
		if err != nil {
			return value.Value{}, err
		}
		if !first.Equal(second) {
			return value.Value{}, errors.New("memoized file changed after source mutation")
		}
		return mustObject(t, mustEntry(t, "report", second)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	file, _ := objectMember(t, candidate, "report").File()
	var copied bytes.Buffer
	if err := repository.CopyFile(ctx, file, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.String() != "first" {
		t.Fatalf("memoized bytes = %q, want first", copied.String())
	}
}

func TestCaptureRejectsInvalidSlotsContractsAndPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("special-entry coverage requires Unix")
	}
	tests := []struct {
		name      string
		fileMedia string
		setup     func(*testing.T, workspace.Target)
	}{
		{"missing file", "text/plain", func(*testing.T, workspace.Target) {}},
		{"extra file", "text/plain", func(t *testing.T, target workspace.Target) {
			mustWriteOutput(t, target, "plain text")
			if err := os.WriteFile(filepath.Join(filepath.Dir(target.Location()), "extra"), []byte("extra"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong directory kind", "text/plain", func(t *testing.T, target workspace.Target) {
			if err := os.Mkdir(target.Location(), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", "text/plain", func(t *testing.T, target workspace.Target) {
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target.Location()); err != nil {
				t.Fatal(err)
			}
		}},
		{"special entry", "text/plain", func(t *testing.T, target workspace.Target) {
			if err := syscall.Mkfifo(target.Location(), 0o600); err != nil {
				t.Skipf("FIFO creation unavailable: %v", err)
			}
		}},
		{"media mismatch", "application/pdf", func(t *testing.T, target workspace.Target) {
			mustWriteOutput(t, target, "plain text")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repository, err := content.NewRepository(content.NewMemory())
			if err != nil {
				t.Fatal(err)
			}
			fileType, err := value.File(test.fileMedia)
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
			test.setup(t, target)
			candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
				captured, err := outputs.Value(ctx, value.Path{}.Field("report"))
				if err != nil {
					return value.Value{}, err
				}
				return mustObject(t, mustEntry(t, "report", captured)), nil
			})
			assertZeroCaptureError(t, candidate, err)
		})
	}

	t.Run("escaping tree symlink", func(t *testing.T) {
		ctx := context.Background()
		repository, _ := content.NewRepository(content.NewMemory())
		contract := mustContract(t, mustField(t, "tree", value.Tree()))
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		target, err := environment.Output(value.Path{}.Field("tree"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../outside", filepath.Join(target.Location(), "escape")); err != nil {
			t.Fatal(err)
		}
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			tree, err := outputs.Value(ctx, value.Path{}.Field("tree"))
			if err != nil {
				return value.Value{}, err
			}
			return mustObject(t, mustEntry(t, "tree", tree)), nil
		})
		assertZeroCaptureError(t, candidate, err)
	})

	t.Run("undeclared output root entry", func(t *testing.T) {
		ctx := context.Background()
		repository, _ := content.NewRepository(content.NewMemory())
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), value.EmptyContract(), nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		outputRoot := filepath.Join(filepath.Dir(environment.Workspace()), "outputs")
		if err := os.WriteFile(filepath.Join(outputRoot, "undeclared"), []byte("bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate, err := environment.Capture(ctx, func(workspace.Outputs) (value.Value, error) {
			return mustObject(t), nil
		})
		assertZeroCaptureError(t, candidate, err)
	})

	t.Run("arbitrary semantic path", func(t *testing.T) {
		ctx := context.Background()
		repository, _ := content.NewRepository(content.NewMemory())
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), value.EmptyContract(), nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			_, err := outputs.Value(ctx, value.Path{}.Field("/tmp/host-file"))
			return value.Value{}, err
		})
		assertZeroCaptureError(t, candidate, err)
	})
}

func TestCapturePublishesWorkspaceOnlyWhenDeclaredAndRequested(t *testing.T) {
	ctx := context.Background()
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("not declared", func(t *testing.T) {
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), value.EmptyContract(), nil, nil)
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		if err := os.WriteFile(filepath.Join(environment.Workspace(), "private.txt"), []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			_, _ = outputs.Workspace(ctx)
			return mustObject(t), nil
		})
		assertZeroCaptureError(t, candidate, err)
		candidate, err = environment.Capture(ctx, func(workspace.Outputs) (value.Value, error) {
			return mustObject(t), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(candidate.Entries()) != 0 {
			t.Fatalf("private workspace leaked into candidate: %#v", candidate.Entries())
		}
	})

	t.Run("declared and requested", func(t *testing.T) {
		contract := mustContract(t, mustField(t, "continued", value.Tree()))
		leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, []string{"continued"})
		environment, err := workspace.Prepare(ctx, repository, leaf, mustObject(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = environment.Close() })
		if err := os.WriteFile(filepath.Join(environment.Workspace(), "private.txt"), []byte("published"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
			continued, err := outputs.Workspace(ctx)
			if err != nil {
				return value.Value{}, err
			}
			return mustObject(t, mustEntry(t, "continued", continued)), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tree, ok := objectMember(t, candidate, "continued").Tree()
		if !ok {
			t.Fatal("published workspace is not a tree")
		}
		entries, err := repository.Entries(ctx, tree)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Segments()[0] != "private.txt" {
			t.Fatalf("published workspace entries = %#v", entries)
		}
	})
}

func TestCaptureTreeOutputUsesPinnedRootAcrossPathSwapAndRestore(t *testing.T) {
	base := context.Background()
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t, mustField(t, "result", value.Tree()))
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, nil)
	environment, err := workspace.Prepare(base, repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	target, err := environment.Output(value.Path{}.Field("result"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.Location(), "original.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Dir(environment.Workspace())
	decoy := filepath.Join(runtimeRoot, "tree-decoy")
	saved := filepath.Join(runtimeRoot, "tree-saved")
	if err := os.Mkdir(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "decoy.txt"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := newSwapRestoreContext(base, target.Location(), saved, decoy)

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		result, err := outputs.Value(ctx, value.Path{}.Field("result"))
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "result", result)), nil
	})
	if actionErr := ctx.ActionError(); actionErr != nil {
		t.Fatalf("swap/restore fixture: %v", actionErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	tree, _ := objectMember(t, candidate, "result").Tree()
	assertSingleTreeEntry(t, repository, tree, "original.txt")
}

func TestCaptureWorkspaceUsesPinnedRootAcrossPathSwapAndRestore(t *testing.T) {
	base := context.Background()
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	contract := mustContract(t, mustField(t, "continued", value.Tree()))
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), contract, nil, []string{"continued"})
	environment, err := workspace.Prepare(base, repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	if err := os.WriteFile(filepath.Join(environment.Workspace(), "original.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Dir(environment.Workspace())
	decoy := filepath.Join(runtimeRoot, "workspace-decoy")
	saved := filepath.Join(runtimeRoot, "workspace-saved")
	if err := os.Mkdir(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "decoy.txt"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := newSwapRestoreContext(base, environment.Workspace(), saved, decoy)

	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		continued, err := outputs.Workspace(ctx)
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "continued", continued)), nil
	})
	if actionErr := ctx.ActionError(); actionErr != nil {
		t.Fatalf("swap/restore fixture: %v", actionErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	tree, _ := objectMember(t, candidate, "continued").Tree()
	assertSingleTreeEntry(t, repository, tree, "original.txt")
}

type swapRestoreContext struct {
	context.Context
	mu        sync.Mutex
	calls     int
	path      string
	saved     string
	decoy     string
	actionErr error
}

func newSwapRestoreContext(base context.Context, path, saved, decoy string) *swapRestoreContext {
	return &swapRestoreContext{Context: base, path: path, saved: saved, decoy: decoy}
}

func (c *swapRestoreContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	switch c.calls {
	case 3:
		if err := os.Rename(c.path, c.saved); err != nil {
			c.actionErr = errors.Join(c.actionErr, err)
			break
		}
		if err := os.Rename(c.decoy, c.path); err != nil {
			c.actionErr = errors.Join(c.actionErr, err, os.Rename(c.saved, c.path))
		}
	case 4:
		if err := os.Rename(c.path, c.decoy); err != nil {
			c.actionErr = errors.Join(c.actionErr, err)
			break
		}
		if err := os.Rename(c.saved, c.path); err != nil {
			c.actionErr = errors.Join(c.actionErr, err, os.Rename(c.decoy, c.path))
		}
	}
	return c.Context.Err()
}

func (c *swapRestoreContext) ActionError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.actionErr
}

func assertSingleTreeEntry(t *testing.T, repository *content.Repository, tree content.Tree, name string) {
	t.Helper()
	entries, err := repository.Entries(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Segments()) != 1 || entries[0].Segments()[0] != name {
		t.Fatalf("captured tree entries = %#v, want one %q", entries, name)
	}
}

func objectMember(t *testing.T, object value.Value, name string) value.Value {
	t.Helper()
	for _, entry := range object.Entries() {
		if entry.Name() == name {
			return entry.Value()
		}
	}
	t.Fatalf("object has no member %q", name)
	return value.Value{}
}

func mustWriteOutput(t *testing.T, target workspace.Target, data string) {
	t.Helper()
	if err := os.WriteFile(target.Location(), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertZeroCaptureError(t *testing.T, candidate value.Value, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Capture error = nil, want rejection")
	}
	if candidate.Kind() != value.InvalidKind || candidate.Valid() {
		t.Fatalf("Capture candidate = kind %v valid %v, want zero value", candidate.Kind(), candidate.Valid())
	}
}
