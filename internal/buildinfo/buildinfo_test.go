package buildinfo

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		linked linked
		bi     *debug.BuildInfo
		ok     bool
		want   Info
	}{
		{
			name:   "no build info",
			linked: linked{version: "dev"},
			want:   Info{Version: "dev", GoVersion: runtime.Version()},
		},
		{
			name:   "build info reported but nil",
			linked: linked{version: "dev"},
			ok:     true,
			want:   Info{Version: "dev", GoVersion: runtime.Version()},
		},
		{
			name:   "build info without vcs settings",
			linked: linked{version: "v1.2.3"},
			bi:     &debug.BuildInfo{GoVersion: "go1.27.4"},
			ok:     true,
			want:   Info{Version: "v1.2.3", GoVersion: "go1.27.4"},
		},
		{
			name:   "empty go version falls back to runtime",
			linked: linked{version: "dev"},
			bi:     &debug.BuildInfo{},
			ok:     true,
			want:   Info{Version: "dev", GoVersion: runtime.Version()},
		},
		{
			name:   "clean vcs checkout",
			linked: linked{version: "v0.1.0"},
			bi: &debug.BuildInfo{
				GoVersion: "go1.27.4",
				Settings: []debug.BuildSetting{
					{Key: "-trimpath", Value: "true"},
					{Key: "vcs", Value: "git"},
					{Key: "vcs.revision", Value: "0123456789abcdef"},
					{Key: "vcs.time", Value: "2026-11-23T10:00:00Z"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			ok: true,
			want: Info{
				Version:    "v0.1.0",
				Commit:     "0123456789abcdef",
				CommitTime: "2026-11-23T10:00:00Z",
				GoVersion:  "go1.27.4",
			},
		},
		{
			name:   "linked commit without build info",
			linked: linked{version: "dev", commit: "linkedsha", commitTime: "2026-11-22T09:00:00Z"},
			want: Info{
				Version:    "dev",
				Commit:     "linkedsha",
				CommitTime: "2026-11-22T09:00:00Z",
				GoVersion:  runtime.Version(),
			},
		},
		{
			name:   "linked commit with build info lacking vcs settings",
			linked: linked{version: "v1.0.0", commit: "linkedsha", commitTime: "2026-11-22T09:00:00Z"},
			bi: &debug.BuildInfo{
				GoVersion: "go1.27.4",
				Settings:  []debug.BuildSetting{{Key: "-trimpath", Value: "true"}, {Key: "vcs.modified", Value: "false"}},
			},
			ok: true,
			want: Info{
				Version:    "v1.0.0",
				Commit:     "linkedsha",
				CommitTime: "2026-11-22T09:00:00Z",
				GoVersion:  "go1.27.4",
			},
		},
		{
			name:   "vcs settings override linked values",
			linked: linked{version: "v1.0.0", commit: "linkedsha", commitTime: "2026-11-22T09:00:00Z"},
			bi: &debug.BuildInfo{
				GoVersion: "go1.27.4",
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "vcssha"},
					{Key: "vcs.time", Value: "2026-11-23T10:00:00Z"},
				},
			},
			ok: true,
			want: Info{
				Version:    "v1.0.0",
				Commit:     "vcssha",
				CommitTime: "2026-11-23T10:00:00Z",
				GoVersion:  "go1.27.4",
			},
		},
		{
			name:   "dirty vcs checkout",
			linked: linked{version: "dev"},
			bi: &debug.BuildInfo{
				GoVersion: "go1.27.4",
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "abc"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:   true,
			want: Info{Version: "dev", Commit: "abc", GoVersion: "go1.27.4", Modified: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := read(tt.linked, func() (*debug.BuildInfo, bool) { return tt.bi, tt.ok })
			if got != tt.want {
				t.Errorf("read() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReadUsesLinkedVersion(t *testing.T) {
	t.Parallel()

	got := Read()
	if got.Version != version {
		t.Errorf("Read().Version = %q, want %q", got.Version, version)
	}
	if got.GoVersion == "" {
		t.Error("Read().GoVersion is empty")
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "version only",
			info: Info{Version: "dev", GoVersion: "go1.27.4"},
			want: "doorstep dev (go1.27.4)",
		},
		{
			name: "clean commit",
			info: Info{Version: "v1.0.0", Commit: "abc", CommitTime: "2026-11-23T10:00:00Z", GoVersion: "go1.27.4"},
			want: "doorstep v1.0.0 (commit abc, 2026-11-23T10:00:00Z, go1.27.4)",
		},
		{
			name: "dirty commit",
			info: Info{Version: "dev", Commit: "abc", GoVersion: "go1.27.4", Modified: true},
			want: "doorstep dev (commit abc-dirty, go1.27.4)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.info.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInfoJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "omits empty vcs fields",
			info: Info{Version: "dev", GoVersion: "go1.27.4"},
			want: `{"version":"dev","goVersion":"go1.27.4","modified":false}`,
		},
		{
			name: "all fields",
			info: Info{Version: "v1", Commit: "abc", CommitTime: "t", GoVersion: "go1.27.4", Modified: true},
			want: `{"version":"v1","commit":"abc","commitTime":"t","goVersion":"go1.27.4","modified":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tt.info)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestInfoLogValue(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("started", slog.Any("build", Info{Version: "v1", Commit: "abc", GoVersion: "go1.27.4", Modified: true}))

	var entry struct {
		Build map[string]any `json:"build"`
	}
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("Unmarshal %q: %v", buf.String(), err)
	}
	want := map[string]any{
		"version":     "v1",
		"commit":      "abc",
		"commit_time": "",
		"go_version":  "go1.27.4",
		"modified":    true,
	}
	for key, value := range want {
		if entry.Build[key] != value {
			t.Errorf("build[%q] = %v, want %v", key, entry.Build[key], value)
		}
	}
}
