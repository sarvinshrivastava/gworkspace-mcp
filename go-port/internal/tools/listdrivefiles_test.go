package tools

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Reference test: every handler test follows this shape — fake Google records
// the exact request, asserts the exact output text, and covers the zod-parity
// argument validation branches.

func withDriveBase(t *testing.T, base string) {
	t.Helper()
	old := DriveBase
	DriveBase = base
	t.Cleanup(func() { DriveBase = old })
}

func TestListDriveFiles_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[{"id":"f1","name":"Notes","mimeType":"application/vnd.google-apps.document","size":"1234","modifiedTime":"2026-01-02T03:04:05.000Z","createdTime":"2026-01-01T00:00:00.000Z","webViewLink":"https://docs.example/f1","owners":[{"displayName":"Ada"}]},{"id":"f2","name":"NoOwner","mimeType":"application/pdf","webViewLink":"https://docs.example/f2"}]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := ListDriveFiles(context.Background(), g, mustArgs(t, `{}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Auth != "Bearer fake-token" {
		t.Fatalf("auth header = %q", req.Auth)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("q"); got != "trashed=false" {
		t.Fatalf("q = %q", got)
	}
	if got := q.Get("pageSize"); got != "20" { // zod default, applied by the handler
		t.Fatalf("pageSize = %q", got)
	}
	if got := q.Get("orderBy"); got != "modifiedTime desc" { // zod defaults
		t.Fatalf("orderBy = %q", got)
	}
	if got := q.Get("supportsAllDrives"); got != "true" {
		t.Fatalf("supportsAllDrives = %q", got)
	}

	want := `{
  "files": [
    {
      "id": "f1",
      "name": "Notes",
      "mimeType": "application/vnd.google-apps.document",
      "size": 1234,
      "modifiedTime": "2026-01-02T03:04:05.000Z",
      "createdTime": "2026-01-01T00:00:00.000Z",
      "owner": "Ada",
      "url": "https://docs.example/f1"
    },
    {
      "id": "f2",
      "name": "NoOwner",
      "mimeType": "application/pdf",
      "size": null,
      "owner": null,
      "url": "https://docs.example/f2"
    }
  ],
  "total": 2
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestListDriveFiles_Filters(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	_, err := ListDriveFiles(context.Background(), g, mustArgs(t,
		`{"maxResults":5,"mimeType":"document","folderId":"fold'er","sharedWithMe":true,"modifiedAfter":"2024-01-01","orderBy":"name","sortDirection":"asc"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	q, _ := url.ParseQuery(fg.lastRequest().Query)
	wantQ := "trashed=false and mimeType='application/vnd.google-apps.document' and 'fold\\'er' in parents and sharedWithMe=true and modifiedTime > '2024-01-01T00:00:00.000Z'"
	if got := q.Get("q"); got != wantQ {
		t.Fatalf("q = %q, want %q", got, wantQ)
	}
	if got := q.Get("orderBy"); got != "name" { // asc is Google's default direction: upstream omits " asc"
		t.Fatalf("orderBy = %q", got)
	}
}

func TestListDriveFiles_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"owned+shared conflict", `{"ownedByMe":true,"sharedWithMe":true}`, "ownedByMe and sharedWithMe cannot both be true."},
		{"unknown key", `{"bogus":1}`, `Unrecognized key: "bogus"`},
		{"fractional maxResults", `{"maxResults":2.5}`, `Expected integer for "maxResults"`},
		{"maxResults over max", `{"maxResults":101}`, `Number must be less than or equal to 100: "maxResults"`},
		{"bad enum", `{"orderBy":"size"}`, `Invalid enum value for "orderBy": "size"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListDriveFiles(ctx, g, mustArgs(t, tc.args))
			ue, ok := err.(*UserError)
			if !ok {
				t.Fatalf("err = %v (not a UserError)", err)
			}
			if ue.Msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", ue.Msg, tc.wantMsg)
			}
		})
	}
}

func TestListDriveFiles_HTTPBranches(t *testing.T) {
	t.Run("403 permission", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := ListDriveFiles(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
		ue := err.(*UserError)
		if ue.Msg != "Permission denied. Make sure you have granted Google Drive access to the application." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := ListDriveFiles(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
		ue := err.(*UserError)
		if ue.Msg != "Failed to list files: Backend exploded" {
			t.Fatalf("msg = %q", ue.Msg)
		}
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
			cannedJSON(w, 200, `{"files":[]}`)
		})
		withDriveBase(t, fg.server.URL)
		out, err := ListDriveFiles(context.Background(), fg.googleFor(tok), nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"total": 0`) {
			t.Fatalf("out = %s", out)
		}
	})
}
