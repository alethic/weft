package eval

import (
	"fmt"
	"strconv"
	"strings"

	"go.starlark.net/starlark"
)

// segment is one step of a field path: a map key or a list index.
type segment struct {
	key   string
	index int
	isIdx bool
}

func (s segment) String() string {
	if s.isIdx {
		return "[" + strconv.Itoa(s.index) + "]"
	}
	return s.key
}

// parsePath parses a field path.
//
// Dotted syntax covers the common case (status.atProvider.principalId) and
// bracket syntax covers the keys that cannot be identifiers, which in practice
// means annotations:
//
//	metadata.annotations["crossplane.io/external-name"]
//
// That key is not an edge case. It is written back asynchronously by the
// provider and is one of the handful of fields these compositions actually
// read, so it has to be expressible without ceremony.
func parsePath(path string) ([]segment, error) {
	if path == "" {
		return nil, fmt.Errorf("empty path")
	}
	var segs []segment
	i := 0
	expectSep := false
	for i < len(path) {
		switch {
		case path[i] == '.':
			if !expectSep {
				return nil, fmt.Errorf("unexpected %q at position %d in path %q", ".", i, path)
			}
			i++
			expectSep = false
			if i == len(path) {
				return nil, fmt.Errorf("path %q ends with a separator", path)
			}
		case path[i] == '[':
			s, n, err := parseBracket(path[i:])
			if err != nil {
				return nil, fmt.Errorf("in path %q: %w", path, err)
			}
			segs = append(segs, s)
			i += n
			expectSep = true
		default:
			if expectSep {
				return nil, fmt.Errorf("missing separator at position %d in path %q", i, path)
			}
			j := strings.IndexAny(path[i:], ".[")
			if j < 0 {
				j = len(path) - i
			}
			segs = append(segs, segment{key: path[i : i+j]})
			i += j
			expectSep = true
		}
	}
	if !expectSep {
		return nil, fmt.Errorf("path %q ends with a separator", path)
	}
	return segs, nil
}

// parseBracket parses a leading [...] and returns the segment and its length.
func parseBracket(s string) (segment, int, error) {
	if len(s) < 2 {
		return segment{}, 0, fmt.Errorf("unterminated %q", "[")
	}
	switch s[1] {
	case '"', '\'':
		quote := s[1]
		end := strings.IndexByte(s[2:], quote)
		if end < 0 {
			return segment{}, 0, fmt.Errorf("unterminated quoted key")
		}
		key := s[2 : 2+end]
		rest := 2 + end + 1
		if rest >= len(s) || s[rest] != ']' {
			return segment{}, 0, fmt.Errorf("expected %q after quoted key %q", "]", key)
		}
		return segment{key: key}, rest + 1, nil
	default:
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return segment{}, 0, fmt.Errorf("unterminated %q", "[")
		}
		body := s[1:end]
		n, err := strconv.Atoi(body)
		if err != nil {
			return segment{}, 0, fmt.Errorf("index %q is not a number; quote it if it is a key", body)
		}
		return segment{index: n, isIdx: true}, end + 1, nil
	}
}

// pathFromValue accepts either a path string or an explicit sequence of
// segments. The sequence form exists so a program can build a path from data
// without worrying about quoting.
func pathFromValue(v starlark.Value) ([]segment, error) {
	if s, ok := starlark.AsString(v); ok {
		return parsePath(s)
	}
	seq, ok := v.(starlark.Sequence)
	if !ok {
		return nil, fmt.Errorf("path must be a string or a list of segments, got %s", v.Type())
	}
	var segs []segment
	iter := seq.Iterate()
	defer iter.Done()
	var e starlark.Value
	for iter.Next(&e) {
		if s, ok := starlark.AsString(e); ok {
			segs = append(segs, segment{key: s})
			continue
		}
		if i, err := starlark.AsInt32(e); err == nil {
			segs = append(segs, segment{index: i, isIdx: true})
			continue
		}
		return nil, fmt.Errorf("path segment must be a string or an integer, got %s", e.Type())
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	return segs, nil
}

// resolvePath walks segments from a root value.
//
// It reports the prefix that did resolve, so a caller can say which link of the
// chain is missing rather than only that the whole path failed.
func resolvePath(root starlark.Value, segs []segment) (val starlark.Value, ok bool, resolved int) {
	cur := root
	for i, s := range segs {
		if cur == nil || cur == starlark.None {
			return nil, false, i
		}
		if s.isIdx {
			idx, isIdx := cur.(starlark.Indexable)
			if !isIdx {
				return nil, false, i
			}
			n := idx.Len()
			j := s.index
			if j < 0 {
				j += n
			}
			if j < 0 || j >= n {
				return nil, false, i
			}
			cur = idx.Index(j)
			continue
		}
		m, isMap := cur.(starlark.Mapping)
		if !isMap {
			// Fall back to attribute access so structs and similar values work.
			if ha, isAttr := cur.(starlark.HasAttrs); isAttr {
				v, err := ha.Attr(s.key)
				if err != nil || v == nil {
					return nil, false, i
				}
				cur = v
				continue
			}
			return nil, false, i
		}
		v, found, err := m.Get(starlark.String(s.key))
		if err != nil || !found {
			return nil, false, i
		}
		cur = v
	}
	return cur, true, len(segs)
}

// pathString renders segments back into the syntax a user would type.
func pathString(segs []segment) string {
	var b strings.Builder
	for i, s := range segs {
		if s.isIdx {
			b.WriteString(s.String())
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		if isIdentifier(s.key) {
			b.WriteString(s.key)
		} else {
			if i > 0 {
				// Undo the separator: a non-identifier key is bracketed.
				str := b.String()
				b.Reset()
				b.WriteString(str[:len(str)-1])
			}
			b.WriteString("[")
			b.WriteString(strconv.Quote(s.key))
			b.WriteString("]")
		}
	}
	return b.String()
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
