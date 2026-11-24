package buildinfo

import (
	"log/slog"
	"runtime"
	"runtime/debug"
	"strings"
)

var version = "dev"

type Info struct {
	Version    string `json:"version"`
	Commit     string `json:"commit,omitempty"`
	CommitTime string `json:"commitTime,omitempty"`
	GoVersion  string `json:"goVersion"`
	Modified   bool   `json:"modified"`
}

func Read() Info {
	return read(version, debug.ReadBuildInfo)
}

func read(releaseVersion string, readBuildInfo func() (*debug.BuildInfo, bool)) Info {
	info := Info{Version: releaseVersion, GoVersion: runtime.Version()}
	bi, ok := readBuildInfo()
	if !ok || bi == nil {
		return info
	}
	if bi.GoVersion != "" {
		info.GoVersion = bi.GoVersion
	}
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Commit = setting.Value
		case "vcs.time":
			info.CommitTime = setting.Value
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		}
	}
	return info
}

func (i Info) String() string {
	details := make([]string, 0, 3)
	if i.Commit != "" {
		commit := i.Commit
		if i.Modified {
			commit += "-dirty"
		}
		details = append(details, "commit "+commit)
	}
	if i.CommitTime != "" {
		details = append(details, i.CommitTime)
	}
	details = append(details, i.GoVersion)
	return "doorstep " + i.Version + " (" + strings.Join(details, ", ") + ")"
}

func (i Info) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("version", i.Version),
		slog.String("commit", i.Commit),
		slog.String("commit_time", i.CommitTime),
		slog.String("go_version", i.GoVersion),
		slog.Bool("modified", i.Modified),
	)
}
