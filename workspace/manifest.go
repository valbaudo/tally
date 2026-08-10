// Package workspace prepares one private filesystem environment for an agent
// or script invocation.
package workspace

import (
	"github.com/valbaudo/dawn/content"
	"github.com/valbaudo/dawn/value"
)

// Manifest is an immutable inspection view of the file and tree locations
// prepared for one invocation.
type Manifest struct {
	inputs  []Input
	outputs []Output
}

// Inputs returns a defensive copy in canonical value-path order.
func (m Manifest) Inputs() []Input { return append([]Input(nil), m.inputs...) }

// Outputs returns a defensive copy in canonical schema-path order.
func (m Manifest) Outputs() []Output { return append([]Output(nil), m.outputs...) }

// Input describes one materialized file or tree input. Location is an absolute
// invocation-local path; RelativeLocation is its deterministic layout beneath
// the private runtime root.
type Input struct {
	valuePath        value.Path
	kind             value.Kind
	location         string
	relativeLocation string
	workspace        bool
	file             *content.File
	tree             *content.Tree
}

// ValuePath returns a defensive copy of the structured input path.
func (i Input) ValuePath() value.Path { return cloneValuePath(i.valuePath) }

// Kind returns FileKind or TreeKind.
func (i Input) Kind() value.Kind { return i.kind }

// Location returns the absolute invocation-local materialization path.
func (i Input) Location() string { return i.location }

// RelativeLocation returns the deterministic slash-separated runtime layout.
func (i Input) RelativeLocation() string { return i.relativeLocation }

// Workspace reports whether this input is the tree selected as the writable
// workspace base.
func (i Input) Workspace() bool { return i.workspace }

// Digest returns the committed content identity.
func (i Input) Digest() content.Digest {
	if i.file != nil {
		return i.file.Digest()
	}
	if i.tree != nil {
		return i.tree.Digest()
	}
	return content.Digest{}
}

// Name returns the exact logical filename for a file input.
func (i Input) Name() (string, bool) {
	if i.file == nil {
		return "", false
	}
	return i.file.Name(), true
}

// Media returns the exact declared media for a file input.
func (i Input) Media() (string, bool) {
	if i.file == nil {
		return "", false
	}
	return i.file.Media(), true
}

// Size returns the committed byte length for a file input.
func (i Input) Size() (int64, bool) {
	if i.file == nil {
		return 0, false
	}
	return i.file.Size(), true
}

// Output describes one statically allocated target or one namespace for a
// dynamic list/map file or tree schema position.
type Output struct {
	valuePath        value.Path
	schemaPath       string
	kind             value.Kind
	location         string
	relativeLocation string
	dynamic          bool
}

// ValuePath returns the exact path for a static output. For a dynamic output it
// returns the statically known prefix before the first list/map member.
func (o Output) ValuePath() value.Path { return cloneValuePath(o.valuePath) }

// SchemaPath returns a deterministic escaped diagnostic with [*] and key(*)
// placeholders for dynamic list and map members.
func (o Output) SchemaPath() string { return o.schemaPath }

// Kind returns FileKind or TreeKind.
func (o Output) Kind() value.Kind { return o.kind }

// Location returns the absolute static target or dynamic namespace path.
func (o Output) Location() string { return o.location }

// RelativeLocation returns the deterministic slash-separated runtime layout.
func (o Output) RelativeLocation() string { return o.relativeLocation }

// Dynamic reports whether this record is a namespace whose members are
// allocated lazily by Environment.Output.
func (o Output) Dynamic() bool { return o.dynamic }

func cloneValuePath(path value.Path) value.Path {
	var cloned value.Path
	for _, segment := range path.Segments() {
		switch segment.Kind() {
		case value.FieldSegment:
			name, _ := segment.Name()
			cloned = cloned.Field(name)
		case value.MapKeySegment:
			name, _ := segment.Name()
			cloned = cloned.MapKey(name)
		case value.ListIndexSegment:
			index, _ := segment.Index()
			cloned = cloned.ListIndex(index)
		}
	}
	return cloned
}
