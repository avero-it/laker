package version

import "runtime/debug"

type AppInfo struct {
	Name            string
	Description     string
	AppFunction     string
	GoVersion       string
	Version         string
	CommitHash      string
	CommitTimestamp string
}

func GetCommitHash() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return ""
}

func GetCommitTimestamp() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.time" {
				return setting.Value
			}
		}
	}
	return ""
}

// version is stamped at release time: -ldflags "-X github.com/avero-it/laker/pkg/version.version=1.2.3".
// Plain "go build" leaves it empty and the module version from the build info is used.
var version string

func GetVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return ""
}

func GetGoVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return ""
}
