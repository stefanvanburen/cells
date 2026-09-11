package lsp

// This test is in package lsp rather than lsp_test because what it checks —
// which environments the server still holds — has no shape on the wire.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/ok"
)

// configuredDir writes a cel.yaml to a new directory and returns its path
// along with the path of a document beside it. The document itself is never
// written: its content arrives in didOpen, and the search for the
// configuration above it only looks at the directory.
func configuredDir(t *testing.T, name string) (configPath, celPath string) {
	t.Helper()
	dir := t.TempDir()

	configPath = filepath.Join(dir, ConfigFileName)
	ok.MustNoError(t, os.WriteFile(configPath, []byte("name: "+name+"\n"), 0o600))

	return configPath, filepath.Join(dir, name+".cel")
}

// clientContext carries a client, which is what makes DidOpen publish
// diagnostics — and publishing is what has the server build the document's
// environment.
func clientContext() context.Context {
	return protocol.WithClient(context.Background(), protocol.UnimplementedClient{})
}

func openDoc(t *testing.T, s *server, path string) uri.URI {
	t.Helper()

	docURI := uri.File(path)
	ok.MustNoError(t, s.DidOpen(clientContext(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: docURI, LanguageID: "cel", Version: 1, Text: "1 + 2\n",
		},
	}))
	return docURI
}

func closeDoc(t *testing.T, s *server, docURI uri.URI) {
	t.Helper()

	ok.MustNoError(t, s.DidClose(clientContext(), &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
	}))
}

func TestEnvsReleasedWhenLastDocumentCloses(t *testing.T) {
	t.Parallel()

	firstConfig, firstCEL := configuredDir(t, "first")
	secondConfig, secondCEL := configuredDir(t, "second")

	s, err := newServer(Options{})
	ok.MustNoError(t, err)

	cached := func(configPath string) bool {
		t.Helper()
		_, ok := s.envs.byPath[configPath]
		return ok
	}

	firstURI, secondURI := openDoc(t, s, firstCEL), openDoc(t, s, secondCEL)
	ok.Equal(t, cached(firstConfig), true)
	ok.Equal(t, cached(secondConfig), true)

	// Closing one document leaves the other's environment alone.
	closeDoc(t, s, firstURI)
	ok.Equal(t, cached(firstConfig), false)
	ok.Equal(t, cached(secondConfig), true)

	closeDoc(t, s, secondURI)
	ok.Equal(t, cached(secondConfig), false)

	// The environment the server was started with survives having nothing
	// open: it governs every document, whatever configuration is found above.
	ok.Equal(t, cached(""), true)
}

func TestEnvForNamedConfigSurvivesClose(t *testing.T) {
	t.Parallel()

	configPath, celPath := configuredDir(t, "named")
	s, err := newServer(Options{ConfigPath: configPath})
	ok.MustNoError(t, err)

	closeDoc(t, s, openDoc(t, s, celPath))

	_, cached := s.envs.byPath[configPath]
	ok.Equal(t, cached, true)
}
