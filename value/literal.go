package value

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Literal is one canonical ordinary JSON value.
type Literal struct {
	canonical []byte
	decoded   any
}

// ParseLiteral parses exactly one ordinary JSON value into canonical form.
func ParseLiteral(data []byte) (Literal, error) {
	if err := validateOneJSONValue(data); err != nil {
		return Literal{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return Literal{}, fmt.Errorf("decode literal: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Literal{}, err
	}
	canonical, err := canonicalJSON(decoded)
	if err != nil {
		return Literal{}, err
	}
	return Literal{canonical: canonical, decoded: decoded}, nil
}

func validateOneJSONValue(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := validateJSONValue(decoder); err != nil {
		return fmt.Errorf("invalid literal: %w", err)
	}
	return requireEOF(decoder)
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if _, exists := keys[key]; exists {
					return fmt.Errorf("duplicate object key %q", key)
				}
				keys[key] = struct{}{}
				if err := validateJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return fmt.Errorf("object did not end")
			}
		case '[':
			for decoder.More() {
				if err := validateJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return fmt.Errorf("array did not end")
			}
		default:
			return fmt.Errorf("unexpected delimiter %q", value)
		}
	case json.Number:
		if !validJSONNumber(value.String()) {
			return fmt.Errorf("invalid number %q", value)
		}
	case string, bool, nil:
		return nil
	default:
		return fmt.Errorf("unexpected JSON token %T", token)
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("literal contains trailing data")
		}
		return fmt.Errorf("literal contains trailing data: %w", err)
	}
	return nil
}

