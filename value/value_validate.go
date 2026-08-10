package value

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"unicode/utf8"
)

// ValidationError reports a contract mismatch at a structured value path.
type ValidationError struct {
	path    Path
	problem string
}

// Error renders an escaped diagnostic; callers needing identity use Path.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.path.String(), e.problem)
}

// Path returns a defensive copy of the mismatch path.
func (e *ValidationError) Path() Path {
	return Path{segments: e.path.Segments()}
}

type validationPath struct {
	parent  *validationPath
	segment Segment
	length  int
}

func childValidationPath(parent *validationPath, segment Segment) *validationPath {
	length := 1
	if parent != nil {
		length += parent.length
	}
	return &validationPath{parent: parent, segment: segment, length: length}
}

func materializeValidationPath(node *validationPath) Path {
	if node == nil {
		return Path{}
	}
	segments := make([]Segment, node.length)
	for i := len(segments) - 1; i >= 0; i-- {
		segments[i] = node.segment
		node = node.parent
	}
	return Path{segments: segments}
}

func validationFailure(path *validationPath, problem string) error {
	return &ValidationError{path: materializeValidationPath(path), problem: problem}
}

type validationTask struct {
	typ   Type
	value Value
	path  *validationPath
}

// Validate checks a runtime value without coercion or object projection.
func (t Type) Validate(value Value) error {
	if !t.Valid() {
		return fmt.Errorf("invalid type")
	}
	if !value.Valid() {
		return fmt.Errorf("invalid value")
	}
	tasks := []validationTask{{typ: t, value: value}}
	for len(tasks) > 0 {
		task := tasks[len(tasks)-1]
		tasks = tasks[:len(tasks)-1]
		currentType, current := task.typ, task.value

		switch currentType.kind {
		case StringKind:
			if current.kind != StringKind {
				return validationFailure(task.path, "want string")
			}
		case IntegerKind:
			if current.kind != IntegerKind {
				return validationFailure(task.path, "want integer")
			}
		case NumberKind:
			if current.kind != IntegerKind && current.kind != NumberKind {
				return validationFailure(task.path, "want number")
			}
		case BooleanKind:
			if current.kind != BooleanKind {
				return validationFailure(task.path, "want boolean")
			}
		case NullKind:
			if current.kind != NullKind {
				return validationFailure(task.path, "want null")
			}
		case EnumKind:
			ordinary, ok := scalarOrdinaryCanonical(current)
			if !ok {
				return validationFailure(task.path, "want scalar enum member")
			}
			matched := false
			for _, member := range currentType.enum {
				if bytes.Equal(member.canonical, ordinary) {
					matched = true
					break
				}
			}
			if !matched {
				return validationFailure(task.path, "value is not an enum member")
			}
		case ObjectKind:
			if current.kind != ObjectKind {
				return validationFailure(task.path, "want object")
			}
			children := make([]validationTask, 0, len(current.entries))
			entryIndex, fieldIndex := 0, 0
			for entryIndex < len(current.entries) || fieldIndex < len(currentType.fields) {
				switch {
				case entryIndex == len(current.entries):
					field := currentType.fields[fieldIndex]
					if !field.optional {
						path := childValidationPath(task.path, Segment{kind: FieldSegment, name: field.name})
						return validationFailure(path, "required field is absent")
					}
					fieldIndex++
				case fieldIndex == len(currentType.fields):
					entry := current.entries[entryIndex]
					path := childValidationPath(task.path, Segment{kind: FieldSegment, name: entry.name})
					return validationFailure(path, "undeclared field")
				default:
					entry, field := current.entries[entryIndex], currentType.fields[fieldIndex]
					switch {
					case entry.name < field.name:
						path := childValidationPath(task.path, Segment{kind: FieldSegment, name: entry.name})
						return validationFailure(path, "undeclared field")
					case field.name < entry.name:
						if !field.optional {
							path := childValidationPath(task.path, Segment{kind: FieldSegment, name: field.name})
							return validationFailure(path, "required field is absent")
						}
						fieldIndex++
					default:
						children = append(children, validationTask{
							typ:   field.typ,
							value: entry.value,
							path:  childValidationPath(task.path, Segment{kind: FieldSegment, name: entry.name}),
						})
						entryIndex++
						fieldIndex++
					}
				}
			}
			for i := len(children) - 1; i >= 0; i-- {
				tasks = append(tasks, children[i])
			}
		case MapKind:
			if current.kind != MapKind {
				return validationFailure(task.path, "want map")
			}
			for i := len(current.entries) - 1; i >= 0; i-- {
				entry := current.entries[i]
				tasks = append(tasks, validationTask{
					typ:   *currentType.elem,
					value: entry.value,
					path:  childValidationPath(task.path, Segment{kind: MapKeySegment, name: entry.name}),
				})
			}
		case ListKind:
			if current.kind != ListKind {
				return validationFailure(task.path, "want list")
			}
			for i := len(current.items) - 1; i >= 0; i-- {
				tasks = append(tasks, validationTask{
					typ:   *currentType.elem,
					value: current.items[i],
					path:  childValidationPath(task.path, Segment{kind: ListIndexSegment, index: i}),
				})
			}
		case FileKind:
			if current.kind != FileKind {
				return validationFailure(task.path, "want file")
			}
			base, _, err := mime.ParseMediaType(current.file.Media())
			if err != nil {
				return validationFailure(task.path, "file has invalid media")
			}
			if len(currentType.media) > 0 {
				matched := false
				for _, constraint := range currentType.media {
					if mediaContains(constraint, base) {
						matched = true
						break
					}
				}
				if !matched {
					return validationFailure(task.path, "file media is not accepted")
				}
			}
		case TreeKind:
			if current.kind != TreeKind {
				return validationFailure(task.path, "want tree")
			}
		case AnyKind:
			switch current.kind {
			case FileKind, TreeKind:
				return validationFailure(task.path, "ordinary any rejects file and tree values")
			case ObjectKind:
				for i := len(current.entries) - 1; i >= 0; i-- {
					entry := current.entries[i]
					tasks = append(tasks, validationTask{typ: currentType, value: entry.value, path: childValidationPath(task.path, Segment{kind: FieldSegment, name: entry.name})})
				}
			case MapKind:
				for i := len(current.entries) - 1; i >= 0; i-- {
					entry := current.entries[i]
					tasks = append(tasks, validationTask{typ: currentType, value: entry.value, path: childValidationPath(task.path, Segment{kind: MapKeySegment, name: entry.name})})
				}
			case ListKind:
				for i := len(current.items) - 1; i >= 0; i-- {
					tasks = append(tasks, validationTask{typ: currentType, value: current.items[i], path: childValidationPath(task.path, Segment{kind: ListIndexSegment, index: i})})
				}
			}
		default:
			return fmt.Errorf("invalid type")
		}
	}
	return nil
}

func scalarOrdinaryCanonical(value Value) ([]byte, bool) {
	switch value.kind {
	case StringKind:
		if !utf8.ValidString(value.text) {
			return nil, false
		}
		canonical, err := json.Marshal(value.text)
		return canonical, err == nil
	case IntegerKind, NumberKind:
		return []byte(value.number), true
	case BooleanKind:
		if value.boolean {
			return []byte("true"), true
		}
		return []byte("false"), true
	case NullKind:
		return []byte("null"), true
	default:
		return nil, false
	}
}

// Validate checks that value is a closed object satisfying all contract ports.
func (c Contract) Validate(value Value) error {
	if !c.Valid() {
		return fmt.Errorf("invalid contract")
	}
	return c.ObjectType().Validate(value)
}
