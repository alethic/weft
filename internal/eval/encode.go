package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	yaml "go.yaml.in/yaml/v3"
)

// yamlNode builds a YAML node tree directly from a Starlark value.
//
// Going through a Go map would lose mapping order, and these documents are
// usually written for a person to read: a values.yaml whose keys come out in
// hash order is materially worse than one in the order the author wrote.
func yamlNode(v starlark.Value, depth int) (*yaml.Node, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("value nested deeper than %d levels", maxDepth)
	}
	switch t := v.(type) {
	case starlark.NoneType:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case starlark.Bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(bool(t))}, nil
	case starlark.Int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: t.String()}, nil
	case starlark.Float:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(float64(t), 'g', -1, 64)}, nil
	case starlark.String:
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(t)}
		// A multi-line string as a block scalar is the difference between a
		// readable embedded document and a single line of escaped newlines.
		if strings.Contains(string(t), "\n") {
			n.Style = yaml.LiteralStyle
		}
		return n, nil
	case *starlark.List:
		return yamlSequence(t, t.Len(), func(i int) starlark.Value { return t.Index(i) }, depth)
	case starlark.Tuple:
		return yamlSequence(t, t.Len(), func(i int) starlark.Value { return t.Index(i) }, depth)
	case *starlark.Dict:
		items := t.Items()
		n := &yaml.Node{Kind: yaml.MappingNode}
		if len(items) == 0 {
			n.Style = yaml.FlowStyle
		}
		for _, kv := range items {
			k, ok := starlark.AsString(kv[0])
			if !ok {
				return nil, fmt.Errorf("mapping key must be a string, got %s", kv[0].Type())
			}
			vn, err := yamlNode(kv[1], depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, vn)
		}
		return n, nil
	case *Object:
		n := &yaml.Node{Kind: yaml.MappingNode}
		if len(t.keys) == 0 {
			n.Style = yaml.FlowStyle
		}
		for _, k := range t.keys {
			vn, err := yamlNode(t.m[k], depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, vn)
		}
		return n, nil
	case *starlarkstruct.Struct:
		n := &yaml.Node{Kind: yaml.MappingNode}
		names := t.AttrNames()
		if len(names) == 0 {
			n.Style = yaml.FlowStyle
		}
		for _, k := range names {
			av, err := t.Attr(k)
			if err != nil {
				return nil, err
			}
			vn, err := yamlNode(av, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, vn)
		}
		return n, nil
	case *waitValue:
		return nil, fmt.Errorf("wait() cannot be serialised")
	default:
		return nil, fmt.Errorf("cannot serialise a %s", v.Type())
	}
}

func yamlSequence(v starlark.Value, n int, at func(int) starlark.Value, depth int) (*yaml.Node, error) {
	node := &yaml.Node{Kind: yaml.SequenceNode}
	if n == 0 {
		node.Style = yaml.FlowStyle
	}
	for i := 0; i < n; i++ {
		c, err := yamlNode(at(i), depth+1)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		node.Content = append(node.Content, c)
	}
	return node, nil
}

func marshalYAML(node *yaml.Node, indent int) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(indent)
	if err := enc.Encode(node); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// encodeJSON writes JSON directly from Starlark values, preserving mapping
// order for the same reason yamlNode does.
func encodeJSON(v starlark.Value, indent int) (string, error) {
	var buf bytes.Buffer
	pad := strings.Repeat(" ", indent)
	if err := writeJSON(&buf, v, pad, "", 0); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeJSON(buf *bytes.Buffer, v starlark.Value, pad, cur string, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("value nested deeper than %d levels", maxDepth)
	}
	nl, sep := "", ""
	next := cur
	if pad != "" {
		nl, sep = "\n", " "
		next = cur + pad
	}

	switch t := v.(type) {
	case starlark.NoneType:
		buf.WriteString("null")
		return nil
	case starlark.Bool:
		buf.WriteString(strconv.FormatBool(bool(t)))
		return nil
	case starlark.Int:
		buf.WriteString(t.String())
		return nil
	case starlark.Float:
		f := float64(t)
		if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
			return fmt.Errorf("%v cannot be represented in JSON", f)
		}
		buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
		return nil
	case starlark.String:
		b, err := json.Marshal(string(t))
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	case *starlark.List:
		return writeJSONArray(buf, t.Len(), func(i int) starlark.Value { return t.Index(i) }, pad, cur, next, nl, depth)
	case starlark.Tuple:
		return writeJSONArray(buf, t.Len(), func(i int) starlark.Value { return t.Index(i) }, pad, cur, next, nl, depth)
	case *starlark.Dict:
		items := t.Items()
		keys := make([]string, 0, len(items))
		vals := make([]starlark.Value, 0, len(items))
		for _, kv := range items {
			k, ok := starlark.AsString(kv[0])
			if !ok {
				return fmt.Errorf("mapping key must be a string, got %s", kv[0].Type())
			}
			keys = append(keys, k)
			vals = append(vals, kv[1])
		}
		return writeJSONObject(buf, keys, vals, pad, cur, next, nl, sep, depth)
	case *Object:
		vals := make([]starlark.Value, 0, len(t.keys))
		for _, k := range t.keys {
			vals = append(vals, t.m[k])
		}
		return writeJSONObject(buf, t.keys, vals, pad, cur, next, nl, sep, depth)
	case *starlarkstruct.Struct:
		names := t.AttrNames()
		vals := make([]starlark.Value, 0, len(names))
		for _, k := range names {
			av, err := t.Attr(k)
			if err != nil {
				return err
			}
			vals = append(vals, av)
		}
		return writeJSONObject(buf, names, vals, pad, cur, next, nl, sep, depth)
	case *waitValue:
		return fmt.Errorf("wait() cannot be serialised")
	default:
		return fmt.Errorf("cannot serialise a %s", v.Type())
	}
}

func writeJSONArray(buf *bytes.Buffer, n int, at func(int) starlark.Value, pad, cur, next, nl string, depth int) error {
	if n == 0 {
		buf.WriteString("[]")
		return nil
	}
	buf.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString(nl)
		buf.WriteString(next)
		if err := writeJSON(buf, at(i), pad, next, depth+1); err != nil {
			return fmt.Errorf("[%d]: %w", i, err)
		}
	}
	buf.WriteString(nl)
	buf.WriteString(cur)
	buf.WriteString("]")
	return nil
}

func writeJSONObject(buf *bytes.Buffer, keys []string, vals []starlark.Value, pad, cur, next, nl, sep string, depth int) error {
	if len(keys) == 0 {
		buf.WriteString("{}")
		return nil
	}
	buf.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString(nl)
		buf.WriteString(next)
		b, err := json.Marshal(k)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteString(":")
		buf.WriteString(sep)
		if err := writeJSON(buf, vals[i], pad, next, depth+1); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	buf.WriteString(nl)
	buf.WriteString(cur)
	buf.WriteString("}")
	return nil
}