func validJSONNumber(number string) bool {
	if number == "" {
		return false
	}
	i := 0
	if number[i] == '-' {
		i++
		if i == len(number) {
			return false
		}
	}
	if number[i] == '0' {
		i++
	} else if number[i] >= '1' && number[i] <= '9' {
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
	} else {
		return false
	}
	if i < len(number) && number[i] == '.' {
		i++
		start := i
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(number) && (number[i] == 'e' || number[i] == 'E') {
		i++
		if i < len(number) && (number[i] == '+' || number[i] == '-') {
			i++
		}
		start := i
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(number)
}

func canonicalJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := appendCanonicalJSON(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonicalJSON(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		out.Write(encoded)
	case json.Number:
		if !validJSONNumber(value.String()) {
			return fmt.Errorf("invalid number %q", value)
		}
		out.WriteString(value.String())
	case []any:
		out.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, err := json.Marshal(key)
			if err != nil {
				return err
			}
			out.Write(encoded)
			out.WriteByte(':')
			if err := appendCanonicalJSON(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", value)
	}
	return nil
}

// Bytes returns a copy of the literal's canonical JSON bytes.
func (l Literal) Bytes() []byte { return append([]byte(nil), l.canonical...) }

// Equal reports whether two literals have identical canonical JSON bytes.
func (l Literal) Equal(other Literal) bool { return bytes.Equal(l.canonical, other.canonical) }

func (l Literal) valid() bool { return len(l.canonical) > 0 }

func (l Literal) scalar() bool {
	switch l.decoded.(type) {
	case nil, bool, string, json.Number:
		return l.valid()
	default:
		return false
	}
}

// ValidateLiteral checks that l satisfies t without coercion or projection.
func (t Type) ValidateLiteral(l Literal) error {
	if !t.Valid() {
		return fmt.Errorf("invalid type")
	}
	if !l.valid() {
		return fmt.Errorf("invalid literal")
	}
	return validateLiteral(t, l)
}

func validateLiteral(t Type, l Literal) error {
	switch t.kind {
	case StringKind:
		if _, ok := l.decoded.(string); !ok {
			return fmt.Errorf("want string")
		}
	case IntegerKind:
		number, ok := l.decoded.(json.Number)
		if !ok || strings.ContainsAny(number.String(), ".eE") {
			return fmt.Errorf("want integer")
		}
	case NumberKind:
		if _, ok := l.decoded.(json.Number); !ok {
			return fmt.Errorf("want number")
		}
	case BooleanKind:
		if _, ok := l.decoded.(bool); !ok {
			return fmt.Errorf("want boolean")
		}
	case NullKind:
		if l.decoded != nil {
			return fmt.Errorf("want null")
		}
	case EnumKind:
		for _, value := range t.enum {
			if l.Equal(value) {
				return nil
			}
		}
		return fmt.Errorf("literal is not an enum member")
	case ObjectKind:
		object, ok := l.decoded.(map[string]any)
		if !ok {
			return fmt.Errorf("want object")
		}
		if len(object) != len(t.fields) {
			for key := range object {
				if _, ok := findField(t.fields, key); !ok {
					return fmt.Errorf("undeclared field %q", key)
				}
			}
		}
		for _, field := range t.fields {
			value, present := object[field.name]
			if !present {
				if field.optional {
					continue
				}
				return fmt.Errorf("required field %q is absent", field.name)
			}
			child, err := literalFromDecoded(value)
			if err != nil {
				return err
			}
			if err := validateLiteral(field.typ, child); err != nil {
				return fmt.Errorf("field %q: %w", field.name, err)
			}
		}
		for key := range object {
			if _, ok := findField(t.fields, key); !ok {
				return fmt.Errorf("undeclared field %q", key)
			}
		}
	case MapKind:
		object, ok := l.decoded.(map[string]any)
		if !ok {
			return fmt.Errorf("want map")
		}
		for key, value := range object {
			child, err := literalFromDecoded(value)
			if err != nil {
				return err
			}
			if err := validateLiteral(*t.elem, child); err != nil {
				return fmt.Errorf("map key %q: %w", key, err)
			}
		}
	case ListKind:
		list, ok := l.decoded.([]any)
		if !ok {
			return fmt.Errorf("want list")
		}
		for i, value := range list {
			child, err := literalFromDecoded(value)
			if err != nil {
				return err
			}
			if err := validateLiteral(*t.elem, child); err != nil {
				return fmt.Errorf("list item %d: %w", i, err)
			}
		}
	case FileKind, TreeKind:
		return fmt.Errorf("%v values cannot be compile-time literals", t.kind)
	case AnyKind:
		return nil
	default:
		return fmt.Errorf("invalid type")
	}
	return nil
}

func literalFromDecoded(value any) (Literal, error) {
	canonical, err := canonicalJSON(value)
	if err != nil {
		return Literal{}, err
	}
	return Literal{canonical: canonical, decoded: value}, nil
}

// ValidateLiteral checks that l is a closed object satisfying all contract ports.
func (c Contract) ValidateLiteral(l Literal) error {
	if !c.Valid() {
		return fmt.Errorf("invalid contract")
	}
	return c.ObjectType().ValidateLiteral(l)
}

// Assignment describes validation that must occur at a binding boundary.
type Assignment struct {
	RuntimeValidation bool
}

// CheckAssignable verifies whether a source type may bind to a destination type.
func CheckAssignable(from, to Type) (Assignment, error) {
	if !from.Valid() || !to.Valid() {
		return Assignment{}, fmt.Errorf("assignability requires valid types")
	}
	return checkAssignable(from, to)
}

func checkAssignable(from, to Type) (Assignment, error) {
	if from.Equal(to) {
		return Assignment{}, nil
	}
	if to.kind == AnyKind {
		if ordinaryType(from) {
			return Assignment{}, nil
		}
		return Assignment{}, fmt.Errorf("%v is not assignable to ordinary any", from.kind)
	}
	if from.kind == AnyKind {
		if ordinaryType(to) {
			return Assignment{RuntimeValidation: true}, nil
		}
		return Assignment{}, fmt.Errorf("ordinary any is not assignable to %v", to.kind)
	}
	if from.kind == IntegerKind && to.kind == NumberKind {
		return Assignment{}, nil
	}
	if from.kind == EnumKind && enumWidensTo(from, to.kind) {
		return Assignment{}, nil
	}
	switch {
	case from.kind == ListKind && to.kind == ListKind:
		return checkAssignable(*from.elem, *to.elem)
	case from.kind == MapKind && to.kind == MapKind:
		return checkAssignable(*from.elem, *to.elem)
	case from.kind == ObjectKind && to.kind == ObjectKind:
		return assignObject(from, to)
	case from.kind == FileKind && to.kind == FileKind:
		return assignFile(from, to)
	default:
		return Assignment{}, fmt.Errorf("%v is not assignable to %v", from.kind, to.kind)
	}
}

func ordinaryType(t Type) bool {
	switch t.kind {
	case StringKind, IntegerKind, NumberKind, BooleanKind, NullKind, EnumKind, AnyKind:
		return true
	case ObjectKind:
		for _, field := range t.fields {
			if !ordinaryType(field.typ) {
				return false
			}
		}
		return true
	case MapKind, ListKind:
		return ordinaryType(*t.elem)
	default:
		return false
	}
}

func enumWidensTo(enum Type, destination Kind) bool {
	if destination != StringKind && destination != IntegerKind && destination != NumberKind && destination != BooleanKind && destination != NullKind {
		return false
	}
	for _, value := range enum.enum {
		kind := literalKind(value)
		if kind == IntegerKind && destination == NumberKind {
			continue
		}
		if kind != destination {
			return false
		}
	}
	return true
}

func literalKind(l Literal) Kind {
	switch value := l.decoded.(type) {
	case nil:
		return NullKind
	case bool:
		return BooleanKind
	case string:
		return StringKind
	case json.Number:
		if strings.ContainsAny(value.String(), ".eE") {
			return NumberKind
		}
		return IntegerKind
	default:
		return InvalidKind
	}
}

func assignObject(from, to Type) (Assignment, error) {
	if len(from.fields) != len(to.fields) {
		return Assignment{}, fmt.Errorf("closed objects have different fields")
	}
	assignment := Assignment{}
	for i := range from.fields {
		source, destination := from.fields[i], to.fields[i]
		if source.name != destination.name {
			return Assignment{}, fmt.Errorf("closed objects have different fields")
		}
		if source.optional && !destination.optional {
			return Assignment{}, fmt.Errorf("optional field %q cannot satisfy a required field", source.name)
		}
		child, err := checkAssignable(source.typ, destination.typ)
		if err != nil {
			return Assignment{}, fmt.Errorf("field %q: %w", source.name, err)
		}
		assignment.RuntimeValidation = assignment.RuntimeValidation || child.RuntimeValidation
	}
	return assignment, nil
}

func assignFile(from, to Type) (Assignment, error) {
	if len(to.media) == 0 {
		return Assignment{}, nil
	}
	if len(from.media) == 0 {
		return Assignment{RuntimeValidation: true}, nil
	}
	allAccepted := true
	intersects := false
	for _, source := range from.media {
		accepted := false
		for _, destination := range to.media {
			if mediaContains(destination, source) {
				accepted = true
			}
			if mediaIntersects(source, destination) {
				intersects = true
			}
		}
		allAccepted = allAccepted && accepted
	}
	if allAccepted {
		return Assignment{}, nil
	}
	if !intersects {
		return Assignment{}, fmt.Errorf("file media constraints are disjoint")
	}
	return Assignment{RuntimeValidation: true}, nil
}

func mediaContains(destination, source string) bool {
	if destination == source {
		return true
	}
	destinationType, destinationSub := splitMedia(destination)
	sourceType, sourceSub := splitMedia(source)
	return destinationType == sourceType && destinationSub == "*" && sourceSub != ""
}

func mediaIntersects(left, right string) bool {
	leftType, leftSub := splitMedia(left)
	rightType, rightSub := splitMedia(right)
	return leftType == rightType && (leftSub == "*" || rightSub == "*" || leftSub == rightSub)
}

func splitMedia(media string) (string, string) {
	parts := strings.SplitN(media, "/", 2)
	return parts[0], parts[1]
}
