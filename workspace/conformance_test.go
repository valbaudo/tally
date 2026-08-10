package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
	"github.com/valbaudo/dawn/workspace"
)

func TestPrestigeValueFlow(t *testing.T) {
	ctx := context.Background()
	storeRoot := filepath.Join(t.TempDir(), "content")
	filesystemStore, err := content.OpenFS(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := &conformanceFaultStore{Store: filesystemStore}
	repository, err := content.NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	values := value.NewStore(store)

	fixtures := filepath.Join(t.TempDir(), "ingestion")
	sourceRoot := filepath.Join(fixtures, "source")
	validatorRoot := filepath.Join(fixtures, "validators")
	skillRoot := filepath.Join(fixtures, "skills")
	mustMkdirAll(t, filepath.Join(sourceRoot, "src"))
	mustWriteFile(t, filepath.Join(sourceRoot, "README.md"), "committed source\n")
	mustWriteFile(t, filepath.Join(sourceRoot, "decision.txt"), "base\n")
	mustWriteFile(t, filepath.Join(sourceRoot, "src", "service.go"), "package service\n")
	mustMkdirAll(t, filepath.Join(validatorRoot, "policy"))
	mustWriteFile(t, filepath.Join(validatorRoot, "policy", "validate.rego"), "package validation\n")
	mustMkdirAll(t, filepath.Join(skillRoot, "review"))
	mustWriteFile(t, filepath.Join(skillRoot, "review", "SKILL.md"), "# Review\n")

	source := mustCaptureTree(t, repository, sourceRoot)
	validators := mustCaptureTree(t, repository, validatorRoot)
	skills := mustCaptureTree(t, repository, skillRoot)
	pdfBytes := []byte("%PDF-1.7\nPrestige assessment brief\n%%EOF\n")
	brief, err := repository.IngestFile(ctx, "assessment.pdf", "application/pdf", bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}
	if brief.Name() != "assessment.pdf" || brief.Media() != "application/pdf" || brief.Size() != int64(len(pdfBytes)) {
		t.Fatalf("ingested PDF facts = (%q, %q, %d)", brief.Name(), brief.Media(), brief.Size())
	}
	if err := os.RemoveAll(fixtures); err != nil {
		t.Fatal(err)
	}

	pdfType := mustFileType(t, "application/pdf")
	plainFile := mustFileType(t, "text/plain")
	rawInputs := mustContract(t, mustField(t, "brief", pdfType))
	rawLeaf := compileAttachmentLeaf(t, rawInputs, []workflow.AttachmentDraft{{
		Input: []string{"brief"}, Fidelity: workflow.VisualFidelity,
	}})
	rawValue := mustObject(t, mustEntry(t, "brief", value.NewFileValue(brief)))
	if err := rawLeaf.Inputs().Validate(rawValue); err != nil {
		t.Fatal(err)
	}
	attachments := rawLeaf.Attachments()
	if len(attachments) != 1 || attachments[0].Fidelity() != workflow.VisualFidelity ||
		len(attachments[0].Input()) != 1 || attachments[0].Input()[0] != "brief" {
		t.Fatalf("raw attachment declaration = %#v, want the PDF at visual fidelity", attachments)
	}
	if rawEnvironment, err := workspace.Prepare(ctx, repository, rawLeaf, rawValue); err == nil || rawEnvironment != nil {
		t.Fatal("raw LLM unexpectedly received a filesystem environment")
	}

	branchInputs := mustContract(t,
		mustField(t, "source", value.Tree()),
		mustField(t, "brief", pdfType),
		mustField(t, "validators", value.Tree()),
		mustField(t, "skills", value.Tree()),
	)
	branchInput := mustObject(t,
		mustEntry(t, "source", value.NewTreeValue(source)),
		mustEntry(t, "brief", value.NewFileValue(brief)),
		mustEntry(t, "validators", value.NewTreeValue(validators)),
		mustEntry(t, "skills", value.NewTreeValue(skills)),
	)
	staticOutputs := mustContract(t,
		mustField(t, "continued", value.Tree()),
		mustField(t, "static_analysis_json", plainFile),
		mustField(t, "validation_report", plainFile),
	)
	remediationOutputs := mustContract(t,
		mustField(t, "continued", value.Tree()),
		mustField(t, "scored_findings_json", plainFile),
		mustField(t, "remediation_report", plainFile),
	)
	staticLeaf := compileLeaf(t, workflow.Agent, branchInputs, staticOutputs, []string{"source"}, []string{"continued"})
	remediationLeaf := compileLeaf(t, workflow.Script, branchInputs, remediationOutputs, []string{"source"}, []string{"continued"})

	staticEnvironment, err := workspace.Prepare(ctx, repository, staticLeaf, branchInput)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staticEnvironment.Close() })
	remediationEnvironment, err := workspace.Prepare(ctx, repository, remediationLeaf, branchInput)
	if err != nil {
		_ = staticEnvironment.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remediationEnvironment.Close() })
	if staticEnvironment.Workspace() == remediationEnvironment.Workspace() {
		t.Fatal("independent branches share one writable directory")
	}
	staticRuntimeRoot := filepath.Dir(staticEnvironment.Workspace())
	remediationRuntimeRoot := filepath.Dir(remediationEnvironment.Workspace())

	staticPDF := manifestInput(t, staticEnvironment.Manifest(), `$.field("brief")`)
	if staticPDF.Workspace() {
		t.Fatal("the agent PDF was selected as a writable base")
	}
	if got := mustReadFile(t, staticPDF.Location()); !bytes.Equal(got, pdfBytes) {
		t.Fatalf("agent staged PDF = %q, want committed PDF bytes", got)
	}
	if manifestInput(t, staticEnvironment.Manifest(), `$.field("source")`).Workspace() == false {
		t.Fatal("agent source tree was not selected as the writable base")
	}
	if manifestInput(t, remediationEnvironment.Manifest(), `$.field("source")`).Workspace() == false {
		t.Fatal("script source tree was not selected as the writable base")
	}
	if got := string(mustReadFile(t, filepath.Join(staticEnvironment.Workspace(), "decision.txt"))); got != "base\n" {
		t.Fatalf("static branch base = %q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(remediationEnvironment.Workspace(), "decision.txt"))); got != "base\n" {
		t.Fatalf("remediation branch base = %q", got)
	}

	mustWriteFile(t, filepath.Join(staticEnvironment.Workspace(), "decision.txt"), "static\n")
	mustWriteFile(t, filepath.Join(staticEnvironment.Workspace(), "src", "static.go"), "package service\n")
	mustWriteFile(t, filepath.Join(staticEnvironment.Workspace(), "static-private.tmp"), "static cache\n")
	mustWriteFile(t, filepath.Join(remediationEnvironment.Workspace(), "decision.txt"), "remediation\n")
	mustWriteFile(t, filepath.Join(remediationEnvironment.Workspace(), "src", "remediation.go"), "package service\n")
	mustWriteFile(t, filepath.Join(remediationEnvironment.Workspace(), "remediation-private.tmp"), "remediation cache\n")
	if got := string(mustReadFile(t, filepath.Join(staticEnvironment.Workspace(), "decision.txt"))); got != "static\n" {
		t.Fatalf("static branch decision = %q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(remediationEnvironment.Workspace(), "decision.txt"))); got != "remediation\n" {
		t.Fatalf("remediation branch decision = %q", got)
	}

	staticCandidate := captureBranch(t, ctx, staticEnvironment,
		"static_analysis_json", `{"findings":2}`+"\n",
		"validation_report", "validation complete\n",
	)
	assertBranchCandidate(t, staticCandidate, "static_analysis_json", "validation_report")
	remediationCandidate := captureBranch(t, ctx, remediationEnvironment,
		"scored_findings_json", `[{"severity":9}]`+"\n",
		"remediation_report", "remediation prepared\n",
	)
	assertBranchCandidate(t, remediationCandidate, "scored_findings_json", "remediation_report")
	staticCommitted := commitAndLoadValue(t, ctx, values, staticCandidate)
	remediationCommitted := commitAndLoadValue(t, ctx, values, remediationCandidate)
	if err := staticEnvironment.Close(); err != nil {
		t.Fatal(err)
	}
	if err := remediationEnvironment.Close(); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, staticRuntimeRoot)
	assertAbsent(t, remediationRuntimeRoot)

	staticTree := mustTree(t, objectMember(t, staticCommitted, "continued"))
	remediationTree := mustTree(t, objectMember(t, remediationCommitted, "continued"))
	if staticTree.Equal(remediationTree) || staticTree.Equal(source) || remediationTree.Equal(source) {
		t.Fatal("private branch edits did not produce three independent tree identities")
	}
	committedSource := filepath.Join(t.TempDir(), "committed-source")
	if err := repository.MaterializeTree(ctx, source, committedSource); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, filepath.Join(committedSource, "decision.txt"))); got != "base\n" {
		t.Fatalf("committed source changed to %q", got)
	}
	assertAbsent(t, filepath.Join(committedSource, "src", "static.go"))
	assertAbsent(t, filepath.Join(committedSource, "src", "remediation.go"))
	if err := os.RemoveAll(committedSource); err != nil {
		t.Fatal(err)
	}

	validationType := mustTypeObject(t,
		mustField(t, "static_analysis_json", plainFile),
		mustField(t, "validation_report", plainFile),
	)
	scoringType := mustTypeObject(t, mustField(t, "scored_findings_json", plainFile))
	remediationType := mustTypeObject(t,
		mustField(t, "remediation_report", plainFile),
		mustField(t, "source", value.Tree()),
	)
	mergeInputs := mustContract(t,
		mustField(t, "base", value.Tree()),
		mustField(t, "other", value.Tree()),
		mustField(t, "validation", validationType),
		mustField(t, "scoring", scoringType),
		mustField(t, "remediation", remediationType),
	)
	mergeInput := mustObject(t,
		mustEntry(t, "base", value.NewTreeValue(staticTree)),
		mustEntry(t, "other", value.NewTreeValue(remediationTree)),
		mustEntry(t, "validation", mustObject(t,
			mustEntry(t, "static_analysis_json", objectMember(t, staticCommitted, "static_analysis_json")),
			mustEntry(t, "validation_report", objectMember(t, staticCommitted, "validation_report")),
		)),
		mustEntry(t, "scoring", mustObject(t,
			mustEntry(t, "scored_findings_json", objectMember(t, remediationCommitted, "scored_findings_json")),
		)),
		mustEntry(t, "remediation", mustObject(t,
			mustEntry(t, "remediation_report", objectMember(t, remediationCommitted, "remediation_report")),
			mustEntry(t, "source", value.NewTreeValue(remediationTree)),
		)),
	)
	mergeOutputs := mustContract(t,
		mustField(t, "continued", value.Tree()),
		mustField(t, "merge_report", plainFile),
	)
	mergeLeaf := compileLeaf(t, workflow.Script, mergeInputs, mergeOutputs, []string{"base"}, []string{"continued"})
	mergeEnvironment, err := workspace.Prepare(ctx, repository, mergeLeaf, mergeInput)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mergeEnvironment.Close() })
	mergeRuntimeRoot := filepath.Dir(mergeEnvironment.Workspace())
	writableBases := 0
	for _, input := range mergeEnvironment.Manifest().Inputs() {
		if input.Workspace() {
			writableBases++
		}
	}
	if writableBases != 1 {
		t.Fatalf("fan-in writable bases = %d, want exactly one", writableBases)
	}
	other := manifestInput(t, mergeEnvironment.Manifest(), `$.field("other")`)
	if other.Workspace() {
		t.Fatal("the second branch tree was made writable")
	}
	if got := string(mustReadFile(t, filepath.Join(mergeEnvironment.Workspace(), "decision.txt"))); got != "static\n" {
		t.Fatalf("fan-in base decision = %q", got)
	}
	assertAbsent(t, filepath.Join(mergeEnvironment.Workspace(), "src", "remediation.go"))
	otherDecision := string(mustReadFile(t, filepath.Join(other.Location(), "decision.txt")))
	otherChange := mustReadFile(t, filepath.Join(other.Location(), "src", "remediation.go"))
	mustWriteFile(t, filepath.Join(mergeEnvironment.Workspace(), "decision.txt"), "static+"+strings.TrimSpace(otherDecision)+"\n")
	mustWriteFile(t, filepath.Join(mergeEnvironment.Workspace(), "src", "remediation.go"), string(otherChange))

	mergeReportTarget, err := mergeEnvironment.Output(value.Path{}.Field("merge_report"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, mergeReportTarget.Location(), "explicit reconciliation complete\n")
	mergeCandidate, err := mergeEnvironment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		continued, err := outputs.Workspace(ctx)
		if err != nil {
			return value.Value{}, err
		}
		report, err := outputs.File(ctx, value.Path{}.Field("merge_report"), "merge-report.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t,
			mustEntry(t, "continued", continued),
			mustEntry(t, "merge_report", report),
		), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mergeCommitted := commitAndLoadValue(t, ctx, values, mergeCandidate)
	if err := mergeEnvironment.Close(); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, mergeRuntimeRoot)

	mergedTree := mustTree(t, objectMember(t, mergeCommitted, "continued"))
	finalReportType := mustTypeObject(t,
		mustField(t, "merge_report", plainFile),
		mustField(t, "source", value.Tree()),
	)
	assessmentType := mustTypeObject(t,
		mustField(t, "validation", validationType),
		mustField(t, "scoring", scoringType),
		mustField(t, "remediation", remediationType),
		mustField(t, "final_report", finalReportType),
	)
	assetsType := mustTypeObject(t,
		mustField(t, "brief", pdfType),
		mustField(t, "validators", value.Tree()),
		mustField(t, "skills", value.Tree()),
	)
	finalInputs := mustContract(t,
		mustField(t, "assessment", assessmentType),
		mustField(t, "assets", assetsType),
	)
	finalInput := mustObject(t,
		mustEntry(t, "assessment", mustObject(t,
			mustEntry(t, "validation", objectMember(t, mergeInput, "validation")),
			mustEntry(t, "scoring", objectMember(t, mergeInput, "scoring")),
			mustEntry(t, "remediation", objectMember(t, mergeInput, "remediation")),
			mustEntry(t, "final_report", mustObject(t,
				mustEntry(t, "merge_report", objectMember(t, mergeCommitted, "merge_report")),
				mustEntry(t, "source", value.NewTreeValue(mergedTree)),
			)),
		)),
		mustEntry(t, "assets", mustObject(t,
			mustEntry(t, "brief", value.NewFileValue(brief)),
			mustEntry(t, "validators", value.NewTreeValue(validators)),
			mustEntry(t, "skills", value.NewTreeValue(skills)),
		)),
	)
	if err := finalInputs.Validate(finalInput); err != nil {
		t.Fatal(err)
	}
	finalInput = commitAndLoadValue(t, ctx, values, finalInput)
	finalOutputs := mustContract(t, mustField(t, "report", plainFile))
	finalLeaf := compileLeaf(t, workflow.Agent, finalInputs, finalOutputs,
		[]string{"assessment", "final_report", "source"}, nil)
	finalEnvironment, err := workspace.Prepare(ctx, repository, finalLeaf, finalInput)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = finalEnvironment.Close() })
	finalRuntimeRoot := filepath.Dir(finalEnvironment.Workspace())
	if got := string(mustReadFile(t, filepath.Join(finalEnvironment.Workspace(), "decision.txt"))); got != "static+remediation\n" {
		t.Fatalf("explicitly merged decision = %q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(finalEnvironment.Workspace(), "src", "static.go"))); got != "package service\n" {
		t.Fatalf("merged static change = %q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(finalEnvironment.Workspace(), "src", "remediation.go"))); got != "package service\n" {
		t.Fatalf("merged remediation change = %q", got)
	}
	if got := mustReadFile(t, manifestInput(t, finalEnvironment.Manifest(), `$.field("assets").field("brief")`).Location()); !bytes.Equal(got, pdfBytes) {
		t.Fatalf("final staged PDF = %q, want committed PDF bytes", got)
	}
	validatorInput := manifestInput(t, finalEnvironment.Manifest(), `$.field("assets").field("validators")`)
	if got := string(mustReadFile(t, filepath.Join(validatorInput.Location(), "policy", "validate.rego"))); got != "package validation\n" {
		t.Fatalf("staged validator = %q", got)
	}
	skillInput := manifestInput(t, finalEnvironment.Manifest(), `$.field("assets").field("skills")`)
	if got := string(mustReadFile(t, filepath.Join(skillInput.Location(), "review", "SKILL.md"))); got != "# Review\n" {
		t.Fatalf("staged skill = %q", got)
	}

	reportTarget, err := finalEnvironment.Output(value.Path{}.Field("report"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(finalEnvironment.Workspace(), "unpublished-notes.tmp"), "private notes\n")
	mustWriteFile(t, reportTarget.Location(), "Prestige assessment complete\n")
	lateCandidate, err := finalEnvironment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		report, err := outputs.File(ctx, value.Path{}.Field("report"), "prestige-assessment.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "report", report)), errors.New("injected after complete candidate assembly")
	})
	assertZeroCaptureError(t, lateCandidate, err)

	finalCandidate, err := finalEnvironment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		report, err := outputs.File(ctx, value.Path{}.Field("report"), "prestige-assessment.txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t, mustEntry(t, "report", report)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finalCommitted := commitAndLoadValue(t, ctx, values, finalCandidate)
	if len(finalCommitted.Entries()) != 1 || finalCommitted.Entries()[0].Name() != "report" {
		t.Fatalf("final committed fields = %#v, want only the declared report", finalCommitted.Entries())
	}
	finalReport := mustFile(t, objectMember(t, finalCommitted, "report"))
	var finalBytes bytes.Buffer
	if err := repository.CopyFile(ctx, finalReport, &finalBytes); err != nil {
		t.Fatal(err)
	}
	if finalBytes.String() != "Prestige assessment complete\n" {
		t.Fatalf("final report = %q", finalBytes.String())
	}
	if err := finalEnvironment.Close(); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, finalRuntimeRoot)

	store.Corrupt(brief.Digest())
	corruptEnvironment, err := workspace.Prepare(ctx, repository, finalLeaf, finalInput)
	if err == nil || corruptEnvironment != nil || !strings.Contains(strings.ToLower(err.Error()), "integrity") {
		t.Fatalf("prepare with corrupt committed PDF = (%v, %v), want an integrity failure", corruptEnvironment, err)
	}
}

func captureBranch(t *testing.T, ctx context.Context, environment *workspace.Environment, firstName, firstData, secondName, secondData string) value.Value {
	t.Helper()
	firstPath := value.Path{}.Field(firstName)
	secondPath := value.Path{}.Field(secondName)
	first, err := environment.Output(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := environment.Output(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, first.Location(), firstData)
	mustWriteFile(t, second.Location(), secondData)
	candidate, err := environment.Capture(ctx, func(outputs workspace.Outputs) (value.Value, error) {
		continued, err := outputs.Workspace(ctx)
		if err != nil {
			return value.Value{}, err
		}
		firstValue, err := outputs.File(ctx, firstPath, firstName+".txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		secondValue, err := outputs.File(ctx, secondPath, secondName+".txt", "text/plain")
		if err != nil {
			return value.Value{}, err
		}
		return mustObject(t,
			mustEntry(t, "continued", continued),
			mustEntry(t, firstName, firstValue),
			mustEntry(t, secondName, secondValue),
		), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func assertBranchCandidate(t *testing.T, candidate value.Value, firstName, secondName string) {
	t.Helper()
	want := map[string]value.Kind{
		"continued": value.TreeKind,
		firstName:   value.FileKind,
		secondName:  value.FileKind,
	}
	entries := candidate.Entries()
	if len(entries) != len(want) {
		t.Fatalf("branch candidate fields = %#v, want exactly three declared results", entries)
	}
	for _, entry := range entries {
		kind, ok := want[entry.Name()]
		if !ok || entry.Value().Kind() != kind {
			t.Fatalf("branch candidate member %q kind = %v", entry.Name(), entry.Value().Kind())
		}
	}
}

func compileAttachmentLeaf(t *testing.T, inputs value.Contract, attachments []workflow.AttachmentDraft) workflow.Leaf {
	t.Helper()
	draft := workflow.GraphDraft{
		Inputs: inputs, Outputs: value.EmptyContract(),
		Nodes: []workflow.NodeDraft{{
			Name: "raw", Leaf: &workflow.LeafDraft{
				Kind: workflow.LLM, Inputs: inputs, Outputs: value.EmptyContract(), Attachments: attachments,
			},
		}},
	}
	bindings := make([]workflow.BindingDraft, 0, len(inputs.Ports()))
	for _, port := range inputs.Ports() {
		bindings = append(bindings, workflow.BindingDraft{From: []string{port.Name()}, To: port.Name()})
	}
	draft.Edges = append(draft.Edges, workflow.EdgeDraft{
		From:     workflow.EndpointDraft{Kind: workflow.Boundary},
		To:       workflow.EndpointDraft{Kind: workflow.Child, Child: "raw"},
		Bindings: bindings,
	})
	definition, err := workflow.Compile(workflow.ProgramDraft{
		Root:    "root",
		Modules: []workflow.ModuleDraft{{Name: "root", Graph: draft}},
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, ok := definition.Root().Nodes()[0].Leaf()
	if !ok {
		t.Fatal("compiled raw node is not a leaf")
	}
	return leaf
}

func commitAndLoadValue(t *testing.T, ctx context.Context, store *value.Store, candidate value.Value) value.Value {
	t.Helper()
	object, err := store.Put(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := store.Get(ctx, object.Digest())
	if err != nil {
		t.Fatal(err)
	}
	if !committed.Equal(candidate) {
		t.Fatal("committed value changed during round trip")
	}
	return committed
}

func manifestInput(t *testing.T, manifest workspace.Manifest, path string) workspace.Input {
	t.Helper()
	for _, input := range manifest.Inputs() {
		if input.ValuePath().String() == path {
			return input
		}
	}
	t.Fatalf("manifest has no input %s", path)
	return workspace.Input{}
}

func mustFileType(t *testing.T, media ...string) value.Type {
	t.Helper()
	typ, err := value.File(media...)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func mustCaptureTree(t *testing.T, repository *content.Repository, root string) content.Tree {
	t.Helper()
	tree, err := repository.CaptureTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func mustTree(t *testing.T, member value.Value) content.Tree {
	t.Helper()
	tree, ok := member.Tree()
	if !ok {
		t.Fatalf("value kind = %v, want tree", member.Kind())
	}
	return tree
}

func mustFile(t *testing.T, member value.Value) content.File {
	t.Helper()
	file, ok := member.File()
	if !ok {
		t.Fatalf("value kind = %v, want file", member.Kind())
	}
	return file
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %q status = %v, want absent", path, err)
	}
}

type conformanceFaultStore struct {
	content.Store
	mu      sync.RWMutex
	corrupt content.Digest
}

func (s *conformanceFaultStore) Corrupt(digest content.Digest) {
	s.mu.Lock()
	s.corrupt = digest
	s.mu.Unlock()
}

func (s *conformanceFaultStore) Copy(ctx context.Context, digest content.Digest, destination io.Writer) (int64, error) {
	s.mu.RLock()
	corrupt := s.corrupt
	s.mu.RUnlock()
	if !digest.Equal(corrupt) {
		return s.Store.Copy(ctx, digest, destination)
	}
	var stored bytes.Buffer
	if _, err := s.Store.Copy(ctx, digest, &stored); err != nil {
		return 0, err
	}
	data := stored.Bytes()
	if len(data) == 0 {
		return 0, errors.New("conformance corruption target was empty")
	}
	data[0] ^= 0xff
	written, err := destination.Write(data)
	if err != nil {
		return int64(written), err
	}
	if written != len(data) {
		return int64(written), io.ErrShortWrite
	}
	return int64(written), nil
}
