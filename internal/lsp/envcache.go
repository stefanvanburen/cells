package lsp

import (
	"os"
	"path/filepath"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// environment is a CEL environment together with what cells needs to know
// about how it was configured.
type environment struct {
	celEnv *cel.Env

	// checkSeverity is how a type-check failure against this environment is
	// reported; see the function of the same name.
	checkSeverity protocol.DiagnosticSeverity

	// fieldDocs holds the documentation the environment's descriptor sets
	// carry, keyed by fully-qualified field name. cel-go's type provider knows
	// a field's type but not what the .proto file said about it.
	fieldDocs map[string]string

	// configPath is the configuration this environment was built from, or ""
	// when it was built from none. stamp is what that file looked like at the
	// time, so that a later edit to it is noticed.
	configPath string
	stamp      fileStamp
}

// fieldDoc returns the documentation the .proto file carried for a field of
// the given message type, or "" when there is none — the type is not a
// message, the descriptor set was built without source info, or the field
// simply has no comment.
func (e *environment) fieldDoc(messageType *types.Type, fieldName string) string {
	if e == nil || messageType == nil || messageType.Kind() != types.StructKind {
		return ""
	}
	return e.fieldDocs[messageType.TypeName()+"."+fieldName]
}

// fileStamp is the part of a file's metadata cells uses to decide whether it
// has changed. Modification time alone misses an edit made within the
// filesystem's timestamp resolution, which a size usually catches.
type fileStamp struct {
	modTime time.Time
	size    int64
}

// statStamp returns the stamp of the file at path. A file that cannot be
// stat'd, including one that does not exist, gets the zero stamp, so that its
// appearing or disappearing counts as a change.
func statStamp(path string) fileStamp {
	if path == "" {
		return fileStamp{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{modTime: info.ModTime(), size: info.Size()}
}

// envCache builds the CEL environment for each configuration in use and reuses
// it, rebuilding when the configuration file changes on disk.
//
// It is not guarded by a lock: like the rest of the server's state it relies on
// ServeStream's synchronous dispatch, which runs every handler on one
// goroutine.
type envCache struct {
	// opts are the options every environment is built from. Their ConfigPath,
	// when set, is the configuration named by the command line or by
	// initializationOptions: it applies to every document, and suppresses the
	// search for one.
	opts Options

	byPath map[string]*environment
}

func newEnvCache(opts Options) *envCache {
	return &envCache{
		opts:   opts,
		byPath: make(map[string]*environment),
	}
}

// forDocument returns the environment that applies to the document at uri,
// which is the one built from the nearest configuration above it.
func (c *envCache) forDocument(docURI uri.URI) (*environment, error) {
	return c.forPath(c.configPathFor(docURI))
}

// configPathFor returns the configuration that governs the document at uri.
func (c *envCache) configPathFor(docURI uri.URI) string {
	if c.opts.ConfigPath != "" {
		return c.opts.ConfigPath
	}
	// A document with no file behind it — an untitled buffer, say — has no
	// directory to search from.
	if !docURI.IsFile() {
		return ""
	}
	return FindConfig(filepath.Dir(docURI.FsPath()))
}

// forPath returns the environment built from the configuration at configPath,
// which may be "" for no configuration at all.
func (c *envCache) forPath(configPath string) (*environment, error) {
	stamp := statStamp(configPath)
	if cached, ok := c.byPath[configPath]; ok && cached.stamp == stamp {
		return cached, nil
	}

	opts := c.opts
	opts.ConfigPath = configPath
	built, err := newEnvironment(opts)
	if err != nil {
		return nil, err
	}
	built.stamp = stamp

	c.byPath[configPath] = built
	return built, nil
}
