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
	segments []Segment
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
	segments := make([]Segment, len(p.segments), len(p.segments)+1)
	copy(segments, p.segments)
	segments = append(segments, segment)
	return Path{segments: segments}
}

// Segments returns a copy of the structured path.
func (p Path) Segments() []Segment { return append([]Segment(nil), p.segments...) }

// String renders an escaped diagnostic. The rendering is not path identity or
// a filesystem interpretation.
func (p Path) String() string {
	var out strings.Builder
	out.WriteByte('$')
	for _, segment := range p.segments {
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
