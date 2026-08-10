package value

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaterializeLiteral turns an ordinary literal into the runtime value selected
// by target. Objects require a target because JSON does not distinguish closed
// objects from maps.
func MaterializeLiteral(literal Literal, target Type) (Value, error) {
	if !target.Valid() || !literal.valid() {
		return Value{}, fmt.Errorf("materialize literal: invalid literal or target")
	}
	if err := target.ValidateLiteral(literal); err != nil {
		return Value{}, fmt.Errorf("materialize literal: %w", err)
	}
	return materializeDecoded(target, literal.decoded)
}

type materializeTask struct {
	typ      Type
	decoded  any
	assemble bool
	names    []string
}

// materializeDecoded uses explicit tasks and completed values so that literal
// depth does not consume the Go call stack.
func materializeDecoded(target Type, decoded any) (Value, error) {
	tasks := []materializeTask{{typ: target, decoded: decoded}}
	values := make([]Value, 0, 1)

	for len(tasks) > 0 {
		task := tasks[len(tasks)-1]
		tasks = tasks[:len(tasks)-1]

		if task.assemble {
			count := len(task.names)
			start := len(values) - count
			if start < 0 {
				return Value{}, fmt.Errorf("materialize literal: incomplete %v", task.typ.kind)
			}
			children := values[start:]
			values = values[:start]

			switch task.typ.kind {
			case ListKind:
				values = append(values, NewList(children...))
			case ObjectKind, MapKind:
				entries := make([]Entry, count)
				for index, name := range task.names {
					entry, err := NewEntry(name, children[index])
					if err != nil {
						return Value{}, fmt.Errorf("materialize literal: %w", err)
					}
					entries[index] = entry
				}
				var (
					value Value
					err   error
				)
				if task.typ.kind == ObjectKind {
					value, err = NewObject(entries...)
				} else {
					value, err = NewMap(entries...)
				}
				if err != nil {
					return Value{}, fmt.Errorf("materialize literal: %w", err)
				}
				values = append(values, value)
			default:
				return Value{}, fmt.Errorf("materialize literal: cannot assemble %v", task.typ.kind)
			}
			continue
		}

		switch task.typ.kind {
		case StringKind, IntegerKind, NumberKind, BooleanKind, NullKind, EnumKind:
			value, err := materializeScalar(task.decoded)
			if err != nil {
				return Value{}, err
			}
			values = append(values, value)
		case ObjectKind:
			object, ok := task.decoded.(map[string]any)
			if !ok {
				return Value{}, fmt.Errorf("materialize literal: want object")
			}
			names := make([]string, 0, len(object))
			fields := make([]Field, 0, len(object))
			for _, field := range task.typ.fields {
				if _, present := object[field.name]; present {
					names = append(names, field.name)
					fields = append(fields, field)
				}
			}
			tasks = append(tasks, materializeTask{typ: task.typ, assemble: true, names: names})
			for index := len(fields) - 1; index >= 0; index-- {
				field := fields[index]
				tasks = append(tasks, materializeTask{typ: field.typ, decoded: object[field.name]})
			}
		case MapKind, AnyKind:
			if object, ok := task.decoded.(map[string]any); ok {
				keys := sortedMapKeys(object)
				mapType := task.typ
				if task.typ.kind == AnyKind {
					mapType = Type{kind: MapKind}
				}
				tasks = append(tasks, materializeTask{typ: mapType, assemble: true, names: keys})
				for index := len(keys) - 1; index >= 0; index-- {
					key := keys[index]
					childType := Any()
					if task.typ.kind == MapKind {
						childType = *task.typ.elem
					}
					tasks = append(tasks, materializeTask{typ: childType, decoded: object[key]})
				}
				continue
			}
			if list, ok := task.decoded.([]any); ok && task.typ.kind == AnyKind {
				listType := Type{kind: ListKind}
				names := make([]string, len(list))
				tasks = append(tasks, materializeTask{typ: listType, assemble: true, names: names})
				for index := len(list) - 1; index >= 0; index-- {
					tasks = append(tasks, materializeTask{typ: Any(), decoded: list[index]})
				}
				continue
			}
			if task.typ.kind == MapKind {
				return Value{}, fmt.Errorf("materialize literal: want map")
			}
			value, err := materializeScalar(task.decoded)
			if err != nil {
				return Value{}, err
			}
			values = append(values, value)
		case ListKind:
			list, ok := task.decoded.([]any)
			if !ok {
				return Value{}, fmt.Errorf("materialize literal: want list")
			}
			names := make([]string, len(list))
			tasks = append(tasks, materializeTask{typ: task.typ, assemble: true, names: names})
			for index := len(list) - 1; index >= 0; index-- {
				tasks = append(tasks, materializeTask{typ: *task.typ.elem, decoded: list[index]})
			}
		case FileKind, TreeKind:
			return Value{}, fmt.Errorf("materialize literal: %v values cannot be compile-time literals", task.typ.kind)
		default:
			return Value{}, fmt.Errorf("materialize literal: invalid target")
		}
	}

	if len(values) != 1 {
		return Value{}, fmt.Errorf("materialize literal: incomplete result")
	}
	return values[0], nil
}

func materializeScalar(decoded any) (Value, error) {
	switch decoded := decoded.(type) {
	case nil:
		return NewNull(), nil
	case bool:
		return NewBoolean(decoded), nil
	case string:
		return NewString(decoded), nil
	case json.Number:
		if strings.ContainsAny(decoded.String(), ".eE") {
			value, err := NewNumber(decoded.String())
			if err != nil {
				return Value{}, fmt.Errorf("materialize literal: %w", err)
			}
			return value, nil
		}
		value, err := NewInteger(decoded.String())
		if err != nil {
			return Value{}, fmt.Errorf("materialize literal: %w", err)
		}
		return value, nil
	default:
		return Value{}, fmt.Errorf("materialize literal: want scalar")
	}
}
