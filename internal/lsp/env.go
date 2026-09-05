package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/env"
	"cel.dev/cel-go/ext"
)

// unknownExtensionError reports an extension name cells does not accept,
// listing the ones it does.
func unknownExtensionError(name string) error {
	return fmt.Errorf("unknown CEL extension %q (available: %s)", name, strings.Join(sortedExtensionNames(), ", "))
}

// latestExtensionVersion is the version string cel-go reads as "the newest
// version of this extension library".
const latestExtensionVersion = "latest"

// Options describe the CEL environment that cells checks expressions against.
// The zero value is plain CEL with no declarations, which is what cells uses
// when a user configures nothing.
type Options struct {
	// ConfigPath names a CEL environment configuration file, declaring the
	// variables, types and functions that expressions may refer to. An empty
	// path means cells knows about nothing beyond the CEL standard library —
	// callers that want a configuration found rather than named should look
	// for one with FindConfig.
	ConfigPath string

	// Extensions names CEL extension libraries to enable on top of whatever
	// Config asks for. See ExtensionNames for the valid names.
	Extensions []string
}

// declared reports whether these options tell cells what the expressions it
// sees are allowed to refer to. Diagnostics are reported as errors when they
// do, and as warnings when they do not: without declarations every reference
// to a real variable is undeclared, so the check phase cannot distinguish a
// typo from a name cells was simply never told about.
func (o Options) declared() bool {
	return o.ConfigPath != ""
}

// ConfigFileName is the file cells looks for when no configuration is named
// explicitly.
const ConfigFileName = "cel.yaml"

// FindConfig returns the path of the nearest ConfigFileName in dir or one of
// its parents, or "" when there is none. A configuration therefore covers the
// directory tree beneath it, the way most per-repository tool configuration
// does.
func FindConfig(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ConfigFileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// LoadConfig reads a CEL environment configuration from path. The format is
// cel-go's own, shared with other CEL tooling; see
// https://pkg.go.dev/cel.dev/cel-go/common/env#Config.
func LoadConfig(path string) (*env.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	config, err := env.ConfigFromYAML(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// newCELEnv builds the CEL environment described by opts.
func newCELEnv(opts Options) (*cel.Env, error) {
	config := env.NewConfig(serverName)
	if opts.ConfigPath != "" {
		loaded, err := LoadConfig(opts.ConfigPath)
		if err != nil {
			return nil, err
		}
		config = loaded
	}
	if len(opts.Extensions) > 0 {
		// Extensions named outside the configuration are added to it, so that
		// there is one path into the environment rather than two. Copy first:
		// the caller's configuration is not ours to modify.
		withExts := *config
		withExts.Extensions = slices.Clone(config.Extensions)
		for _, name := range opts.Extensions {
			withExts.Extensions = append(withExts.Extensions, &env.Extension{Name: name, Version: latestExtensionVersion})
		}
		config = &withExts
	}

	return cel.NewEnv(
		cel.FromConfig(config, extensionOptionFactory),

		// These come after FromConfig deliberately, and the order is load
		// bearing. cells needs macro call tracking whatever a configuration
		// says — hover and rename both read SourceInfo.MacroCalls() — and a
		// configuration that disables the feature silently wins if it is
		// applied last. Nothing errors in that case; hover over a macro just
		// stops working.
		cel.EnableMacroCallTracking(),

		// Validate the arguments to duration(), timestamp(), and matches()
		// when they are literals. cel-go runs these during Check, so bad
		// literals surface as diagnostics rather than as runtime errors the
		// author only finds later.
		//
		// This is deliberately not cel.ExtendedValidations(), which also
		// bundles ValidateHomogeneousAggregateLiterals(). Heterogeneous
		// literals like [1, 'two'] are valid CEL — they type as list(dyn) —
		// so flagging them would be a false positive.
		cel.ASTValidators(
			cel.ValidateDurationLiterals(),
			cel.ValidateTimestampLiterals(),
			cel.ValidateRegexLiterals(),
		),
	)
}

// extensionOptionFactory resolves an extension named in a configuration.
//
// It answers for the names cel-go's own factory does not know, and for regex,
// whose library refuses to install unless optional types are already present.
// Everything else falls through to cel-go, which honors the version pinned in
// the configuration; the names cells answers for are always the newest
// version, since cells never persists a compiled expression and so has no
// reason to reproduce an older one.
func extensionOptionFactory(configElement any) (cel.EnvOption, bool) {
	extension, ok := configElement.(*env.Extension)
	if !ok {
		return nil, false
	}
	if cellsOwnsExtension(extension) {
		return extensionFactories[extension.Name](), true
	}
	if opt, ok := ext.ExtensionOptionFactory(configElement); ok {
		return opt, true
	}
	// Claim the element even though it cannot be resolved, so that the failure
	// is reported with the name cells uses and the list of names it accepts,
	// rather than cel-go's bare "unrecognized extension".
	err := unknownExtensionError(extension.Name)
	return func(*cel.Env) (*cel.Env, error) { return nil, err }, true
}

// cellsOwnsExtension reports whether cells, rather than cel-go, supplies the
// named extension. cells cannot honor a pinned version, so a configuration
// asking for one is left to cel-go to answer or reject.
func cellsOwnsExtension(extension *env.Extension) bool {
	if _, ok := extensionFactories[extension.Name]; !ok {
		return false
	}
	return extension.Version == "" || extension.Version == latestExtensionVersion
}
