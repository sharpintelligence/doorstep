package gitops

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

const insertedNodes = 2

type kustomizeEditor struct {
	image string
}

func (k kustomizeEditor) entry(docs []*yaml.Node) (*yaml.Node, error) {
	var matches []*yaml.Node
	hasImages := false
	for _, doc := range docs {
		root := documentRoot(doc)
		if root == nil || root.Kind != yaml.MappingNode {
			continue
		}
		_, images, err := mappingValue(root, "images")
		if err != nil {
			return nil, err
		}
		if images == nil || images.ShortTag() == "!!null" {
			continue
		}
		if images.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("%w: images on line %d is not a list", ErrUnsupportedYAML, images.Line)
		}
		hasImages = true
		for _, item := range images.Content {
			if item.Kind == yaml.AliasNode {
				return nil, fmt.Errorf("%w: images entry on line %d is an alias", ErrUnsupportedYAML, item.Line)
			}
			if item.Kind != yaml.MappingNode {
				continue
			}
			_, name, err := mappingValue(item, "name")
			if err != nil {
				return nil, err
			}
			if name != nil && name.Kind == yaml.ScalarNode && name.Value == k.image {
				matches = append(matches, item)
			}
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		return nil, fmt.Errorf("%w: %d images entries are named %s", ErrAmbiguousPin, len(matches), k.image)
	case hasImages:
		return nil, fmt.Errorf("%w: no images entry is named %s", ErrPinNotFound, k.image)
	default:
		return nil, fmt.Errorf("%w: no images list; add an entry named %s", ErrPinNotFound, k.image)
	}
}

func (k kustomizeEditor) value(docs []*yaml.Node) (string, error) {
	entry, err := k.entry(docs)
	if err != nil {
		return "", err
	}
	_, digest, err := mappingValue(entry, "digest")
	if err != nil || digest == nil {
		return "", err
	}
	if digest.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%w: digest on line %d is not a scalar", ErrUnsupportedYAML, digest.Line)
	}
	if digest.ShortTag() != "!!str" {
		return "", nil
	}
	return digest.Value, nil
}

func (k kustomizeEditor) splice(src []byte, docs []*yaml.Node, value string) ([]byte, int, error) {
	entry, err := k.entry(docs)
	if err != nil {
		return nil, 0, err
	}
	_, digest, err := mappingValue(entry, "digest")
	if err != nil {
		return nil, 0, err
	}
	if digest != nil {
		out, err := replaceScalar(src, digest, value)
		return out, 0, err
	}
	if entry.Style&yaml.FlowStyle != 0 {
		return nil, 0, fmt.Errorf("%w: images entry on line %d is a flow mapping without a digest key", ErrUnsupportedYAML, entry.Line)
	}
	nameKey, name, err := mappingValue(entry, "name")
	if err != nil {
		return nil, 0, err
	}
	if _, _, err := scalarSpan(src, name); err != nil || name.Line != nameKey.Line {
		return nil, 0, fmt.Errorf("%w: name of the images entry on line %d must be a single-line scalar", ErrUnsupportedYAML, nameKey.Line)
	}
	out, err := insertLineAfter(src, name.Line, nameKey.Column-1, "digest: "+value)
	return out, insertedNodes, err
}
