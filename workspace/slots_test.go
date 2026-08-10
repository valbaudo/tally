package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
	"github.com/valbaudo/dawn/workspace"
)

func TestSlotsCreateStaticAndConfinedDynamicOutputTargets(t *testing.T) {
	fileType, err := value.File("application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	listFiles, err := value.List(fileType)
	if err != nil {
		t.Fatal(err)
	}
	mapTrees, err := value.Map(value.Tree())
	if err != nil {
		t.Fatal(err)
	}
	bundle := mustTypeObject(t, mustField(t, "dir", value.Tree()), mustField(t, "doc", fileType))
	bundles, err := value.List(bundle)
	if err != nil {
		t.Fatal(err)
	}
	outputs := mustContract(t,
		mustField(t, "bundles", bundles),
		mustField(t, "fixedFile", fileType),
		mustField(t, "fixedTree", value.Tree()),
		mustField(t, "listFiles", listFiles),
		mustField(t, "mapTrees", mapTrees),
		mustField(t, "published", value.Tree()),
		mustField(t, "text", value.String()),
	)
	leaf := compileLeaf(t, workflow.Agent, value.EmptyContract(), outputs, nil, []string{"published"})
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	environment, err := workspace.Prepare(context.Background(), repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })

	records := environment.Manifest().Outputs()
	if got, want := len(records), 6; got != want {
		t.Fatalf("output records = %d, want %d static slots and dynamic namespaces", got, want)
	}
	wantSchemas := []string{
		`$.field("bundles")[*].field("dir")`,
		`$.field("bundles")[*].field("doc")`,
		`$.field("fixedFile")`,
		`$.field("fixedTree")`,
		`$.field("listFiles")[*]`,
		`$.field("mapTrees").key(*)`,
	}
	gotSchemas := make([]string, len(records))
	for index, record := range records {
		gotSchemas[index] = record.SchemaPath()
		if record.Kind() != value.FileKind && record.Kind() != value.TreeKind {
			t.Fatalf("output record kind = %v", record.Kind())
		}
		if pathWithin(record.Location(), environment.Workspace()) {
			t.Fatalf("output %q is inside workspace", record.Location())
		}
		if record.Dynamic() {
			if !regexp.MustCompile(`^outputs/[0-9a-f]{64}$`).MatchString(filepath.ToSlash(record.RelativeLocation())) {
				t.Fatalf("dynamic namespace = %q", record.RelativeLocation())
			}
			entries, err := os.ReadDir(record.Location())
			if err != nil || len(entries) != 0 {
				t.Fatalf("dynamic namespace starts as (%v, %v), want empty", entries, err)
			}
		} else if !regexp.MustCompile(`^outputs/[0-9a-f]{64}/value$`).MatchString(filepath.ToSlash(record.RelativeLocation())) {
			t.Fatalf("static slot = %q", record.RelativeLocation())
		}
	}
	if !reflect.DeepEqual(gotSchemas, wantSchemas) {
		t.Fatalf("output schema order = %q, want %q", gotSchemas, wantSchemas)
	}
	if records[2].ValuePath().String() != `$.field("fixedFile")` {
		t.Fatalf("static output value path = %s", records[2].ValuePath().String())
	}
	for _, forbidden := range []string{`$.field("published")`, `$.field("text")`} {
		for _, schema := range gotSchemas {
			if schema == forbidden {
				t.Fatalf("manifest created named output slot for %s", forbidden)
			}
		}
	}

	staticFile, err := environment.Output(value.Path{}.Field("fixedFile"))
	if err != nil {
		t.Fatal(err)
	}
	if staticFile.Kind() != value.FileKind {
		t.Fatalf("fixed file kind = %v", staticFile.Kind())
	}
	if _, err := os.Lstat(staticFile.Location()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixed file target status = %v, want absent writable filename", err)
	}
	if _, err := os.Stat(filepath.Dir(staticFile.Location())); err != nil {
		t.Fatal(err)
	}
	staticTree, err := environment.Output(value.Path{}.Field("fixedTree"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(staticTree.Location()); err != nil || !info.IsDir() {
		t.Fatalf("fixed tree target = (%v, %v), want directory", info, err)
	}

	fileMemberPath := value.Path{}.Field("listFiles").ListIndex(7)
	fileMember, err := environment.Output(fileMemberPath)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^outputs/[0-9a-f]{64}/[0-9a-f]{64}/value$`).MatchString(filepath.ToSlash(fileMember.RelativeLocation())) {
		t.Fatalf("dynamic file member = %q", fileMember.RelativeLocation())
	}
	if fileMember.ValuePath().String() != fileMemberPath.String() {
		t.Fatalf("dynamic target path = %s, want %s", fileMember.ValuePath().String(), fileMemberPath.String())
	}
	again, err := environment.Output(fileMemberPath)
	if err != nil || again.Location() != fileMember.Location() {
		t.Fatalf("repeat output resolution = (%q, %v), want %q", again.Location(), err, fileMember.Location())
	}

	keys := []string{"", "../x", "a/b", "a\\b", string([]byte{0xff})}
	locations := map[string]bool{}
	for _, key := range keys {
		target, err := environment.Output(value.Path{}.Field("mapTrees").MapKey(key))
		if err != nil {
			t.Fatalf("adversarial key %q: %v", key, err)
		}
		if target.Kind() != value.TreeKind {
			t.Fatalf("map target kind = %v", target.Kind())
		}
		if info, err := os.Stat(target.Location()); err != nil || !info.IsDir() {
			t.Fatalf("dynamic tree target = (%v, %v)", info, err)
		}
		if locations[target.Location()] {
			t.Fatalf("dynamic key %q aliases an earlier slot", key)
		}
		locations[target.Location()] = true
	}

	for _, path := range []value.Path{
		value.Path{}.Field("published"),
		value.Path{}.Field("text"),
		value.Path{}.Field("missing"),
		value.Path{}.Field("listFiles").ListIndex(-1),
		value.Path{}.Field("listFiles").MapKey("0"),
		value.Path{}.Field("fixedFile").Field("child"),
		value.Path{}.Field("/tmp/host-output"),
	} {
		if target, err := environment.Output(path); err == nil {
			t.Fatalf("Output(%s) = %q, want rejection", path.String(), target.Location())
		}
	}

	records[0] = workspace.Output{}
	if environment.Manifest().Outputs()[0].SchemaPath() != wantSchemas[0] {
		t.Fatal("manifest output slice was not defensive")
	}
}

func TestManifestSlotLayoutIsDeterministicAcrossFreshEnvironments(t *testing.T) {
	fileType, err := value.File()
	if err != nil {
		t.Fatal(err)
	}
	listFiles, err := value.List(fileType)
	if err != nil {
		t.Fatal(err)
	}
	inputs := mustContract(t, mustField(t, "input", fileType))
	outputs := mustContract(t, mustField(t, "files", listFiles), mustField(t, "result", value.Tree()))
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	file := ingestFile(t, repository, "same-logical-name", "text/plain", "same bytes")
	leaf := compileLeaf(t, workflow.Script, inputs, outputs, nil, nil)
	input := mustObject(t, mustEntry(t, "input", value.NewFileValue(file)))

	first, err := workspace.Prepare(context.Background(), repository, leaf, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := workspace.Prepare(context.Background(), repository, leaf, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if first.Workspace() == second.Workspace() {
		t.Fatal("fresh preparations shared a workspace")
	}

	if got, want := relativeInputs(first.Manifest()), relativeInputs(second.Manifest()); !reflect.DeepEqual(got, want) {
		t.Fatalf("input layouts differ: %q != %q", got, want)
	}
	if got, want := relativeOutputs(first.Manifest()), relativeOutputs(second.Manifest()); !reflect.DeepEqual(got, want) {
		t.Fatalf("output layouts differ: %q != %q", got, want)
	}
	firstTarget, err := first.Output(value.Path{}.Field("files").ListIndex(3))
	if err != nil {
		t.Fatal(err)
	}
	secondTarget, err := second.Output(value.Path{}.Field("files").ListIndex(3))
	if err != nil {
		t.Fatal(err)
	}
	if firstTarget.RelativeLocation() != secondTarget.RelativeLocation() {
		t.Fatalf("dynamic member layouts differ: %q != %q", firstTarget.RelativeLocation(), secondTarget.RelativeLocation())
	}
}

func TestSlotsRejectReplacedDynamicNamespaceWithoutCreatingOutsideRoot(t *testing.T) {
	fileType, err := value.File()
	if err != nil {
		t.Fatal(err)
	}
	files, err := value.List(fileType)
	if err != nil {
		t.Fatal(err)
	}
	outputs := mustContract(t, mustField(t, "files", files))
	leaf := compileLeaf(t, workflow.Script, value.EmptyContract(), outputs, nil, nil)
	repository, err := content.NewRepository(content.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	environment, err := workspace.Prepare(context.Background(), repository, leaf, mustObject(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	namespace := environment.Manifest().Outputs()[0].Location()
	displaced := namespace + ".displaced"
	outside := t.TempDir()
	if err := os.Rename(namespace, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, namespace); err != nil {
		t.Skipf("symlink replacement is unavailable: %v", err)
	}

	if target, err := environment.Output(value.Path{}.Field("files").ListIndex(0)); err == nil {
		t.Fatalf("Output accepted a replaced namespace and returned %q", target.Location())
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside directory after rejected allocation = (%v, %v), want empty", entries, err)
	}
	if entries, err := os.ReadDir(displaced); err != nil || len(entries) != 0 {
		t.Fatalf("pinned namespace after rejected replacement = (%v, %v), want empty", entries, err)
	}
}

func relativeInputs(manifest workspace.Manifest) []string {
	records := manifest.Inputs()
	result := make([]string, len(records))
	for index, record := range records {
		result[index] = record.ValuePath().String() + "=" + record.RelativeLocation()
	}
	return result
}

func relativeOutputs(manifest workspace.Manifest) []string {
	records := manifest.Outputs()
	result := make([]string, len(records))
	for index, record := range records {
		result[index] = record.SchemaPath() + "=" + record.RelativeLocation()
	}
	return result
}
