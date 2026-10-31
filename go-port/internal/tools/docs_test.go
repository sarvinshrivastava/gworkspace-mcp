package tools

// docs_test.go — per-tool parity tests for the Docs tools, patterned on
// listdrivefiles_test.go: fake Google records the exact request, exact
// output text asserted, zod-parity validation branches, HTTP error branches,
// and the retry-once-on-401 contract. No production credentials.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func withDocsBase(t *testing.T, base string) {
	t.Helper()
	old := DocsBase
	DocsBase = base
	t.Cleanup(func() { DocsBase = old })
}

// withBothBases points Drive and Docs at the same fake server.
func withBothBases(t *testing.T, base string) {
	t.Helper()
	withDriveBase(t, base)
	withDocsBase(t, base)
}

func lastQuery(t *testing.T, fg *fakeGoogle) url.Values {
	t.Helper()
	q, err := url.ParseQuery(fg.lastRequest().Query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	return q
}

func wantUserError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", want)
	}
	ue, ok := err.(*UserError)
	if !ok {
		t.Fatalf("err = %v (not a UserError)", err)
	}
	if ue.Msg != want {
		t.Fatalf("msg = %q, want %q", ue.Msg, want)
	}
}

const smallDocJSON = `{"documentId":"doc1","title":"T","body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Hi\n"}}]},"startIndex":1,"endIndex":4}]}}`

// ---------- readDocument ----------

func TestReadDocument_TextHappy(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, smallDocJSON)
	})
	withBothBases(t, fg.server.URL)
	out, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodGet || req.Path != "/documents/doc1" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	q := lastQuery(t, fg)
	if got := q.Get("fields"); got != "body(content(paragraph(elements(textRun(content)))))" {
		t.Fatalf("fields = %q", got)
	}
	if out != "Content (3 characters):\n---\nHi\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestReadDocument_JSONFormat(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, smallDocJSON)
	})
	withBothBases(t, fg.server.URL)
	out, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1","format":"json"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if got := lastQuery(t, fg).Get("fields"); got != "*" {
		t.Fatalf("fields = %q", got)
	}
	// jsStringifyRaw: 2-space indent, no HTML escaping, original key order.
	want := "{\n  \"documentId\": \"doc1\",\n  \"title\": \"T\","
	if !strings.HasPrefix(out, want) {
		t.Fatalf("out prefix = %q", out[:min(len(out), len(want))])
	}
}

