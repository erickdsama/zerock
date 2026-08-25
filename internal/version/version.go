package version

import "runtime/debug"

// Version is the zerock release, set at build time with
// -ldflags "-X github.com/erickdsama/zerock/internal/version.Version=x.y.z".
var Version = "dev"

// Build identifies the individual build. The Makefile stamps it with a UTC
// timestamp, which is what makes two "dev" builds distinguishable — without it
// there is no way to tell whether a deployed binary is the one you just built.
var Build = ""

// String renders the full version, including the build stamp when there is one.
func String() string {
	if Build == "" {
		return Version
	}
	return Version + " (build " + Build + ")"
}

// UserAgent identifies this build in the control handshake.
func UserAgent() string { return "zerock/" + Version }

// GoVersion reports the toolchain that produced this binary.
func GoVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return "unknown"
}
