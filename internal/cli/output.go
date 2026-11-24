package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const (
	flagOutput = "output"

	outputTable = "table"
	outputJSON  = "json"

	shortSHALength    = 12
	shortDigestLength = len("sha256:") + 12
)

func shortSHA(sha string) string {
	return sha[:min(len(sha), shortSHALength)]
}

func shortDigest(d string) string {
	return d[:min(len(d), shortDigestLength)]
}

func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func validateOutput(output string) error {
	if output != outputTable && output != outputJSON {
		return fmt.Errorf("--%s: must be %s or %s, got %q", flagOutput, outputTable, outputJSON, output)
	}
	return nil
}

func writeIndentedJSON(out io.Writer, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	buf.WriteByte('\n')
	_, err := buf.WriteTo(out)
	return err
}
