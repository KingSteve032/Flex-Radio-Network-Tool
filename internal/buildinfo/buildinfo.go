package buildinfo

import "fmt"

var (
	// Version is the semantic version for this build.
	// Override at build time with:
	//   -ldflags "-X github.com/KingSteve032/Flex-Radio-Network-Tool/internal/buildinfo.Version=vX.Y.Z"
	Version = "0.2.6"

	// Commit is the git commit used for this build.
	Commit = "dev"

	// BuildDate should be UTC, e.g. 2026-05-04T16:00:00Z.
	BuildDate = "unknown"
)

func Short() string {
	return Version
}

func Full() string {
	return fmt.Sprintf("%s (commit=%s, built=%s)", Version, Commit, BuildDate)
}
