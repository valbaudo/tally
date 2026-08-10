package value

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// Literal is one canonical ordinary JSON value.
type Literal struct {
	canonical []byte
	decoded   any
}

// ParseLiteral parses exactly one ordinary JSON value into canonical form.
func ParseLiteral(data []byte) (Literal, error) {
	if err := validateLiteralLexically(data); err != nil {
		return Literal{}, err
	}
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

func validateLiteralLexically(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("literal contains invalid UTF-8")
	}
	inString := false
	for i := 0; i < len(data); i++ {
		if !inString {
			if data[i] == '"' {
				inString = true
			}
			continue
		}
		switch data[i] {
		case '"':
			inString = false
		case '\\':
			i++
			if i == len(data) {
				return fmt.Errorf("literal ends in an escape")
			}
			if data[i] != 'u' {
				continue
			}
			unit, ok := unicodeEscapeUnit(data, i+1)
			if !ok {
				return fmt.Errorf("literal contains an invalid Unicode escape")
			}
			switch {
			case unit >= 0xD800 && unit <= 0xDBFF:
				if i+10 >= len(data) || data[i+5] != '\\' || data[i+6] != 'u' {
					return fmt.Errorf("literal contains an unpaired high surrogate")
				}
				low, ok := unicodeEscapeUnit(data, i+7)
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return fmt.Errorf("literal contains an unpaired high surrogate")
				}
				i += 10
			case unit >= 0xDC00 && unit <= 0xDFFF:
				return fmt.Errorf("literal contains an unpaired low surrogate")
			default:
				i += 4
			}
		}
	}
	return nil
}

