package gitops

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var (
	byteOrderMark = []byte("\xef\xbb\xbf")
	safeScalar    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
)

func lineStart(src []byte, line int) (int, error) {
	offset := 0
	if bytes.HasPrefix(src, byteOrderMark) {
		offset = len(byteOrderMark)
	}
	for current := 1; current < line; current++ {
		i := bytes.IndexAny(src[offset:], "\r\n")
		if i < 0 {
			return 0, fmt.Errorf("%w: line %d is past the end of the file", ErrUnsupportedYAML, line)
		}
		offset += i + lineBreakLength(src[offset+i:])
	}
	return offset, nil
}

func lineBreakLength(b []byte) int {
	if bytes.HasPrefix(b, []byte("\r\n")) {
		return 2
	}
	return 1
}

func offsetOf(src []byte, line, column int) (int, error) {
	offset, err := lineStart(src, line)
	if err != nil {
		return 0, err
	}
	for range column - 1 {
		if offset >= len(src) || src[offset] == '\n' || src[offset] == '\r' {
			return 0, fmt.Errorf("%w: column %d is past the end of line %d", ErrUnsupportedYAML, column, line)
		}
		r, size := utf8.DecodeRune(src[offset:])
		if r == utf8.RuneError && size <= 1 {
			return 0, fmt.Errorf("%w: invalid UTF-8 on line %d", ErrUnsupportedYAML, line)
		}
		offset += size
	}
	return offset, nil
}

func checkEditable(node *yaml.Node) error {
	switch {
	case node.Kind == yaml.AliasNode:
		return fmt.Errorf("%w: value on line %d is an alias", ErrUnsupportedYAML, node.Line)
	case node.Kind != yaml.ScalarNode:
		return fmt.Errorf("%w: value on line %d is not a scalar", ErrUnsupportedYAML, node.Line)
	case node.Anchor != "":
		return fmt.Errorf("%w: value on line %d has an anchor", ErrUnsupportedYAML, node.Line)
	case node.Style&yaml.TaggedStyle != 0:
		return fmt.Errorf("%w: value on line %d has an explicit tag", ErrUnsupportedYAML, node.Line)
	case node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return fmt.Errorf("%w: value on line %d is a block scalar", ErrUnsupportedYAML, node.Line)
	case node.ShortTag() == "!!null":
		return fmt.Errorf("%w: value on line %d is null; set a placeholder such as \"\"", ErrUnsupportedYAML, node.Line)
	case node.ShortTag() != "!!str":
		return fmt.Errorf("%w: value on line %d is a %s, not a string", ErrUnsupportedYAML, node.Line, node.ShortTag())
	}
	return nil
}

func scalarSpan(src []byte, node *yaml.Node) (int, int, error) {
	start, err := offsetOf(src, node.Line, node.Column)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case node.Style&yaml.DoubleQuotedStyle != 0:
		return quotedSpan(src, start, '"', node.Line)
	case node.Style&yaml.SingleQuotedStyle != 0:
		return quotedSpan(src, start, '\'', node.Line)
	case node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return 0, 0, fmt.Errorf("%w: value on line %d is a block scalar", ErrUnsupportedYAML, node.Line)
	}
	end := start + len(node.Value)
	if node.Value == "" || end > len(src) || string(src[start:end]) != node.Value || !endsPlain(src, end) {
		return 0, 0, fmt.Errorf("%w: value on line %d is not a single-line plain scalar", ErrUnsupportedYAML, node.Line)
	}
	return start, end, nil
}

func quotedSpan(src []byte, start int, quote byte, line int) (int, int, error) {
	if start >= len(src) || src[start] != quote {
		return 0, 0, fmt.Errorf("%w: expected a quote on line %d", ErrUnsupportedYAML, line)
	}
	for i := start + 1; i < len(src); i++ {
		switch c := src[i]; {
		case c == '\n' || c == '\r':
			return 0, 0, fmt.Errorf("%w: quoted value on line %d spans several lines", ErrUnsupportedYAML, line)
		case quote == '"' && c == '\\':
			i++
		case c == quote && quote == '\'' && i+1 < len(src) && src[i+1] == '\'':
			i++
		case c == quote:
			return start, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: unterminated quoted value on line %d", ErrUnsupportedYAML, line)
}

func endsPlain(src []byte, end int) bool {
	return end == len(src) || strings.IndexByte(" \t\r\n,]}", src[end]) >= 0
}

func replaceScalar(src []byte, node *yaml.Node, value string) ([]byte, error) {
	if err := checkEditable(node); err != nil {
		return nil, err
	}
	if !safeScalar.MatchString(value) {
		return nil, fmt.Errorf("%w: %q cannot be written without escaping", ErrUnsupportedYAML, value)
	}
	start, end, err := scalarSpan(src, node)
	if err != nil {
		return nil, err
	}
	literal := value
	switch {
	case node.Style&yaml.DoubleQuotedStyle != 0:
		literal = `"` + value + `"`
	case node.Style&yaml.SingleQuotedStyle != 0:
		literal = "'" + value + "'"
	}
	out := make([]byte, 0, len(src)-(end-start)+len(literal))
	out = append(out, src[:start]...)
	out = append(out, literal...)
	return append(out, src[end:]...), nil
}

func insertLineAfter(src []byte, line, indent int, text string) ([]byte, error) {
	start, err := lineStart(src, line)
	if err != nil {
		return nil, err
	}
	newLine := strings.Repeat(" ", indent) + text
	var at int
	var inserted string
	if i := bytes.IndexAny(src[start:], "\r\n"); i >= 0 {
		at = start + i + lineBreakLength(src[start+i:])
		inserted = newLine + string(src[start+i:at])
	} else {
		at = len(src)
		inserted = fileLineEnding(src) + newLine
	}
	out := make([]byte, 0, len(src)+len(inserted))
	out = append(out, src[:at]...)
	out = append(out, inserted...)
	return append(out, src[at:]...), nil
}

func fileLineEnding(src []byte) string {
	i := bytes.IndexAny(src, "\r\n")
	if i < 0 {
		return "\n"
	}
	return string(src[i : i+lineBreakLength(src[i:])])
}
