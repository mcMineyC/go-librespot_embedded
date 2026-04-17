package go_librespot

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

const SpotifyVersionCode = 127700358

var version string

// Extract and return the commit hash stored in the binary, if available.
func commitHash() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return ""
}

func VersionNumberString() string {
	if len(version) > 0 {
		return strings.TrimPrefix(version, "v")
	} else if commit := commitHash(); len(commit) >= 8 {
		return commit[:8]
	} else {
		return "dev"
	}
}

func SpotifyLikeClientVersion() string {
	if len(version) > 0 {
		if commit := commitHash(); len(commit) >= 8 {
			return fmt.Sprintf("%s.g%s", version, commit[:8])
		} else {
			return version
		}
	}

	return "0.0.0"
}

func VersionString() string {
	return fmt.Sprintf("go-librespot %s", VersionNumberString())
}

func SystemInfoString() string {
	return fmt.Sprintf("%s; Go %s (%s %s)", VersionString(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func userAgentPlatformComment() string {
	osName := strings.TrimPrefix(GetOS().String(), "OS_")
	cpuName := strings.TrimPrefix(GetCpuFamily().String(), "CPU_")
	platformName := strings.TrimPrefix(GetPlatform().String(), "PLATFORM_")

	osName = strings.ReplaceAll(osName, "_", " ")
	cpuName = strings.ReplaceAll(cpuName, "_", " ")
	platformName = strings.ReplaceAll(platformName, "_", " ")

	osName = strings.ToLower(strings.TrimSpace(osName))
	cpuName = strings.ToLower(strings.TrimSpace(cpuName))
	platformName = strings.ToLower(strings.TrimSpace(platformName))

	parts := make([]string, 0, 3)
	if osName != "" && osName != "unknown" {
		parts = append(parts, osName)
	}
	if cpuName != "" && cpuName != "unknown" {
		parts = append(parts, cpuName)
	}
	if platformName != "" && platformName != "generic partner" {
		parts = append(parts, platformName)
	}

	if len(parts) == 0 {
		return "generic"
	}

	return strings.Join(parts, "; ")
}

func UserAgent() string {
	return fmt.Sprintf(
		"Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.7559.246 Spotify/%s Safari/537.36",
		userAgentPlatformComment(),
		SpotifyLikeClientVersion(),
	)
}
