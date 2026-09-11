package version

import (
	"runtime/debug"
	"strings"
	"testing"

	"go.vanburen.xyz/ok"
)

// buildInfo builds what the toolchain would have stamped, from vcs settings
// given as alternating key and value.
func buildInfo(mainVersion string, settings ...string) *debug.BuildInfo {
	info := &debug.BuildInfo{GoVersion: "go1.27.0"}
	info.Main.Version = mainVersion
	for i := 0; i+1 < len(settings); i += 2 {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return info
}

func TestShort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"unstamped_binary", nil, "unknown"},
		{"installed_from_module", buildInfo("v0.4.0"), "v0.4.0"},
		{"no_version_and_no_revision", buildInfo("(devel)"), "devel"},
		{
			"revision_stands_in_for_devel",
			buildInfo("(devel)", vcsRevision, "8d6dbb7fb8d4924ac613b86952e447117d7f9bc7"),
			"8d6dbb7fb8d4",
		},
		{
			"modified_tree_is_marked",
			buildInfo("(devel)", vcsRevision, "8d6dbb7fb8d4924ac613b86952e447117d7f9bc7", vcsModified, "true"),
			"8d6dbb7fb8d4 (modified)",
		},
		{
			"module_version_wins_over_revision",
			buildInfo("v0.4.0", vcsRevision, "8d6dbb7fb8d4924ac613b86952e447117d7f9bc7"),
			"v0.4.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok.Equal(t, short(tt.info), tt.want)
		})
	}
}

func TestDetailOmitsWhatTheBinaryDoesNotCarry(t *testing.T) {
	t.Parallel()

	// Installed from a module: no revision and no build time to report.
	got := detail(buildInfo("v0.4.0"))
	ok.Equal(t, got, "cells    v0.4.0\ngo       go1.27.0\n")

	// Built from a checkout: everything, with the revision on its own line
	// only because there is a version leading.
	got = detail(buildInfo("v0.4.0",
		vcsRevision, "8d6dbb7fb8d4924ac613b86952e447117d7f9bc7",
		vcsTime, "2026-09-11T18:08:46Z",
	))
	ok.Equal(t, got, strings.Join([]string{
		"cells    v0.4.0",
		"revision 8d6dbb7fb8d4",
		"built    2026-09-11T18:08:46Z",
		"go       go1.27.0",
		"",
	}, "\n"))

	// The revision is not repeated when it is already the version.
	got = detail(buildInfo("(devel)", vcsRevision, "8d6dbb7fb8d4924ac613b86952e447117d7f9bc7"))
	ok.Equal(t, got, "cells    8d6dbb7fb8d4\ngo       go1.27.0\n")
}
