package value

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/valbaudo/dawn/content"
)

const canonicalValueHeader = "dawn.value/1"

type encodeItem struct {
	value Value
	name  string
	kind  uint8
}

const (
	encodeValue uint8 = iota
	encodeName
)

// Canonical returns a copied private canonical encoding, or nil for an invalid
// value. Encoding walks user-controlled nesting with an explicit stack.
func (v Value) Canonical() []byte {
	if !v.Valid() {
		return nil
	}
	out := append([]byte(nil), canonicalValueHeader...)
	stack := []encodeItem{{value: v, kind: encodeValue}}
	for len(stack) > 0 {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if item.kind == encodeName {
			out = binary.AppendUvarint(out, uint64(len(item.name)))
			out = append(out, item.name...)
			continue
		}
		current := item.value
		out = append(out, byte(current.kind))
		switch current.kind {
		case StringKind:
			out = appendLengthBytes(out, current.text)
		case IntegerKind, NumberKind:
			out = appendLengthBytes(out, current.number)
		case BooleanKind:
			if current.boolean {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		case NullKind:
		case ObjectKind, MapKind:
			out = binary.AppendUvarint(out, uint64(len(current.entries)))
			for i := len(current.entries) - 1; i >= 0; i-- {
				stack = append(stack,
					encodeItem{value: current.entries[i].value, kind: encodeValue},
					encodeItem{name: current.entries[i].name, kind: encodeName},
				)
			}
		case ListKind:
			out = binary.AppendUvarint(out, uint64(len(current.items)))
			for i := len(current.items) - 1; i >= 0; i-- {
				stack = append(stack, encodeItem{value: current.items[i], kind: encodeValue})
			}
		case FileKind:
			file := *current.file
			out = append(out, file.Digest().Bytes()...)
			out = binary.AppendUvarint(out, uint64(file.Size()))
			out = appendLengthBytes(out, file.Name())
			out = appendLengthBytes(out, file.Media())
		case TreeKind:
			out = append(out, current.tree.Digest().Bytes()...)
		}
	}
	return out
}

func appendLengthBytes(out []byte, value string) []byte {
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

// ParseCanonical parses exactly one canonical private runtime-value encoding.
// It rejects malformed representations instead of normalizing them.
func ParseCanonical(data []byte) (Value, error) {
	if len(data) < len(canonicalValueHeader) || !bytes.Equal(data[:len(canonicalValueHeader)], []byte(canonicalValueHeader)) {
		return Value{}, fmt.Errorf("value: invalid canonical header")
	}
	decoder := wireDecoder{data: data, offset: len(canonicalValueHeader)}
	completed, frame, err := decoder.readValueStart()
	if err != nil {
		return Value{}, err
	}
	frames := make([]decodeFrame, 0)
	if frame != nil {
		frames = append(frames, *frame)
		completed = Value{}
	}

	for {
		if shallowValidValue(completed) {
			if len(frames) == 0 {
				break
			}
			top := &frames[len(frames)-1]
			switch top.kind {
			case ListKind:
				top.items = append(top.items, completed)
			case ObjectKind, MapKind:
				top.entries = append(top.entries, Entry{name: top.name, value: completed})
				top.haveName = false
			}
			top.remaining--
			if top.remaining == 0 {
				switch top.kind {
				case ObjectKind, MapKind:
					completed = Value{kind: top.kind, entries: top.entries}
				case ListKind:
					completed = Value{kind: ListKind, items: top.items}
				}
				frames = frames[:len(frames)-1]
				continue
			}
			completed = Value{}
		}

		top := &frames[len(frames)-1]
		if top.kind == ObjectKind || top.kind == MapKind {
			name, err := decoder.readString()
			if err != nil {
				return Value{}, fmt.Errorf("value: read entry name: %w", err)
			}
			if top.kind == ObjectKind && name == "" {
				return Value{}, fmt.Errorf("value: empty object field")
			}
			if top.havePrevious && top.previous >= name {
				return Value{}, fmt.Errorf("value: entries are duplicate or unsorted")
			}
			top.name = name
			top.previous = name
			top.havePrevious = true
			top.haveName = true
		}
		child, childFrame, err := decoder.readValueStart()
		if err != nil {
			return Value{}, err
		}
		if childFrame != nil {
			frames = append(frames, *childFrame)
			completed = Value{}
		} else {
			completed = child
		}
	}
	if decoder.offset != len(data) {
		return Value{}, fmt.Errorf("value: trailing canonical data")
	}
	if !completed.Valid() {
		return Value{}, fmt.Errorf("value: decoded value is invalid")
	}
	return completed, nil
}

type decodeFrame struct {
	kind         Kind
	remaining    uint64
	entries      []Entry
	items        []Value
	name         string
	haveName     bool
	previous     string
	havePrevious bool
}

type wireDecoder struct {
	data   []byte
	offset int
}

func (d *wireDecoder) readValueStart() (Value, *decodeFrame, error) {
	tag, err := d.readByte()
	if err != nil {
		return Value{}, nil, fmt.Errorf("value: read tag: %w", err)
	}
	kind := Kind(tag)
	switch kind {
	case StringKind:
		text, err := d.readString()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read string: %w", err)
		}
		return NewString(text), nil, nil
	case IntegerKind, NumberKind:
		number, err := d.readString()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read number: %w", err)
		}
		var value Value
		if kind == IntegerKind {
			value, err = NewInteger(number)
		} else {
			value, err = NewNumber(number)
		}
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: %w", err)
		}
		return value, nil, nil
	case BooleanKind:
		encoded, err := d.readByte()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read boolean: %w", err)
		}
		if encoded > 1 {
			return Value{}, nil, fmt.Errorf("value: invalid boolean byte %d", encoded)
		}
		return NewBoolean(encoded == 1), nil, nil
	case NullKind:
		return NewNull(), nil, nil
	case ObjectKind, MapKind, ListKind:
		count, err := d.readUvarint()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read collection length: %w", err)
		}
		if count == 0 {
			return Value{kind: kind}, nil, nil
		}
		return Value{}, &decodeFrame{kind: kind, remaining: count}, nil
	case FileKind:
		digest, err := d.readDigest()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read file digest: %w", err)
		}
		size, err := d.readUvarint()
		if err != nil || size > math.MaxInt64 {
			return Value{}, nil, fmt.Errorf("value: invalid file length")
		}
		name, err := d.readString()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read file name: %w", err)
		}
		media, err := d.readString()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read file media: %w", err)
		}
		file, err := content.NewFile(digest, int64(size), name, media)
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: invalid file: %w", err)
		}
		return NewFileValue(file), nil, nil
	case TreeKind:
		digest, err := d.readDigest()
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: read tree digest: %w", err)
		}
		tree, err := content.NewTree(digest)
		if err != nil {
			return Value{}, nil, fmt.Errorf("value: invalid tree: %w", err)
		}
		return NewTreeValue(tree), nil, nil
	default:
		return Value{}, nil, fmt.Errorf("value: unknown tag %d", tag)
	}
}

