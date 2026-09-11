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

// configuredDir writes a cel.yaml and a .cel file beside it, returning the
// path of each.
func configuredDir(t *testing.T, name string) (configPath string, celPath string) {
	t.Helper()
	dir := t.TempDir()

	configPath = filepath.Join(dir, ConfigFileName)
	ok.MustNoError(t, os.WriteFile(configPath, []byte("name: "+name+"\n"), 0o600))

	celPath = filepath.Join(dir, name+".cel")
	ok.MustNoError(t, os.WriteFile(celPath, []byte("1 + 2\n"), 0o600))

	return configPath, celPath
}

func TestEnvsReleasedWhenLastDocumentCloses(t *testing.T) {
	t.Parallel()

	// A client on the context is what makes DidOpen publish diagnostics, and
	// publishing is what has the server build the document's environment.
	ctx := protocol.WithClient(context.Background(), protocol.UnimplementedClient{})

	firstConfig, firstCEL := configuredDir(t, "first")
	secondConfig, secondCEL := configuredDir(t, "second")

	s, err := newServer(Options{})
	ok.MustNoError(t, err)

	open := func(path string) uri.URI {
		t.Helper()
		docURI := uri.File(path)
		ok.MustNoError(t, s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI: docURI, LanguageID: "cel", Version: 1, Text: "1 + 2\n",
			},
		}))
		return docURI
	}
	closeDoc := func(docURI uri.URI) {
		t.Helper()
		ok.MustNoError(t, s.DidClose(ctx, &protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		}))
	}
	cached := func(configPath string) bool {
		t.Helper()
		_, ok := s.envs.byPath[configPath]
		return ok
	}

	firstURI, secondURI := open(firstCEL), open(secondCEL)
	ok.Equal(t, cached(firstConfig), true)
	ok.Equal(t, cached(secondConfig), true)

	// Closing one document leaves the other's environment alone.
	closeDoc(firstURI)
	ok.Equal(t, cached(firstConfig), false)
	ok.Equal(t, cached(secondConfig), true)

	closeDoc(secondURI)
	ok.Equal(t, cached(secondConfig), false)

	// The environment the server was started with survives having nothing
	// open: it governs every document, whatever configuration is found above.
	ok.Equal(t, cached(""), true)
}

func TestEnvForNamedConfigSurvivesClose(t *testing.T) {
	t.Parallel()

	ctx := protocol.WithClient(context.Background(), protocol.UnimplementedClient{})

	configPath, celPath := configuredDir(t, "named")
	s, err := newServer(Options{ConfigPath: configPath})
	ok.MustNoError(t, err)

	docURI := uri.File(celPath)
	ok.MustNoError(t, s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: docURI, LanguageID: "cel", Version: 1, Text: "1 + 2\n",
		},
	}))
	ok.MustNoError(t, s.DidClose(ctx, &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
	}))

	_, cached := s.envs.byPath[configPath]
	ok.Equal(t, cached, true)
}