func unicodeEscapeUnit(data []byte, start int) (rune, bool) {
	if start+4 > len(data) {
		return 0, false
	}
	var unit rune
	for _, digit := range data[start : start+4] {
		unit <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			unit += rune(digit - '0')
		case digit >= 'a' && digit <= 'f':
			unit += rune(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			unit += rune(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return unit, true
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
		keys := sortedMapKeys(value)
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

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
	type literalPath struct {
		parent *literalPath
		kind   Kind
		name   string
		index  int
	}
	const (
		validateLiteralTask uint8 = iota
		requiredLiteralTask
	)
	type literalTask struct {
		typ     Type
		decoded any
		path    *literalPath
		mode    uint8
		problem string
	}
	wrap := func(path *literalPath, err error) error {
		for path != nil {
			switch path.kind {
			case ObjectKind:
				err = fmt.Errorf("field %q: %w", path.name, err)
			case MapKind:
				err = fmt.Errorf("map key %q: %w", path.name, err)
			case ListKind:
				err = fmt.Errorf("list item %d: %w", path.index, err)
			}
			path = path.parent
		}
		return err
	}

	tasks := []literalTask{{typ: t, decoded: l.decoded}}
	for len(tasks) != 0 {
		task := tasks[len(tasks)-1]
		tasks = tasks[:len(tasks)-1]
		if task.problem != "" {
			return wrap(task.path, errors.New(task.problem))
		}
		if task.mode == requiredLiteralTask {
			object := task.decoded.(map[string]any)
			for _, field := range task.typ.fields {
				if _, present := object[field.name]; !present && !field.optional {
					return wrap(task.path, fmt.Errorf("required field %q is absent", field.name))
				}
			}
			continue
		}

		switch task.typ.kind {
		case StringKind:
			if _, ok := task.decoded.(string); !ok {
				return wrap(task.path, fmt.Errorf("want string"))
			}
		case IntegerKind:
			number, ok := task.decoded.(json.Number)
			if !ok || strings.ContainsAny(number.String(), ".eE") {
				return wrap(task.path, fmt.Errorf("want integer"))
			}
		case NumberKind:
			if _, ok := task.decoded.(json.Number); !ok {
				return wrap(task.path, fmt.Errorf("want number"))
			}
		case BooleanKind:
			if _, ok := task.decoded.(bool); !ok {
				return wrap(task.path, fmt.Errorf("want boolean"))
			}
		case NullKind:
			if task.decoded != nil {
				return wrap(task.path, fmt.Errorf("want null"))
			}
		case EnumKind:
			canonical, err := canonicalJSON(task.decoded)
			if err != nil {
				return wrap(task.path, err)
			}
			matched := false
			for _, member := range task.typ.enum {
				if bytes.Equal(canonical, member.canonical) {
					matched = true
					break
				}
			}
			if !matched {
				return wrap(task.path, fmt.Errorf("literal is not an enum member"))
			}
		case ObjectKind:
			object, ok := task.decoded.(map[string]any)
			if !ok {
				return wrap(task.path, fmt.Errorf("want object"))
			}
			tasks = append(tasks, literalTask{typ: task.typ, decoded: object, path: task.path, mode: requiredLiteralTask})
			keys := sortedMapKeys(object)
			for index := len(keys) - 1; index >= 0; index-- {
				key := keys[index]
				fieldType, declared := findField(task.typ.fields, key)
				if !declared {
					tasks = append(tasks, literalTask{path: task.path, problem: fmt.Sprintf("undeclared field %q", key)})
					continue
				}
				tasks = append(tasks, literalTask{
					typ: fieldType, decoded: object[key],
					path: &literalPath{parent: task.path, kind: ObjectKind, name: key}, mode: validateLiteralTask,
				})
			}
		case MapKind:
			object, ok := task.decoded.(map[string]any)
			if !ok {
				return wrap(task.path, fmt.Errorf("want map"))
			}
			keys := sortedMapKeys(object)
			for index := len(keys) - 1; index >= 0; index-- {
				key := keys[index]
				tasks = append(tasks, literalTask{
					typ: *task.typ.elem, decoded: object[key],
					path: &literalPath{parent: task.path, kind: MapKind, name: key}, mode: validateLiteralTask,
				})
			}
		case ListKind:
			list, ok := task.decoded.([]any)
			if !ok {
				return wrap(task.path, fmt.Errorf("want list"))
			}
			for index := len(list) - 1; index >= 0; index-- {
				tasks = append(tasks, literalTask{
					typ: *task.typ.elem, decoded: list[index],
					path: &literalPath{parent: task.path, kind: ListKind, index: index}, mode: validateLiteralTask,
				})
			}
		case FileKind, TreeKind:
			return wrap(task.path, fmt.Errorf("%v values cannot be compile-time literals", task.typ.kind))
		case AnyKind:
		default:
			return wrap(task.path, fmt.Errorf("invalid type"))
		}
	}
	return nil
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
	type assignmentPath struct {
		parent *assignmentPath
		field  string
	}
	type assignmentTask struct {
		from, to Type
		path     *assignmentPath
	}
	fail := func(path *assignmentPath, err error) (Assignment, error) {
		for path != nil {
			err = fmt.Errorf("field %q: %w", path.field, err)
			path = path.parent
		}
		return Assignment{}, err
	}

	assignment := Assignment{}
	tasks := []assignmentTask{{from: from, to: to}}
	for len(tasks) != 0 {
		task := tasks[len(tasks)-1]
		tasks = tasks[:len(tasks)-1]
		source, destination := task.from, task.to

		if destination.kind == AnyKind && source.kind != AnyKind {
			if !ordinaryType(source) {
				return fail(task.path, fmt.Errorf("%v is not assignable to ordinary any", source.kind))
			}
			continue
		}
		if source.kind == AnyKind && destination.kind != AnyKind {
			if !ordinaryType(destination) {
				return fail(task.path, fmt.Errorf("ordinary any is not assignable to %v", destination.kind))
			}
			assignment.RuntimeValidation = true
			continue
		}
		if source.kind == IntegerKind && destination.kind == NumberKind {
			continue
		}
		if source.kind == EnumKind && destination.kind != EnumKind && enumWidensTo(source, destination.kind) {
			continue
		}
		if source.kind != destination.kind {
			return fail(task.path, fmt.Errorf("%v is not assignable to %v", source.kind, destination.kind))
		}

		switch source.kind {
		case StringKind, IntegerKind, NumberKind, BooleanKind, NullKind, TreeKind, AnyKind:
		case EnumKind:
			if len(source.enum) != len(destination.enum) {
				return fail(task.path, fmt.Errorf("%v is not assignable to %v", source.kind, destination.kind))
			}
			for index := range source.enum {
				if !source.enum[index].Equal(destination.enum[index]) {
					return fail(task.path, fmt.Errorf("%v is not assignable to %v", source.kind, destination.kind))
				}
			}
		case ListKind, MapKind:
			tasks = append(tasks, assignmentTask{from: *source.elem, to: *destination.elem, path: task.path})
		case ObjectKind:
			if len(source.fields) != len(destination.fields) {
				return fail(task.path, fmt.Errorf("closed objects have different fields"))
			}
			for index := len(source.fields) - 1; index >= 0; index-- {
				sourceField, destinationField := source.fields[index], destination.fields[index]
				if sourceField.name != destinationField.name {
					return fail(task.path, fmt.Errorf("closed objects have different fields"))
				}
				childPath := &assignmentPath{parent: task.path, field: sourceField.name}
				if sourceField.optional && !destinationField.optional {
					return fail(childPath, fmt.Errorf("optional field cannot satisfy a required field"))
				}
				tasks = append(tasks, assignmentTask{from: sourceField.typ, to: destinationField.typ, path: childPath})
			}
		case FileKind:
			child, err := assignFile(source, destination)
			if err != nil {
				return fail(task.path, err)
			}
			assignment.RuntimeValidation = assignment.RuntimeValidation || child.RuntimeValidation
		default:
			return fail(task.path, fmt.Errorf("%v is not assignable to %v", source.kind, destination.kind))
		}
	}
	return assignment, nil
}

func ordinaryType(t Type) bool {
	stack := []Type{t}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch current.kind {
		case StringKind, IntegerKind, NumberKind, BooleanKind, NullKind, EnumKind, AnyKind:
		case ObjectKind:
			for _, field := range current.fields {
				stack = append(stack, field.typ)
			}
		case MapKind, ListKind:
			stack = append(stack, *current.elem)
		default:
			return false
		}
	}
	return true
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
