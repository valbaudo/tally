package workflow

import (
	"fmt"
	"mime"
	"sort"
	"strings"

	"github.com/valbaudo/dawn/value"
)

// Fidelity is the required semantic fidelity for one raw-LLM file delivery.
type Fidelity uint8

const (
	TextFidelity Fidelity = iota + 1
	VisualFidelity
)

// AttachmentDraft selects one input schema subtree for raw-LLM delivery.
type AttachmentDraft struct {
	Input    []string
	Fidelity Fidelity
}

// Attachment is one immutable canonical raw-LLM file-delivery requirement.
type Attachment struct {
	input    []string
	fidelity Fidelity
}

// Input returns the selected input schema path.
func (a Attachment) Input() []string { return append([]string(nil), a.input...) }

// Fidelity returns the required delivery fidelity.
func (a Attachment) Fidelity() Fidelity { return a.fidelity }

// DefaultFidelity returns the raw-LLM delivery default for concrete media.
func DefaultFidelity(media string) Fidelity {
	typeName, _, err := mime.ParseMediaType(media)
	if err != nil {
		typeName = strings.TrimSpace(strings.SplitN(media, ";", 2)[0])
	}
	typeName = strings.ToLower(typeName)
	if typeName == "application/pdf" || strings.HasPrefix(typeName, "image/") {
		return VisualFidelity
	}
	return TextFidelity
}

type deliveryIntent struct {
	baseTree         []string
	publishWorkspace []string
	attachments      []Attachment
}

func compileDelivery(draft LeafDraft, inputs, outputs value.Contract) (deliveryIntent, error) {
	workspaceDeclared := len(draft.BaseTree) != 0 || len(draft.PublishWorkspace) != 0
	if workspaceDeclared && draft.Kind != Agent && draft.Kind != Script {
		return deliveryIntent{}, fmt.Errorf("leaf kind %q cannot declare workspace delivery", draft.Kind)
	}
	if len(draft.Attachments) != 0 && draft.Kind != LLM {
		return deliveryIntent{}, fmt.Errorf("leaf kind %q cannot declare attachments", draft.Kind)
	}

	intent := deliveryIntent{}
	if len(draft.BaseTree) != 0 {
		if !resolvesRequiredTree(inputs, draft.BaseTree) {
			return deliveryIntent{}, fmt.Errorf("base tree path %q must resolve to a required tree input with required ancestors", draft.BaseTree)
		}
		intent.baseTree = append([]string(nil), draft.BaseTree...)
	}
	if len(draft.PublishWorkspace) != 0 {
		if !resolvesRequiredTree(outputs, draft.PublishWorkspace) {
			return deliveryIntent{}, fmt.Errorf("workspace publication path %q must resolve to a required tree output with required ancestors", draft.PublishWorkspace)
		}
		intent.publishWorkspace = append([]string(nil), draft.PublishWorkspace...)
	}

	for _, draftAttachment := range draft.Attachments {
		if !validFidelity(draftAttachment.Fidelity) {
			return deliveryIntent{}, fmt.Errorf("attachment input %q has unknown fidelity %d", draftAttachment.Input, draftAttachment.Fidelity)
		}
		typ, ok := inputs.Resolve(draftAttachment.Input...)
		if !ok || !typeContains(typ, value.FileKind) {
			return deliveryIntent{}, fmt.Errorf("attachment input %q must select a subtree containing a file", draftAttachment.Input)
		}
		intent.attachments = append(intent.attachments, Attachment{
			input: append([]string(nil), draftAttachment.Input...), fidelity: draftAttachment.Fidelity,
		})
	}
	normalizeAttachments(&intent.attachments)
	for left := 0; left < len(intent.attachments); left++ {
		for right := left + 1; right < len(intent.attachments); right++ {
			if intent.attachments[left].fidelity == intent.attachments[right].fidelity {
				continue
			}
			if pathPrefix(intent.attachments[left].input, intent.attachments[right].input) || pathPrefix(intent.attachments[right].input, intent.attachments[left].input) {
				return deliveryIntent{}, fmt.Errorf("attachment inputs %q and %q conflict on fidelity", intent.attachments[left].input, intent.attachments[right].input)
			}
		}
	}

	if draft.Kind == LLM {
		for _, port := range inputs.Ports() {
			if typeContains(port.Type(), value.TreeKind) {
				return deliveryIntent{}, fmt.Errorf("raw LLM input %q contains a tree", port.Name())
			}
		}
	}
	return intent, nil
}

func validFidelity(fidelity Fidelity) bool {
	return fidelity == TextFidelity || fidelity == VisualFidelity
}

func resolvesRequiredTree(contract value.Contract, path []string) bool {
	if len(path) == 0 {
		return false
	}
	fields := contract.Ports()
	var typ value.Type
	for index, segment := range path {
		field, found := fieldNamed(fields, segment)
		if !found || field.Optional() {
			return false
		}
		typ = field.Type()
		if index == len(path)-1 {
			break
		}
		if typ.Kind() != value.ObjectKind {
			return false
		}
		fields = typ.Fields()
	}
	return typ.Kind() == value.TreeKind
}

func typeContains(typ value.Type, kind value.Kind) bool {
	pending := []value.Type{typ}
	for len(pending) != 0 {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current.Kind() == kind {
			return true
		}
		switch current.Kind() {
		case value.ObjectKind:
			for _, field := range current.Fields() {
				pending = append(pending, field.Type())
			}
		case value.ListKind, value.MapKind:
			if element, ok := current.Element(); ok {
				pending = append(pending, element)
			}
		}
	}
	return false
}

func normalizeAttachments(attachments *[]Attachment) {
	sort.Slice(*attachments, func(left, right int) bool {
		if result := compareStrings((*attachments)[left].input, (*attachments)[right].input); result != 0 {
			return result < 0
		}
		return (*attachments)[left].fidelity < (*attachments)[right].fidelity
	})
	if len(*attachments) == 0 {
		return
	}
	write := 1
	for _, attachment := range (*attachments)[1:] {
		previous := (*attachments)[write-1]
		if compareStrings(previous.input, attachment.input) == 0 && previous.fidelity == attachment.fidelity {
			continue
		}
		(*attachments)[write] = attachment
		write++
	}
	*attachments = (*attachments)[:write]
}

func pathPrefix(prefix, path []string) bool {
	if len(prefix) > len(path) {
		return false
	}
	for index := range prefix {
		if prefix[index] != path[index] {
			return false
		}
	}
	return true
}

func cloneAttachments(attachments []Attachment) []Attachment {
	if attachments == nil {
		return nil
	}
	cloned := make([]Attachment, len(attachments))
	for index, attachment := range attachments {
		cloned[index] = Attachment{input: append([]string(nil), attachment.input...), fidelity: attachment.fidelity}
	}
	return cloned
}