func TestReadDocument_MarkdownFormat(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"body":{"content":[{"paragraph":{"paragraphStyle":{"namedStyleType":"HEADING_1"},"elements":[{"textRun":{"content":"Title\n"}}]}}]}}`)
	})
	withBothBases(t, fg.server.URL)
	out, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1","format":"markdown"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if out != "# Title" {
		t.Fatalf("out = %q", out)
	}
}

func TestReadDocument_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, smallDocJSON)
	})
	withBothBases(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing documentId", `{}`, `Required: "documentId"`},
		{"unknown key", `{"documentId":"d","bogus":1}`, `Unrecognized key: "bogus"`},
		{"bad format enum", `{"documentId":"d","format":"html"}`, `Invalid enum value for "format": "html"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadDocument(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

func TestReadDocument_HTTPBranches(t *testing.T) {
	t.Run("404", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"File not found"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1"}`))
		wantUserError(t, err, "Doc not found (ID: doc1).")
	})
	t.Run("403 export fallback succeeds for text", func(t *testing.T) {
		calls := 0
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				cannedJSON(w, 403, `{"error":{"message":"forbidden"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("Hi\n"))
		})
		withBothBases(t, fg.server.URL)
		out, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1"}`))
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d", calls)
		}
		req := fg.lastRequest()
		if req.Path != "/files/doc1/export" || lastQuery(t, fg).Get("mimeType") != "text/plain" {
			t.Fatalf("export request = %s?%s", req.Path, req.Query)
		}
		if out != "Content (3 characters):\n---\nHi\n" {
			t.Fatalf("out = %q", out)
		}
	})
	t.Run("403 export fallback fails", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"forbidden"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1"}`))
		wantUserError(t, err, "Permission denied for doc (ID: doc1). The Google Docs API may be restricted by your Workspace admin.")
	})
	t.Run("403 non-text format no fallback", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"forbidden"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1","format":"json"}`))
		wantUserError(t, err, "Permission denied for doc (ID: doc1). The Google Docs API may be restricted by your Workspace admin.")
		if len(fg.requests) != 1 {
			t.Fatalf("requests = %d, want 1 (no export attempt)", len(fg.requests))
		}
	})
	t.Run("500 generic with code", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"code":500,"message":"backend broke"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := ReadDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"documentId":"doc1"}`))
		wantUserError(t, err, "Failed to read doc: backend broke (Code: 500)")
	})
	t.Run("401 refresh once", func(t *testing.T) {
		tok := newFakeTokenSource()
		calls := 0
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				cannedJSON(w, 401, `{"error":{"message":"Unauthorized"}}`)
				return
			}
			cannedJSON(w, 200, smallDocJSON)
		})
		withBothBases(t, fg.server.URL)
		_, err := ReadDocument(context.Background(), fg.googleFor(tok), mustArgs(t, `{"documentId":"doc1"}`))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
	})
}

// ---------- listDocuments ----------

const docsFilesPage = `{"files":[{"id":"d1","name":"Notes","modifiedTime":"2026-01-02T03:04:05.000Z","webViewLink":"https://docs.example/d1","owners":[{"displayName":"Ada"}]},{"id":"d2","name":"NoOwner","modifiedTime":"2026-01-01T00:00:00.000Z"}]}`

func TestListDocuments_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, docsFilesPage)
	})
	withDriveBase(t, fg.server.URL)
	out, err := ListDocuments(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	q := lastQuery(t, fg)
	if got := q.Get("q"); got != "mimeType='application/vnd.google-apps.document' and trashed=false" {
		t.Fatalf("q = %q", got)
	}
	if got := q.Get("pageSize"); got != "20" {
		t.Fatalf("pageSize = %q", got)
	}
	if got := q.Get("orderBy"); got != "modifiedTime" {
		t.Fatalf("orderBy = %q", got)
	}
	if got := q.Get("fields"); got != "files(id,name,modifiedTime,createdTime,size,webViewLink,owners(displayName,emailAddress))" {
		t.Fatalf("fields = %q", got)
	}
	want := `{
  "documents": [
    {
      "id": "d1",
      "name": "Notes",
      "modifiedTime": "2026-01-02T03:04:05.000Z",
      "owner": "Ada",
      "url": "https://docs.example/d1"
    },
    {
      "id": "d2",
      "name": "NoOwner",
      "modifiedTime": "2026-01-01T00:00:00.000Z",
      "owner": null
    }
  ]
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestListDocuments_QueryAndModifiedAfter(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[]}`)
	})
	withDriveBase(t, fg.server.URL)
	_, err := ListDocuments(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"query":"quar'ter","orderBy":"name","modifiedAfter":"2024-01-01","maxResults":5}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	q := lastQuery(t, fg)
	wantQ := "mimeType='application/vnd.google-apps.document' and trashed=false and (name contains 'quar\\'ter' or fullText contains 'quar\\'ter') and modifiedTime > '2024-01-01T00:00:00.000Z'"
	if got := q.Get("q"); got != wantQ {
		t.Fatalf("q = %q, want %q", got, wantQ)
	}
	if got := q.Get("orderBy"); got != "name" {
		t.Fatalf("orderBy = %q", got)
	}
	if got := q.Get("pageSize"); got != "5" {
		t.Fatalf("pageSize = %q", got)
	}
}

func TestListDocuments_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"unknown key", `{"bogus":1}`, `Unrecognized key: "bogus"`},
		{"maxResults over max", `{"maxResults":101}`, `Number must be less than or equal to 100: "maxResults"`},
		{"bad orderBy enum", `{"orderBy":"size"}`, `Invalid enum value for "orderBy": "size"`},
		{"bad modifiedAfter", `{"modifiedAfter":"not-a-date"}`, "Failed to list documents: Invalid time value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListDocuments(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

func TestListDocuments_HTTPBranches(t *testing.T) {
	t.Run("403", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := ListDocuments(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
		wantUserError(t, err, "Permission denied. Make sure you have granted Google Drive access to the application.")
	})
	t.Run("500", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"boom"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := ListDocuments(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
		wantUserError(t, err, "Failed to list documents: boom")
	})
}

// ---------- searchDocuments ----------

func TestSearchDocuments_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, docsFilesPage)
	})
	withDriveBase(t, fg.server.URL)
	out, err := SearchDocuments(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"query":"budget","modifiedAfter":"2024-06-01"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	q := lastQuery(t, fg)
	// modifiedAfter is sent RAW (upstream pins the string; no ISO conversion).
	wantQ := "mimeType='application/vnd.google-apps.document' and trashed=false and (name contains 'budget' or fullText contains 'budget') and modifiedTime > '2024-06-01'"
	if got := q.Get("q"); got != wantQ {
		t.Fatalf("q = %q, want %q", got, wantQ)
	}
	if got := q.Get("pageSize"); got != "10" {
		t.Fatalf("pageSize = %q", got)
	}
	if got := q.Get("orderBy"); got != "modifiedTime desc" {
		t.Fatalf("orderBy = %q", got)
	}
	if !strings.HasPrefix(out, "{\n  \"documents\": [") {
		t.Fatalf("out prefix wrong: %q", out[:30])
	}
}

func TestSearchDocuments_SearchIn(t *testing.T) {
	for _, tc := range []struct {
		searchIn, wantFrag string
	}{
		{"name", " and name contains 'x'"},
		{"content", " and fullText contains 'x'"},
	} {
		t.Run(tc.searchIn, func(t *testing.T) {
			fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
				cannedJSON(w, 200, `{"files":[]}`)
			})
			withDriveBase(t, fg.server.URL)
			_, err := SearchDocuments(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
				`{"query":"x","searchIn":"`+tc.searchIn+`"}`))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if got := lastQuery(t, fg).Get("q"); !strings.HasSuffix(got, tc.wantFrag) {
				t.Fatalf("q = %q", got)
			}
		})
	}
}

func TestSearchDocuments_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing query", `{}`, `Required: "query"`},
		{"empty query", `{"query":""}`, `String must contain at least 1 character(s): "query"`},
		{"maxResults over max", `{"query":"x","maxResults":51}`, `Number must be less than or equal to 50: "maxResults"`},
		{"bad searchIn", `{"query":"x","searchIn":"everything"}`, `Invalid enum value for "searchIn": "everything"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SearchDocuments(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

// ---------- appendText ----------

func TestAppendText_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			cannedJSON(w, 200, `{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Hi\n"}}]},"startIndex":1,"endIndex":4}]},"documentStyle":{"pageSize":{}}}`)
			return
		}
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withBothBases(t, fg.server.URL)
	out, err := AppendText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","text":"more"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if out != "Successfully appended text to document doc1." {
		t.Fatalf("out = %q", out)
	}
	if len(fg.requests) != 2 {
		t.Fatalf("requests = %d", len(fg.requests))
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPost || req.Path != "/documents/doc1:batchUpdate" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	wantBody := `{"requests":[{"insertText":{"location":{"index":3},"text":"\nmore"}}]}`
	if req.Body != wantBody {
		t.Fatalf("body = %s, want %s", req.Body, wantBody)
	}
}

