package config

import "testing"

func TestValidateAppName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "blog"},
		{name: "digits and dash", in: "blog-2"},
		{name: "empty", in: "", wantErr: true},
		{name: "uppercase", in: "Blog", wantErr: true},
		{name: "dot dot", in: "..", wantErr: true},
		{name: "slash", in: "a/b", wantErr: true},
		{name: "leading dash", in: "-blog", wantErr: true},
		{name: "too long", in: "a234567890123456789012345678901234567890123456789012345678901234", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateAppName(tt.in); (err != nil) != tt.wantErr {
				t.Errorf("ValidateAppName(%q) = %v, want error %v", tt.in, err, tt.wantErr)
			}
		})
	}
}
