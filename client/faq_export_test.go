package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The server already exports FAQ entries as a JSON array in the import payload
// shape, which is what a caller needs to snapshot a knowledge base and feed it
// back to an import. The SDK only asked for the CSV, so a Go caller had to
// parse a spreadsheet to get something it could re-submit.
func TestExportFAQEntriesJSONAsksForTheJSONFormat(t *testing.T) {
	const body = `[{"standard_question":"q","answers":["a"],"tag_name":"t"}]`
	var gotPath, gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFormat = r.URL.Query().Get("format")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	data, err := NewClient(srv.URL).ExportFAQEntriesJSON(context.Background(), "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if want := fmt.Sprintf("/api/v1/knowledge-bases/%s/faq/entries/export", "kb-1"); gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if gotFormat != "json" {
		t.Fatalf("format = %q, want json", gotFormat)
	}
	if string(data) != body {
		t.Fatalf("body = %q, want the server's bytes unchanged", string(data))
	}
}

// The CSV method must keep asking for the default format: changing it would
// hand CSV consumers a JSON document.
func TestExportFAQEntriesStaysCSV(t *testing.T) {
	var gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFormat = r.URL.Query().Get("format")
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		_, _ = w.Write([]byte("分类,问题\n"))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL).ExportFAQEntries(context.Background(), "kb-1"); err != nil {
		t.Fatalf("export: %v", err)
	}
	if gotFormat != "" {
		t.Fatalf("format = %q, want the flag left unset", gotFormat)
	}
}
