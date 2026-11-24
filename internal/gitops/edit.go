package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

type editor interface {
	value(docs []*yaml.Node) (string, error)
	splice(src []byte, docs []*yaml.Node, value string) ([]byte, int, error)
}

func editorFor(t Target) editor {
	if t.YAML != nil {
		return yamlPathEditor{path: t.YAML.Path}
	}
	return kustomizeEditor{image: t.Kustomize.Name}
}

func readValue(e editor, src []byte) (string, error) {
	docs, err := parseDocuments(src)
	if err != nil {
		return "", err
	}
	return e.value(docs)
}

func writeValue(e editor, src []byte, value string) ([]byte, error) {
	before, err := parseDocuments(src)
	if err != nil {
		return nil, err
	}
	out, added, err := e.splice(src, before, value)
	if err != nil {
		return nil, err
	}
	after, err := parseDocuments(out)
	if err != nil {
		return nil, fmt.Errorf("%w: edited file does not parse: %w", ErrUnsupportedYAML, err)
	}
	got, err := e.value(after)
	if err != nil {
		return nil, fmt.Errorf("%w: edited file: %w", ErrUnsupportedYAML, err)
	}
	if got != value {
		return nil, fmt.Errorf("%w: edited file holds %q instead of %q", ErrUnsupportedYAML, got, value)
	}
	if want, have := countNodes(before)+added, countNodes(after); want != have {
		return nil, fmt.Errorf("%w: edit changed the document structure (%d nodes, expected %d)", ErrUnsupportedYAML, have, want)
	}
	return out, nil
}

func parseDocuments(src []byte) ([]*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(src))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: parse: %w", ErrUnsupportedYAML, err)
		}
		docs = append(docs, &doc)
	}
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

func countNodes(docs []*yaml.Node) int {
	total := 0
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		total++
		for _, child := range n.Content {
			walk(child)
		}
	}
	for _, doc := range docs {
		walk(doc)
	}
	return total
}

func mappingValue(m *yaml.Node, key string) (*yaml.Node, *yaml.Node, error) {
	var foundKey, foundValue *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge" {
			return nil, nil, fmt.Errorf("%w: mapping on line %d uses a merge key", ErrUnsupportedYAML, m.Line)
		}
		if k.Kind != yaml.ScalarNode || k.Value != key {
			continue
		}
		if foundKey != nil {
			return nil, nil, fmt.Errorf("%w: key %q appears twice in the mapping on line %d", ErrAmbiguousPin, key, m.Line)
		}
		foundKey, foundValue = k, m.Content[i+1]
	}
	return foundKey, foundValue, nil
}
