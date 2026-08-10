package content

import (
	"encoding/binary"
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
)

const treeManifestHeader = "dawn.tree/1"

type treeKind uint8

const (
	directoryEntry treeKind = iota + 1
	fileEntry
	executableEntry
	symlinkEntry
)

type treeEntry struct {
	segments []string
	kind     treeKind
	content  Object
	target   string
}

func encodeTreeManifest(entries []treeEntry) ([]byte, error) {
	if err := validateTreeEntries(entries); err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, len(treeManifestHeader)+8+len(entries)*17)
	encoded = append(encoded, treeManifestHeader...)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(entries)))
	for _, entry := range entries {
		encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(entry.segments)))
		for _, segment := range entry.segments {
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(segment)))
			encoded = append(encoded, segment...)
		}
		encoded = append(encoded, byte(entry.kind))
		switch entry.kind {
		case fileEntry, executableEntry:
			encoded = append(encoded, entry.content.digest[:]...)
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(entry.content.size))
		case symlinkEntry:
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(entry.target)))
			encoded = append(encoded, entry.target...)
		}
	}
	return encoded, nil
}

func decodeTreeManifest(encoded []byte) ([]treeEntry, error) {
	decoder := treeDecoder{data: encoded}
	header, err := decoder.bytes(uint64(len(treeManifestHeader)), "header")
	if err != nil || string(header) != treeManifestHeader {
		return nil, fmt.Errorf("content: invalid tree manifest header")
	}
	count, err := decoder.uint64("entry count")
	if err != nil {
		return nil, err
	}
	if count > uint64(decoder.remaining()) {
		return nil, fmt.Errorf("content: invalid tree manifest entry count")
	}
	entries := make([]treeEntry, 0, minUint64Int(count, 1024))
	for index := uint64(0); index < count; index++ {
		segmentCount, err := decoder.uint64("segment count")
		if err != nil {
			return nil, err
		}
		if segmentCount > uint64(decoder.remaining()/8) {
			return nil, fmt.Errorf("content: invalid tree manifest segment count")
		}
		segments := make([]string, 0, minUint64Int(segmentCount, 32))
		for segmentIndex := uint64(0); segmentIndex < segmentCount; segmentIndex++ {
			length, err := decoder.uint64("segment length")
			if err != nil {
				return nil, err
			}
			segment, err := decoder.bytes(length, "segment")
			if err != nil {
				return nil, err
			}
			segments = append(segments, string(segment))
		}
		kindByte, err := decoder.byte("entry kind")
		if err != nil {
			return nil, err
		}
		entry := treeEntry{segments: segments, kind: treeKind(kindByte)}
		switch entry.kind {
		case directoryEntry:
		case fileEntry, executableEntry:
			digestBytes, err := decoder.bytes(uint64(len(Digest{})), "content digest")
			if err != nil {
				return nil, err
			}
			copy(entry.content.digest[:], digestBytes)
			size, err := decoder.uint64("content size")
			if err != nil {
				return nil, err
			}
			if size > math.MaxInt64 {
				return nil, fmt.Errorf("content: invalid tree manifest content size")
			}
			entry.content.size = int64(size)
		case symlinkEntry:
			length, err := decoder.uint64("symlink target length")
			if err != nil {
				return nil, err
			}
			target, err := decoder.bytes(length, "symlink target")
			if err != nil {
				return nil, err
			}
			entry.target = string(target)
		default:
			return nil, fmt.Errorf("content: invalid tree manifest entry kind %d", kindByte)
		}
		entries = append(entries, entry)
	}
	if decoder.remaining() != 0 {
		return nil, fmt.Errorf("content: invalid tree manifest trailing data")
	}
	if err := validateTreeEntries(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

type treeDecoder struct {
	data   []byte
	offset int
}

func (d *treeDecoder) remaining() int {
	return len(d.data) - d.offset
}

func (d *treeDecoder) byte(field string) (byte, error) {
	data, err := d.bytes(1, field)
	if err != nil {
		return 0, err
	}
	return data[0], nil
}

func (d *treeDecoder) uint64(field string) (uint64, error) {
	data, err := d.bytes(8, field)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(data), nil
}

func (d *treeDecoder) bytes(length uint64, field string) ([]byte, error) {
	if length > uint64(d.remaining()) {
		return nil, fmt.Errorf("content: truncated tree manifest %s", field)
	}
	end := d.offset + int(length)
	data := d.data[d.offset:end]
	d.offset = end
	return data, nil
}

func minUint64Int(value uint64, maximum int) int {
	if value < uint64(maximum) {
		return int(value)
	}
	return maximum
}

func validateTreeEntries(entries []treeEntry) error {
	byPath := make(map[string]int, len(entries))
	for index, entry := range entries {
		if len(entry.segments) == 0 {
			return fmt.Errorf("content: invalid tree manifest empty entry path")
		}
		for _, segment := range entry.segments {
			if segment == "" || segment == "." || segment == ".." {
				return fmt.Errorf("content: invalid tree manifest path segment")
			}
		}
		if index > 0 && compareTreeSegments(entries[index-1].segments, entry.segments) >= 0 {
			return fmt.Errorf("content: tree manifest entries are not in canonical order")
		}
		switch entry.kind {
		case directoryEntry:
			if entry.content != (Object{}) || entry.target != "" {
				return fmt.Errorf("content: invalid tree manifest directory payload")
			}
		case fileEntry, executableEntry:
			if !entry.content.Valid() || entry.target != "" {
				return fmt.Errorf("content: invalid tree manifest file payload")
			}
		case symlinkEntry:
			if entry.content != (Object{}) || entry.target == "" || treeTargetIsAbsolute(entry.target) {
				return fmt.Errorf("content: invalid tree manifest symlink payload")
			}
		default:
			return fmt.Errorf("content: invalid tree manifest entry kind %d", entry.kind)
		}
		key := treeSegmentsKey(entry.segments)
		if _, exists := byPath[key]; exists {
			return fmt.Errorf("content: duplicate tree manifest entry")
		}
		byPath[key] = index
	}
	for _, entry := range entries {
		if len(entry.segments) == 1 {
			continue
		}
		parentIndex, exists := byPath[treeSegmentsKey(entry.segments[:len(entry.segments)-1])]
		if !exists || entries[parentIndex].kind != directoryEntry {
			return fmt.Errorf("content: tree manifest entry has no directory parent")
		}
	}
	if err := validateTreeSymlinks(entries, byPath); err != nil {
		return err
	}
	return nil
}

func treeTargetIsAbsolute(target string) bool {
	if path.IsAbs(target) || strings.HasPrefix(target, `\`) {
		return true
	}
	return len(target) >= 2 && ((target[0] >= 'A' && target[0] <= 'Z') || (target[0] >= 'a' && target[0] <= 'z')) && target[1] == ':'
}

func compareTreeSegments(a, b []string) int {
	for index := 0; index < min(len(a), len(b)); index++ {
		if comparison := strings.Compare(a[index], b[index]); comparison != 0 {
			return comparison
		}
	}
	return len(a) - len(b)
}

func treeSegmentsKey(segments []string) string {
	encoded := make([]byte, 0, len(segments)*8)
	for _, segment := range segments {
		encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(segment)))
		encoded = append(encoded, segment...)
	}
	return string(encoded)
}

type symlinkResolutionFrame struct {
	index            int
	resolved         []string
	parts            []string
	position         int
	waiting          int
	requireDirectory bool
}

func validateTreeSymlinks(entries []treeEntry, byPath map[string]int) error {
	const (
		unvisited uint8 = iota
		visiting
		resolved
	)
	states := make([]uint8, len(entries))
	resolutions := make([][]string, len(entries))
	for rootIndex, root := range entries {
		if root.kind != symlinkEntry || states[rootIndex] == resolved {
			continue
		}
		states[rootIndex] = visiting
		stack := []symlinkResolutionFrame{newSymlinkResolutionFrame(rootIndex, root)}
		for len(stack) != 0 {
			frame := &stack[len(stack)-1]
			if frame.waiting >= 0 {
				frame.resolved = slices.Clone(resolutions[frame.waiting])
				frame.waiting = -1
				continue
			}
			if frame.position == len(frame.parts) {
				if frame.requireDirectory && !treePathIsDirectory(entries, byPath, frame.resolved) {
					return fmt.Errorf("content: tree symlink target does not resolve to a directory")
				}
				resolutions[frame.index] = slices.Clone(frame.resolved)
				states[frame.index] = resolved
				stack = stack[:len(stack)-1]
				continue
			}
			part := frame.parts[frame.position]
			frame.position++
			if part == "" {
				continue
			}
			if part == "." {
				if !treePathIsDirectory(entries, byPath, frame.resolved) {
					return fmt.Errorf("content: tree symlink traverses a non-directory")
				}
				continue
			}
			if part == ".." {
				if len(frame.resolved) == 0 {
					return fmt.Errorf("content: tree symlink target escapes the root")
				}
				if !treePathIsDirectory(entries, byPath, frame.resolved) {
					return fmt.Errorf("content: tree symlink traverses a non-directory")
				}
				frame.resolved = frame.resolved[:len(frame.resolved)-1]
				continue
			}
			if !treePathIsDirectory(entries, byPath, frame.resolved) {
				return fmt.Errorf("content: tree symlink traverses a non-directory")
			}
			candidate := append(slices.Clone(frame.resolved), part)
			entryIndex, exists := byPath[treeSegmentsKey(candidate)]
			if !exists {
				return fmt.Errorf("content: tree symlink target does not resolve")
			}
			entry := entries[entryIndex]
			if entry.kind != symlinkEntry {
				frame.resolved = candidate
				continue
			}
			switch states[entryIndex] {
			case visiting:
				return fmt.Errorf("content: tree symlink cycle does not resolve")
			case resolved:
				frame.resolved = slices.Clone(resolutions[entryIndex])
			default:
				states[entryIndex] = visiting
				frame.waiting = entryIndex
				stack = append(stack, newSymlinkResolutionFrame(entryIndex, entry))
			}
		}
	}
	return nil
}

func newSymlinkResolutionFrame(index int, entry treeEntry) symlinkResolutionFrame {
	return symlinkResolutionFrame{
		index:            index,
		resolved:         slices.Clone(entry.segments[:len(entry.segments)-1]),
		parts:            strings.Split(entry.target, "/"),
		waiting:          -1,
		requireDirectory: strings.HasSuffix(entry.target, "/"),
	}
}

func treePathIsDirectory(entries []treeEntry, byPath map[string]int, segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	index, exists := byPath[treeSegmentsKey(segments)]
	return exists && entries[index].kind == directoryEntry
}