func TestAppendText_NoNewlineWhenDisabled(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			cannedJSON(w, 200, `{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Hi\n"}}]},"startIndex":1,"endIndex":4}]},"documentStyle":{"pageSize":{}}}`)
			return
		}
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withBothBases(t, fg.server.URL)
	_, err := AppendText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","text":"more","addNewlineIfNeeded":false}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	wantBody := `{"requests":[{"insertText":{"location":{"index":3},"text":"more"}}]}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
}

func TestAppendText_EmptyDoc(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			cannedJSON(w, 200, `{"body":{"content":[{"endIndex":1}]},"documentStyle":{"pageSize":{}}}`)
			return
		}
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withBothBases(t, fg.server.URL)
	_, err := AppendText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","text":"first"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	// upstream: lastElement.endIndex (1) is truthy → endIndex = 1-1 = 0; no
	// leading newline since endIndex > 1 is false. Parity quirk: index 0.
	wantBody := `{"requests":[{"insertText":{"location":{"index":0},"text":"first"}}]}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
}

func TestAppendText_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{}`)
	})
	withBothBases(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing documentId", `{"text":"x"}`, `Required: "documentId"`},
		{"missing text", `{"documentId":"d"}`, `Required: "text"`},
		{"unknown key", `{"documentId":"d","text":"x","bogus":1}`, `Unrecognized key: "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AppendText(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

func TestAppendText_HTTPBranches(t *testing.T) {
	t.Run("get 404 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"not here"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := AppendText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
			`{"documentId":"doc1","text":"x"}`))
		wantUserError(t, err, "Failed to append to doc: not here")
	})
	t.Run("batchUpdate 404", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				cannedJSON(w, 200, `{"body":{"content":[{"endIndex":2}]},"documentStyle":{"pageSize":{}}}`)
				return
			}
			cannedJSON(w, 404, `{"error":{"message":"no doc"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := AppendText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
			`{"documentId":"doc1","text":"x"}`))
		wantUserError(t, err, "Document not found (ID: doc1). Check the ID.")
	})
	t.Run("batchUpdate 400 invalid requests", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				cannedJSON(w, 200, `{"body":{"content":[{"endIndex":2}]},"documentStyle":{"pageSize":{}}}`)
				return
			}
			cannedJSON(w, 400, `{"error":{"message":"Invalid requests[0].insertText: bad","details":[{"description":"Index must be at least 1"},{"description":"Second issue"}]}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := AppendText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
			`{"documentId":"doc1","text":"x"}`))
		wantUserError(t, err, "Invalid request sent to Google Docs API. Details: Index must be at least 1; Second issue")
	})
	t.Run("batchUpdate 401 refresh once", func(t *testing.T) {
		tok := newFakeTokenSource()
		calls := 0
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				cannedJSON(w, 200, `{"body":{"content":[{"endIndex":2}]},"documentStyle":{"pageSize":{}}}`)
				return
			}
			calls++
			if calls == 1 {
				cannedJSON(w, 401, `{"error":{"message":"Unauthorized"}}`)
				return
			}
			cannedJSON(w, 200, `{"replies":[{}]}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := AppendText(context.Background(), fg.googleFor(tok), mustArgs(t,
			`{"documentId":"doc1","text":"x"}`))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
	})
}

