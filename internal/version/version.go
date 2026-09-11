// Package version reports what the running binary records about how it was
// built.
//
// Nothing here is stamped by a build script: the Go toolchain embeds the
// module version, and for a build from a checkout the revision it came from.
// See [runtime/debug.ReadBuildInfo].
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// The build settings the Go toolchain records for a binary built from a
// version control checkout.
const (
	vcsRevision = "vcs.revision"
	vcsTime     = "vcs.time"
	vcsModified = "vcs.modified"
)

// shortRevisionLength is how much of a revision to show. Enough to find the
// commit, short of filling the line with it.
const shortRevisionLength = 12

// Short returns a one-line version: the module version the binary was
// installed from, or the revision it was built from when there is none.
//
// A binary installed with "go install module@version" carries that version. A
// binary built from a checkout is stamped "(devel)" instead, which says
// nothing useful, so the revision answers for it.
func Short() string {
	return short(read())
}

// Detail returns everything the binary records, a field per line. Fields it
// does not carry are left out rather than shown empty, since an installed
// binary has no revision and a checkout build has no module version.
func Detail() string {
	return detail(read())
}

// read returns the build information, or nil when there is none — a binary
// the toolchain did not stamp, which should not happen outside of tests.
func read() *debug.BuildInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return info
}

func short(info *debug.BuildInfo) string {
	if info == nil {
		return "unknown"
	}
	if version := moduleVersion(info); version != "" {
		return version
	}
	if revision := revision(info); revision != "" {
		return revision
	}
	return "devel"
}

func detail(info *debug.BuildInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cells    %s\n", short(info))
	if info == nil {
		return b.String()
	}
	// The version line already holds the revision when there is no module
	// version to lead with, so it is only worth a line of its own alongside
	// one.
	if revision := revision(info); revision != "" && moduleVersion(info) != "" {
		fmt.Fprintf(&b, "revision %s\n", revision)
	}
	if built := setting(info, vcsTime); built != "" {
		fmt.Fprintf(&b, "built    %s\n", built)
	}
	fmt.Fprintf(&b, "go       %s\n", info.GoVersion)
	return b.String()
}

// moduleVersion returns the version the binary's module was installed at, or
// "" for a build from a checkout.
func moduleVersion(info *debug.BuildInfo) string {
	if version := info.Main.Version; version != "" && version != "(devel)" {
		return version
	}
	return ""
}

// revision returns the shortened revision the binary was built from, marked
// when the tree it was built from had uncommitted changes, or "" when the
// build carries no revision at all.
func revision(info *debug.BuildInfo) string {
	revision := setting(info, vcsRevision)
	if revision == "" {
		return ""
	}
	if len(revision) > shortRevisionLength {
		revision = revision[:shortRevisionLength]
	}
	if setting(info, vcsModified) == "true" {
		revision += " (modified)"
	}
	return revision
}

// setting returns the named build setting, or "" when the binary carries none.
func setting(info *debug.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}
