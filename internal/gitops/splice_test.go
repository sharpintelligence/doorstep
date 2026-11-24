package gitops

import (
	"errors"
	"testing"
)

func TestOffsetOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		src     string
		line    int
		column  int
		want    int
		wantErr bool
	}{
		{name: "first column", src: "abc\n", line: 1, column: 1, want: 0},
		{name: "later line", src: "a\nbc\n", line: 2, column: 2, want: 3},
		{name: "CRLF", src: "a\r\nbc\r\n", line: 2, column: 2, want: 4},
		{name: "lone CR", src: "a\rbc", line: 2, column: 2, want: 3},
		{name: "runes", src: "é€x", line: 1, column: 3, want: 5},
		{name: "byte order mark", src: "\xef\xbb\xbfab", line: 1, column: 2, want: 4},
		{name: "end of line", src: "ab", line: 1, column: 3, want: 2},
		{name: "past end of line", src: "ab\ncd", line: 1, column: 4, wantErr: true},
		{name: "past last line", src: "ab", line: 3, column: 1, wantErr: true},
		{name: "invalid UTF-8", src: "\xffab", line: 1, column: 2, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := offsetOf([]byte(tt.src), tt.line, tt.column)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedYAML) {
					t.Fatalf("offsetOf() error = %v, want ErrUnsupportedYAML", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("offsetOf() = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestInsertLineAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		src    string
		line   int
		indent int
		want   string
	}{
		{name: "middle", src: "a\nb\n", line: 1, indent: 2, want: "a\n  x\nb\n"},
		{name: "last line with newline", src: "a\nb\n", line: 2, indent: 0, want: "a\nb\nx\n"},
		{name: "last line without newline", src: "a\nb", line: 2, indent: 1, want: "a\nb\n x"},
		{name: "CRLF", src: "a\r\nb\r\n", line: 1, indent: 0, want: "a\r\nx\r\nb\r\n"},
		{name: "CRLF without final newline", src: "a\r\nb", line: 2, indent: 0, want: "a\r\nb\r\nx"},
		{name: "single line without newline", src: "a", line: 1, indent: 0, want: "a\nx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := insertLineAfter([]byte(tt.src), tt.line, tt.indent, "x")
			if err != nil || string(got) != tt.want {
				t.Fatalf("insertLineAfter() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