// ---------- insertText ----------

func TestInsertText_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withDocsBase(t, fg.server.URL)
	out, err := InsertText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","text":"hello","index":5}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if out != "Successfully inserted text at index 5." {
		t.Fatalf("out = %q", out)
	}
	wantBody := `{"requests":[{"insertText":{"location":{"index":5},"text":"hello"}}]}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
	if fg.lastRequest().Path != "/documents/doc1:batchUpdate" {
		t.Fatalf("path = %s", fg.lastRequest().Path)
	}
}

func TestInsertText_EmptyTextRejected(t *testing.T) {
	// Upstream zod pins text.min(1), so the helper's empty-text short-circuit
	// is unreachable through tool args; validation rejects first.
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withDocsBase(t, fg.server.URL)
	_, err := InsertText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","text":"","index":5}`))
	wantUserError(t, err, `String must contain at least 1 character(s): "text"`)
	if len(fg.requests) != 0 {
		t.Fatalf("requests = %d, want 0", len(fg.requests))
	}
}

func TestInsertText_WithTab(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			cannedJSON(w, 200, `{"tabs":[{"tabProperties":{"tabId":"tab9"},"documentTab":{"body":{"content":[{"endIndex":2}]}}}]}`)
			return
		}
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withBothBases(t, fg.server.URL)
	out, err := InsertText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","text":"hi","index":3,"tabId":"tab9"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if out != "Successfully inserted text at index 3 in tab tab9." {
		t.Fatalf("out = %q", out)
	}
	wantBody := `{"requests":[{"insertText":{"location":{"index":3,"tabId":"tab9"},"text":"hi"}}]}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
}

func TestInsertText_TabNotFound(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"tabs":[{"tabProperties":{"tabId":"other"},"documentTab":{"body":{"content":[]}}}]}`)
	})
	withBothBases(t, fg.server.URL)
	_, err := InsertText(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","text":"hi","index":3,"tabId":"tab9"}`))
	wantUserError(t, err, `Tab with ID "tab9" not found in document.`)
}

