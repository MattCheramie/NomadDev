package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/docfetch"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// fakeDocFetcher is a DocFetcher that performs no network IO, so the
// dispatcher's fetch_external_docs routing can be tested deterministically.
type fakeDocFetcher struct {
	res docfetch.Result
	err error
}

func (f fakeDocFetcher) Fetch(_ context.Context, _ string) (docfetch.Result, error) {
	return f.res, f.err
}

func drainDocChunks(ch <-chan sandbox.ExecChunk) (stdout []byte, exit sandbox.ExecChunk) {
	for c := range ch {
		switch c.Stream {
		case sandbox.StreamStdout:
			stdout = append(stdout, c.Data...)
		case sandbox.StreamExit:
			exit = c
		}
	}
	return stdout, exit
}

func TestDispatcher_FetchExternalDocs_NilBackend(t *testing.T) {
	d := &CompositeDispatcher{}
	_, err := d.Dispatch(context.Background(),
		ToolCall{ID: "c1", Tool: ToolFetchExternalDocs, Args: map[string]any{"url": "https://example.com"}},
		DispatchOptions{})
	if err == nil || !errors.Is(err, sandbox.ErrBadRequest) {
		t.Fatalf("nil Docs: want ErrBadRequest, got %v", err)
	}
}

func TestDispatcher_FetchExternalDocs_StreamsResult(t *testing.T) {
	want := docfetch.Result{
		URL:      "https://example.com/doc",
		FinalURL: "https://example.com/doc",
		Markdown: "# Title\n\nbody text",
	}
	d := &CompositeDispatcher{Docs: fakeDocFetcher{res: want}}
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolFetchExternalDocs, Args: map[string]any{"url": want.URL}},
		DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	stdout, exit := drainDocChunks(ch)
	if exit.ExitCode != 0 || exit.Err != nil {
		t.Fatalf("exit = %+v, want a clean exit 0", exit)
	}
	var got docfetch.Result
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("stdout is not a docfetch.Result envelope: %v\n%s", err, stdout)
	}
	if got.Markdown != want.Markdown || got.FinalURL != want.FinalURL {
		t.Errorf("envelope = %+v, want %+v", got, want)
	}
}

func TestDispatcher_FetchExternalDocs_FetchError(t *testing.T) {
	d := &CompositeDispatcher{Docs: fakeDocFetcher{err: docfetch.ErrBlockedTarget}}
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolFetchExternalDocs, Args: map[string]any{"url": "http://10.0.0.1"}},
		DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_, exit := drainDocChunks(ch)
	if exit.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1", exit.ExitCode)
	}
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Errorf("exit.Err = %v, want a wrap of sandbox.ErrBadRequest", exit.Err)
	}
}

func TestDispatcher_FetchExternalDocs_MissingURL(t *testing.T) {
	d := &CompositeDispatcher{Docs: fakeDocFetcher{}}
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolFetchExternalDocs, Args: map[string]any{}},
		DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_, exit := drainDocChunks(ch)
	if exit.ExitCode != -1 || !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("missing url: exit = %+v, want -1 / ErrBadRequest", exit)
	}
}

func TestDispatcher_FetchExternalDocs_AuditModeAllowed(t *testing.T) {
	// fetch_external_docs is read-only, so audit mode must not refuse it.
	d := &CompositeDispatcher{Docs: fakeDocFetcher{res: docfetch.Result{Markdown: "ok"}}}
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolFetchExternalDocs, Args: map[string]any{"url": "https://example.com"}},
		DispatchOptions{Mode: ModeAudit})
	if err != nil {
		t.Fatalf("audit mode refused read-only fetch_external_docs: %v", err)
	}
	if _, exit := drainDocChunks(ch); exit.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exit.ExitCode)
	}
}
