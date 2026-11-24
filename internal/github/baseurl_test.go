package github

import "testing"

func TestWebBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		api  string
		want string
	}{
		{api: "https://api.github.com", want: "https://github.com"},
		{api: "https://API.GitHub.com/", want: "https://github.com"},
		{api: "https://ghe.example.com/api/v3", want: "https://ghe.example.com"},
		{api: "https://ghe.example.com/api/v3/", want: "https://ghe.example.com"},
		{api: "https://ghe.example.com:8443/api/v3", want: "https://ghe.example.com:8443"},
		{api: "https://api.acme.ghe.com", want: "https://acme.ghe.com"},
		{api: "https://example.com/github/api/v3", want: "https://example.com/github"},
		{api: "http://127.0.0.1:4567", want: "http://127.0.0.1:4567"},
		{api: "http://127.0.0.1:4567/api", want: "http://127.0.0.1:4567/api"},
	}

	for _, tt := range tests {
		t.Run(tt.api, func(t *testing.T) {
			t.Parallel()
			api, err := ParseBaseURL(tt.api)
			if err != nil {
				t.Fatalf("ParseBaseURL(%q): %v", tt.api, err)
			}
			before := api.String()
			if got := WebBaseURL(api).String(); got != tt.want {
				t.Errorf("WebBaseURL(%q) = %q, want %q", tt.api, got, tt.want)
			}
			if api.String() != before {
				t.Errorf("WebBaseURL modified its argument: %q, want %q", api, before)
			}
		})
	}
}