func TestInsertText_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withDocsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing index", `{"documentId":"d","text":"x"}`, `Required: "index"`},
		{"fractional index", `{"documentId":"d","text":"x","index":2.5}`, `Expected integer for "index"`},
		{"index below min", `{"documentId":"d","text":"x","index":0}`, `Number must be greater than or equal to 1: "index"`},
		{"missing text", `{"documentId":"d","index":1}`, `Required: "text"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := InsertText(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

// ---------- findAndReplace ----------

func TestFindAndReplace_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"replies":[{"replaceAllText":{"occurrencesChanged":3}}]}`)
	})
	withDocsBase(t, fg.server.URL)
	out, err := FindAndReplace(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","findText":"foo","replaceText":"bar"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if out != `Replaced 3 occurrence(s) of "foo" with "bar".` {
		t.Fatalf("out = %q", out)
	}
	// Key order matches the upstream object literal; matchCase defaults false
	// (upstream `args.matchCase ?? false`); tabsCriteria absent without tabId.
	wantBody := `{"requests":[{"replaceAllText":{"containsText":{"text":"foo","matchCase":false},"replaceText":"bar"}}]}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
}

func TestFindAndReplace_TabAndMatchCase(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"replies":[{"replaceAllText":{}}]}`)
	})
	withDocsBase(t, fg.server.URL)
	out, err := FindAndReplace(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","findText":"a","replaceText":"b","matchCase":false,"tabId":"t1"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if out != `Replaced 0 occurrence(s) of "a" with "b".` {
		t.Fatalf("out = %q", out)
	}
	wantBody := `{"requests":[{"replaceAllText":{"containsText":{"text":"a","matchCase":false},"replaceText":"b","tabsCriteria":{"tabIds":["t1"]}}}]}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
}

func TestFindAndReplace_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withDocsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing findText", `{"documentId":"d","replaceText":"b"}`, `Required: "findText"`},
		{"missing replaceText", `{"documentId":"d","findText":"a"}`, `Required: "replaceText"`},
		{"unknown key", `{"documentId":"d","findText":"a","replaceText":"b","bogus":1}`, `Unrecognized key: "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FindAndReplace(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

func TestFindAndReplace_HTTP403(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 403, `{"error":{"message":"forbidden"}}`)
	})
	withDocsBase(t, fg.server.URL)
	_, err := FindAndReplace(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","findText":"a","replaceText":"b"}`))
	wantUserError(t, err, "Permission denied for document (ID: doc1). Ensure the authenticated user has edit access.")
}

// ---------- appendMarkdown ----------

func TestAppendMarkdown_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			cannedJSON(w, 200, `{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Hi\n"}}]},"startIndex":1,"endIndex":4}]},"documentStyle":{"pageSize":{}}}`)
			return
		}
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withBothBases(t, fg.server.URL)
	out, err := AppendMarkdown(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","markdown":"**bold**"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !strings.HasPrefix(out, "Successfully appended 8 characters of markdown.\n\n") {
		t.Fatalf("out prefix = %q", out)
	}
	// addNewlineIfNeeded default true: batchUpdate #1 inserts "\n\n" at 3;
	// the splitter then sends the insert phase and the format phase as
	// separate batchUpdate calls.
	if len(fg.requests) != 4 {
		t.Fatalf("requests = %d, want 4 (get + newline + insert + format)", len(fg.requests))
	}
	first := fg.requests[1]
	wantFirst := `{"requests":[{"insertText":{"location":{"index":3},"text":"\n\n"}}]}`
	if first.Body != wantFirst {
		t.Fatalf("newline batch = %s, want %s", first.Body, wantFirst)
	}
	insertBatch := fg.requests[2]
	if !strings.Contains(insertBatch.Body, `"insertText":{"location":{"index":5},"text":"bold"}`) {
		t.Fatalf("insert batch = %s", insertBatch.Body)
	}
	formatBatch := fg.requests[3]
	if !strings.Contains(formatBatch.Body, `"bold":true`) {
		t.Fatalf("format batch missing bold style: %s", formatBatch.Body)
	}
}

func TestAppendMarkdown_NoContent(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"documentStyle":{"pageSize":{}}}`)
	})
	withBothBases(t, fg.server.URL)
	_, err := AppendMarkdown(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"documentId":"doc1","markdown":"x"}`))
	wantUserError(t, err, "No content found in document/tab")
}

