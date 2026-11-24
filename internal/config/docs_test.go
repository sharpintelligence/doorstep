package config

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"testing"
)

func yamlBlocks(t *testing.T, markdown string) []string {
	t.Helper()
	var blocks []string
	var current strings.Builder
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(markdown))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case !inBlock && line == "```yaml":
			inBlock = true
			current.Reset()
		case inBlock && line == "```":
			inBlock = false
			blocks = append(blocks, current.String())
		case inBlock:
			current.WriteString(line)
			current.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan markdown: %v", err)
	}
	if inBlock {
		t.Fatal("unterminated yaml block")
	}
	return blocks
}

func TestDocumentedExamplesAreValid(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	blocks := yamlBlocks(t, string(data))
	if len(blocks) < 2 {
		t.Fatalf("found %d yaml blocks in docs/configuration.md, want at least 2", len(blocks))
	}
	for i, block := range blocks {
		file, err := Parse(strings.NewReader(block))
		if err != nil {
			t.Errorf("yaml block %d does not parse: %v\n%s", i, err, block)
			continue
		}
		if len(file.Apps) == 0 {
			t.Errorf("yaml block %d defines no apps:\n%s", i, block)
		}
		settings := file.Apply(Default())
		if err := settings.ValidateSettings(); err != nil {
			t.Errorf("yaml block %d has invalid settings: %v", i, err)
		}
		if err := errors.Join(file.Catalog().Validate(), file.Catalog().ValidateWith(settings)); err != nil {
			t.Errorf("yaml block %d is invalid: %v", i, err)
		}
	}
}