func (d *wireDecoder) readByte() (byte, error) {
	if d.offset >= len(d.data) {
		return 0, fmt.Errorf("unexpected end of encoding")
	}
	value := d.data[d.offset]
	d.offset++
	return value, nil
}

func (d *wireDecoder) readUvarint() (uint64, error) {
	if d.offset >= len(d.data) {
		return 0, fmt.Errorf("unexpected end of encoding")
	}
	start := d.offset
	value, count := binary.Uvarint(d.data[start:])
	if count == 0 {
		return 0, fmt.Errorf("truncated unsigned length")
	}
	if count < 0 {
		return 0, fmt.Errorf("overflowing unsigned length")
	}
	d.offset += count
	var canonical [binary.MaxVarintLen64]byte
	canonicalCount := binary.PutUvarint(canonical[:], value)
	if canonicalCount != count || !bytes.Equal(canonical[:canonicalCount], d.data[start:d.offset]) {
		return 0, fmt.Errorf("noncanonical unsigned length")
	}
	return value, nil
}

func (d *wireDecoder) readString() (string, error) {
	length, err := d.readUvarint()
	if err != nil {
		return "", err
	}
	remaining := len(d.data) - d.offset
	if length > uint64(remaining) {
		return "", fmt.Errorf("length %d exceeds remaining encoding", length)
	}
	end := d.offset + int(length)
	value := string(d.data[d.offset:end])
	d.offset = end
	return value, nil
}

func (d *wireDecoder) readDigest() (content.Digest, error) {
	const digestLength = 32
	if len(d.data)-d.offset < digestLength {
		return content.Digest{}, fmt.Errorf("truncated digest")
	}
	var digest content.Digest
	copy(digest[:], d.data[d.offset:d.offset+digestLength])
	d.offset += digestLength
	return digest, nil
}
