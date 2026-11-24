package gitops

import (
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type yamlPathEditor struct {
	path []string
}

func (y yamlPathEditor) locate(docs []*yaml.Node) (*yaml.Node, error) {
	var matches []*yaml.Node
	for _, doc := range docs {
		node, err := y.walk(documentRoot(doc))
		if err != nil {
			return nil, err
		}
		if node != nil {
			matches = append(matches, node)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: no value at %s", ErrPinNotFound, y)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%w: %d documents have a value at %s", ErrAmbiguousPin, len(matches), y)
	}
}

func (y yamlPathEditor) walk(node *yaml.Node) (*yaml.Node, error) {
	for _, segment := range y.path {
		if node == nil {
			return nil, nil
		}
		switch node.Kind {
		case yaml.AliasNode:
			return nil, fmt.Errorf("%w: %s passes through an alias on line %d", ErrUnsupportedYAML, y, node.Line)
		case yaml.MappingNode:
			_, value, err := mappingValue(node, segment)
			if err != nil {
				return nil, err
			}
			node = value
		case yaml.SequenceNode:
			index, ok := sequenceIndex(segment, len(node.Content))
			if !ok {
				return nil, nil
			}
			node = node.Content[index]
		case yaml.DocumentNode, yaml.ScalarNode:
			return nil, nil
		}
	}
	if node != nil && node.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("%w: value at %s is an alias", ErrUnsupportedYAML, y)
	}
	return node, nil
}

func (y yamlPathEditor) value(docs []*yaml.Node) (string, error) {
	node, err := y.locate(docs)
	if err != nil {
		return "", err
	}
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%w: value at %s on line %d is not a scalar", ErrUnsupportedYAML, y, node.Line)
	}
	if node.ShortTag() != "!!str" {
		return "", nil
	}
	return node.Value, nil
}

func (y yamlPathEditor) splice(src []byte, docs []*yaml.Node, value string) ([]byte, int, error) {
	node, err := y.locate(docs)
	if err != nil {
		return nil, 0, err
	}
	out, err := replaceScalar(src, node, value)
	return out, 0, err
}

func (y yamlPathEditor) String() string {
	return strings.Join(y.path, ".")
}

func sequenceIndex(segment string, length int) (int, bool) {
	if segment == "" || strings.ContainsFunc(segment, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, false
	}
	index, err := strconv.Atoi(segment)
	return index, err == nil && index < length
}
