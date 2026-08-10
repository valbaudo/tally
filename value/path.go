package value

import (
	"strconv"
	"strings"
)

// SegmentKind identifies one structured value-path step.
type SegmentKind uint8

const (
	FieldSegment SegmentKind = iota + 1
	MapKeySegment
	ListIndexSegment
)

// Segment is one tagged step through a recursive value.
type Segment struct {
	kind  SegmentKind
	name  string
	index int
}

// Kind returns the segment's tag.
func (s Segment) Kind() SegmentKind { return s.kind }

// Name returns the exact field name or map key bytes.
func (s Segment) Name() (string, bool) {
	if s.kind != FieldSegment && s.kind != MapKeySegment {
		return "", false
	}
	return s.name, true
}

// Index returns the list index carried by this segment.
func (s Segment) Index() (int, bool) {
	if s.kind != ListIndexSegment {
		return 0, false
	}
	return s.index, true
}

// Path is an immutable sequence of tagged value segments. The zero Path is
// the root.
type Path struct {
	tail   *pathNode
	length int
}

type pathNode struct {
	parent  *pathNode
	segment Segment
}

// Field returns a copied path extended by an object-field segment.
func (p Path) Field(name string) Path {
	return p.append(Segment{kind: FieldSegment, name: name})
}

// MapKey returns a copied path extended by a map-key segment.
func (p Path) MapKey(key string) Path {
	return p.append(Segment{kind: MapKeySegment, name: key})
}

// ListIndex returns a copied path extended by a list-index segment.
func (p Path) ListIndex(index int) Path {
	return p.append(Segment{kind: ListIndexSegment, index: index})
}

func (p Path) append(segment Segment) Path {
	return Path{tail: &pathNode{parent: p.tail, segment: segment}, length: p.length + 1}
}

// Segments returns a copy of the structured path.
func (p Path) Segments() []Segment {
	segments := make([]Segment, p.length)
	node := p.tail
	for index := len(segments) - 1; index >= 0; index-- {
		segments[index] = node.segment
		node = node.parent
	}
	return segments
}

func pathFromSegments(segments []Segment) Path {
	var path Path
	for _, segment := range segments {
		path = path.append(segment)
	}
	return path
}

// String renders an escaped diagnostic. The rendering is not path identity or
// a filesystem interpretation.
func (p Path) String() string {
	var out strings.Builder
	out.WriteByte('$')
	for _, segment := range p.Segments() {
		switch segment.kind {
		case FieldSegment:
			out.WriteString(".field(")
			out.WriteString(strconv.Quote(segment.name))
			out.WriteByte(')')
		case MapKeySegment:
			out.WriteString(".key(")
			out.WriteString(strconv.Quote(segment.name))
			out.WriteByte(')')
		case ListIndexSegment:
			out.WriteByte('[')
			out.WriteString(strconv.Itoa(segment.index))
			out.WriteByte(']')
		default:
			out.WriteString(".invalid")
		}
	}
	return out.String()
}
