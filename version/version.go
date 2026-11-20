package version

var (
	// Version provides the operators version
	Version    = "1.0.0"
	SDKVersion = "1.21.0"
	// GitCommit is set at build time via -ldflags "-X github.com/openshift/managed-upgrade-operator/version.GitCommit=<sha>"
	GitCommit = "unknown"
)