func TestAppendMarkdown_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{}`)
	})
	withBothBases(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing markdown", `{"documentId":"d"}`, `Required: "markdown"`},
		{"missing documentId", `{"markdown":"x"}`, `Required: "documentId"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AppendMarkdown(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

// ---------- createDocument ----------

func TestCreateDocument_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"newdoc","name":"My Doc","webViewLink":"https://docs.example/newdoc"}`)
	})
	withBothBases(t, fg.server.URL)
	out, err := CreateDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"title":"My Doc","parentFolderId":"fold1"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPost || req.Path != "/files" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	wantBody := `{"name":"My Doc","mimeType":"application/vnd.google-apps.document","parents":["fold1"]}`
	if req.Body != wantBody {
		t.Fatalf("body = %s, want %s", req.Body, wantBody)
	}
	want := `{
  "id": "newdoc",
  "name": "My Doc",
  "url": "https://docs.example/newdoc",
  "contentInserted": false
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestCreateDocument_RawInitialContent(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			cannedJSON(w, 200, `{"id":"newdoc","name":"D","webViewLink":"https://docs.example/newdoc"}`)
			return
		}
		cannedJSON(w, 200, `{"replies":[{}]}`)
	})
	withBothBases(t, fg.server.URL)
	out, err := CreateDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"title":"D","initialContent":"Hello","contentFormat":"raw"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(fg.requests) != 2 {
		t.Fatalf("requests = %d", len(fg.requests))
	}
	req := fg.lastRequest()
	if req.Path != "/documents/newdoc:batchUpdate" {
		t.Fatalf("path = %s", req.Path)
	}
	wantBody := `{"requests":[{"insertText":{"location":{"index":1},"text":"Hello"}}]}`
	if req.Body != wantBody {
		t.Fatalf("body = %s, want %s", req.Body, wantBody)
	}
	if !strings.Contains(out, `"contentInserted": true`) {
		t.Fatalf("out = %s", out)
	}
}

func TestCreateDocument_InitialContentFails(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			cannedJSON(w, 200, `{"id":"newdoc","name":"D","webViewLink":"https://docs.example/newdoc"}`)
			return
		}
		cannedJSON(w, 500, `{"error":{"message":"batch broke"}}`)
	})
	withBothBases(t, fg.server.URL)
	_, err := CreateDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t,
		`{"title":"D","initialContent":"Hello","contentFormat":"raw"}`))
	wantUserError(t, err, `Document "D" was created (id: newdoc, url: https://docs.example/newdoc) but inserting the initial content FAILED, so it is currently EMPTY. Append the content to this existing document (do not create a new one). Underlying error: batch broke`)
}

func TestCreateDocument_HTTPBranches(t *testing.T) {
	t.Run("404 parent folder", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"File not found"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := CreateDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"title":"T"}`))
		wantUserError(t, err, "Parent folder not found. Check the folder ID.")
	})
	t.Run("403", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"forbidden"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := CreateDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"title":"T"}`))
		wantUserError(t, err, "Permission denied. Make sure you have write access to the destination folder.")
	})
	t.Run("500", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"exploded"}}`)
		})
		withBothBases(t, fg.server.URL)
		_, err := CreateDocument(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"title":"T"}`))
		wantUserError(t, err, "Failed to create document: exploded")
	})
}

func TestCreateDocument_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"x"}`)
	})
	withBothBases(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing title", `{}`, `Required: "title"`},
		{"empty title", `{"title":""}`, `String must contain at least 1 character(s): "title"`},
		{"bad contentFormat", `{"title":"T","contentFormat":"html"}`, `Invalid enum value for "contentFormat": "html"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateDocument(context.Background(), g, mustArgs(t, tc.args))
			wantUserError(t, err, tc.wantMsg)
		})
	}
}

// ---------- registry ----------

func TestDocsHandlers_RegistryCoversTools(t *testing.T) {
	h := DocsHandlers()
	for _, name := range []string{
		"readDocument", "listDocuments", "searchDocuments",
		"appendText", "insertText", "findAndReplace", "appendMarkdown", "createDocument",
	} {
		if h[name] == nil {
			t.Fatalf("registry missing %q", name)
		}
	}
	if len(h) != 8 {
		t.Fatalf("registry has %d handlers, want 8", len(h))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
