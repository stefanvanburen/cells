package lsp

import (
	"slices"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"
	"cel.dev/cel-go/ext/security/hmac"
	"cel.dev/cel-go/ext/security/jwt"
)

// extensionFactories maps a stable, user-facing extension name to its cel-go
// library constructor. Each is registered with cel-go's default options, which
// for the versioned libraries means their newest version; cells has no need to
// pin older versions since it never persists compiled expressions.
//
// ext.NativeTypes is deliberately absent: it maps Go struct types into the
// environment, so there is nothing a client could name on the command line or
// in initializationOptions to configure it.
var extensionFactories = map[string]func() cel.EnvOption{
	"bindings":       func() cel.EnvOption { return ext.Bindings() },
	"comprehensions": func() cel.EnvOption { return ext.TwoVarComprehensions() },
	"encoders":       func() cel.EnvOption { return ext.Encoders() },
	// hmac.Library registers the common algorithms (SHA256/384/512 and their
	// JOSE aliases) when no explicit set is configured.
	"hmac": func() cel.EnvOption { return hmac.Library() },
	// jwt.Library also enables optional types, since jwt.Token.claim returns
	// an optional. Enabling "optional" alongside it is harmless.
	"jwt":      func() cel.EnvOption { return jwt.Library() },
	"lists":    func() cel.EnvOption { return ext.Lists() },
	"math":     func() cel.EnvOption { return ext.Math() },
	"network":  func() cel.EnvOption { return ext.Network() },
	"optional": func() cel.EnvOption { return cel.OptionalTypes() },
	"protos":   func() cel.EnvOption { return ext.Protos() },
	// ext.Regex refuses to install unless the optional library is already
	// present, so it has to bring it along.
	"regex":   func() cel.EnvOption { return withOptions(cel.OptionalTypes(), ext.Regex()) },
	"sets":    func() cel.EnvOption { return ext.Sets() },
	"strings": func() cel.EnvOption { return ext.Strings() },
}

// withOptions applies several environment options as one, for extensions that
// depend on another library being installed first.
func withOptions(opts ...cel.EnvOption) cel.EnvOption {
	return func(e *cel.Env) (*cel.Env, error) {
		var err error
		for _, opt := range opts {
			if e, err = opt(e); err != nil {
				return nil, err
			}
		}
		return e, nil
	}
}

// sortedExtensionNames returns the known extension names in sorted order, for
// use in help text and error messages.
func sortedExtensionNames() []string {
	names := make([]string, 0, len(extensionFactories))
	for name := range extensionFactories {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// resolveExtensions converts extension names (e.g. "strings", "math") into
// cel-go environment options, in the order given.
func resolveExtensions(names []string) ([]cel.EnvOption, error) {
	opts := make([]cel.EnvOption, 0, len(names))
	for _, name := range names {
		factory, ok := extensionFactories[name]
		if !ok {
			return nil, unknownExtensionError(name)
		}
		opts = append(opts, factory())
	}
	return opts, nil
}
