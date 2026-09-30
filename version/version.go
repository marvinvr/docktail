// Package version holds the DockTail build version and the optional check for
// a newer release.
package version

// Version is the DockTail release this binary was built as. Release builds
// replace the development value with the image tag via -ldflags
// "-X github.com/marvinvr/docktail/version.Version=<tag>" (see the Dockerfile).
var Version = "dev"
