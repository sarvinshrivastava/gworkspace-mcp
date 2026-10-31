package tools

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Drive tools tests (createFolder) — same shape as the listDriveFiles
// exemplar: exact request, exact output, every zod/HTTP branch, 401 retry.

func TestCreateFolder_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"fld1","name":"New Folder","parents":["abc123DEF_"],"webViewLink":"https://drive.example/fld1"}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := CreateFolder(context.Background(), g, mustArgs(t, `{"name":"New Folder","parentFolderId":"abc123DEF_"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPost {
		t.Fatalf("method = %q", req.Method)
	}
	if req.Path != "/files" {
		t.Fatalf("path = %q", req.Path)
	}
	if req.Auth != "Bearer fake-token" {
		t.Fatalf("auth = %q", req.Auth)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("fields"); got != "id,name,parents,webViewLink" {
		t.Fatalf("fields = %q", got)
	}
	if got := q.Get("supportsAllDrives"); got != "true" {
		t.Fatalf("supportsAllDrives = %q", got)
	}
	wantBody := `{"name":"New Folder","mimeType":"application/vnd.google-apps.folder","parents":["abc123DEF_"]}`
	if req.Body != wantBody {
		t.Fatalf("body = %s, want %s", req.Body, wantBody)
	}

	want := `{
  "id": "fld1",
  "name": "New Folder",
  "url": "https://drive.example/fld1"
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestCreateFolder_RootOmitsParents(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"fld2","name":"Root Folder","webViewLink":"https://drive.example/fld2"}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	_, err := CreateFolder(context.Background(), g, mustArgs(t, `{"name":"Root Folder"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	wantBody := `{"name":"Root Folder","mimeType":"application/vnd.google-apps.folder"}`
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}

	// An explicit "" is falsy upstream too: no parents key.
	_, err = CreateFolder(context.Background(), g, mustArgs(t, `{"name":"Root Folder","parentFolderId":""}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if got := fg.lastRequest().Body; got != wantBody {
		t.Fatalf("body (empty parentFolderId) = %s, want %s", got, wantBody)
	}
}

func TestCreateFolder_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"x"}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"missing name", `{}`, `Required: "name"`},
		{"null name", `{"name":null}`, `Expected string, received null: "name"`},
		{"empty name", `{"name":""}`, `String must contain at least 1 character(s): "name"`},
		{"non-string name", `{"name":7}`, `Expected string for "name"`},
		{"null parentFolderId", `{"name":"n","parentFolderId":null}`, `Expected string, received null: "parentFolderId"`},
		{"unknown key", `{"name":"n","bogus":1}`, `Unrecognized key: "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateFolder(ctx, g, mustArgs(t, tc.args))
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

func TestCreateFolder_HTTPBranches(t *testing.T) {
	t.Run("404 parent missing", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"File not found"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := CreateFolder(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"name":"n","parentFolderId":"abc123DEF_"}`))
		ue := err.(*UserError)
		if ue.Msg != "Parent folder not found. Check the parent folder ID." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403 permission", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := CreateFolder(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"name":"n"}`))
		ue := err.(*UserError)
		if ue.Msg != "Permission denied. Make sure you have write access to the parent folder." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := CreateFolder(context.Background(), fg.googleFor(newFakeTokenSource()), mustArgs(t, `{"name":"n"}`))
		ue := err.(*UserError)
		if ue.Msg != "Failed to create folder: Backend exploded" {
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
			cannedJSON(w, 200, `{"id":"fld9","name":"n","webViewLink":"u"}`)
		})
		withDriveBase(t, fg.server.URL)
		out, err := CreateFolder(context.Background(), fg.googleFor(tok), mustArgs(t, `{"name":"n"}`))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"id": "fld9"`) {
			t.Fatalf("out = %s", out)
		}
	})
}
