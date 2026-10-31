package tools

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func withSheetsBase(t *testing.T, base string) {
	t.Helper()
	old := SheetsBase
	SheetsBase = base
	t.Cleanup(func() { SheetsBase = old })
}

// ---------- readSpreadsheet ----------

func TestReadSpreadsheet_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"range":"Sheet1!A1:B2","majorDimension":"ROWS","values":[["a","b"],["c","d"]]}`)
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := ReadSpreadsheet(context.Background(), g, mustArgs(t, `{"spreadsheetId":"sid","range":"Sheet1!A1:B2"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodGet {
		t.Fatalf("method = %q", req.Method)
	}
	if req.Auth != "Bearer fake-token" {
		t.Fatalf("auth = %q", req.Auth)
	}
	// googleapis encodeURIComponent's the range path param (: -> %3A, ! kept);
	// the recorded path is the decoded form.
	if req.Path != "/spreadsheets/sid/values/Sheet1!A1:B2" {
		t.Fatalf("path = %q", req.Path)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("valueRenderOption"); got != "FORMATTED_VALUE" { // zod default applied by handler
		t.Fatalf("valueRenderOption = %q", got)
	}
	want := `{
  "range": "Sheet1!A1:B2",
  "values": [
    [
      "a",
      "b"
    ],
    [
      "c",
      "d"
    ]
  ]
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestReadSpreadsheet_EmptyValuesAndRenderOption(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"range":"A1:B2","majorDimension":"ROWS"}`)
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := ReadSpreadsheet(context.Background(), g, mustArgs(t,
		`{"spreadsheetId":"sid","range":"A1:B2","valueRenderOption":"FORMULA"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	q, _ := url.ParseQuery(fg.lastRequest().Query)
	if got := q.Get("valueRenderOption"); got != "FORMULA" {
		t.Fatalf("valueRenderOption = %q", got)
	}
	want := "{\n  \"range\": \"A1:B2\",\n  \"values\": []\n}" // values || []
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestReadSpreadsheet_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{}`)
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"unknown key", `{"spreadsheetId":"s","range":"A1","bogus":1}`, `Unrecognized key: "bogus"`},
		{"missing range", `{"spreadsheetId":"s"}`, `Required: "range"`},
		{"missing spreadsheetId", `{"range":"A1"}`, `Required: "spreadsheetId"`},
		{"bad enum", `{"spreadsheetId":"s","range":"A1","valueRenderOption":"HTML"}`, `Invalid enum value for "valueRenderOption": "HTML"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadSpreadsheet(ctx, g, mustArgs(t, tc.args))
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

func TestReadSpreadsheet_HTTPBranches(t *testing.T) {
	t.Run("404", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"Requested entity was not found."}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := ReadSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()),
			mustArgs(t, `{"spreadsheetId":"sid","range":"A1"}`))
		ue := err.(*UserError)
		if ue.Msg != "Spreadsheet not found (ID: sid). Check the ID." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"The caller does not have permission"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := ReadSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()),
			mustArgs(t, `{"spreadsheetId":"sid","range":"A1"}`))
		ue := err.(*UserError)
		if ue.Msg != "Permission denied for spreadsheet (ID: sid). Ensure you have read access." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := ReadSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()),
			mustArgs(t, `{"spreadsheetId":"sid","range":"A1"}`))
		ue := err.(*UserError)
		if ue.Msg != "Failed to read range: Backend exploded" {
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
			cannedJSON(w, 200, `{"values":[["x"]]}`)
		})
		withSheetsBase(t, fg.server.URL)
		out, err := ReadSpreadsheet(context.Background(), fg.googleFor(tok),
			mustArgs(t, `{"spreadsheetId":"sid","range":"A1"}`))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"x"`) {
			t.Fatalf("out = %s", out)
		}
	})
}

// ---------- listSpreadsheets ----------

func TestListSpreadsheets_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[{"id":"s1","name":"Budget","modifiedTime":"2026-01-02T03:04:05.000Z","createdTime":"2026-01-01T00:00:00.000Z","size":"99","webViewLink":"https://sheets.example/s1","owners":[{"displayName":"Ada"}]},{"id":"s2","name":"Orphan","webViewLink":"https://sheets.example/s2"}]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := ListSpreadsheets(context.Background(), g, mustArgs(t, `{}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodGet {
		t.Fatalf("method = %q", req.Method)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("q"); got != "mimeType='application/vnd.google-apps.spreadsheet' and trashed=false" {
		t.Fatalf("q = %q", got)
	}
	if got := q.Get("pageSize"); got != "20" { // zod default
		t.Fatalf("pageSize = %q", got)
	}
	if got := q.Get("orderBy"); got != "modifiedTime" { // NO direction suffix in this tool
		t.Fatalf("orderBy = %q", got)
	}
	if got := q.Get("fields"); got != "files(id,name,modifiedTime,createdTime,size,webViewLink,owners(displayName,emailAddress))" {
		t.Fatalf("fields = %q", got)
	}
	if got := q.Get("supportsAllDrives"); got != "true" {
		t.Fatalf("supportsAllDrives = %q", got)
	}
	if got := q.Get("includeItemsFromAllDrives"); got != "true" {
		t.Fatalf("includeItemsFromAllDrives = %q", got)
	}
	// Output carries id,name,modifiedTime,owner,url only (size/createdTime are
	// fetched but never mapped); owner is always emitted (null).
	want := `{
  "spreadsheets": [
    {
      "id": "s1",
      "name": "Budget",
      "modifiedTime": "2026-01-02T03:04:05.000Z",
      "owner": "Ada",
      "url": "https://sheets.example/s1"
    },
    {
      "id": "s2",
      "name": "Orphan",
      "owner": null,
      "url": "https://sheets.example/s2"
    }
  ]
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestListSpreadsheets_QueryFilter(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := ListSpreadsheets(context.Background(), g, mustArgs(t,
		`{"query":"budg'et","maxResults":5,"orderBy":"name"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	q, _ := url.ParseQuery(fg.lastRequest().Query)
	wantQ := "mimeType='application/vnd.google-apps.spreadsheet' and trashed=false and (name contains 'budg\\'et' or fullText contains 'budg\\'et')"
	if got := q.Get("q"); got != wantQ {
		t.Fatalf("q = %q, want %q", got, wantQ)
	}
	if got := q.Get("pageSize"); got != "5" {
		t.Fatalf("pageSize = %q", got)
	}
	if got := q.Get("orderBy"); got != "name" {
		t.Fatalf("orderBy = %q", got)
	}
	if out != "{\n  \"spreadsheets\": []\n}" { // files || []
		t.Fatalf("out = %q", out)
	}
}

func TestListSpreadsheets_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"files":[]}`)
	})
	withDriveBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"unknown key", `{"bogus":1}`, `Unrecognized key: "bogus"`},
		{"maxResults zero", `{"maxResults":0}`, `Number must be greater than or equal to 1: "maxResults"`},
		{"maxResults over max", `{"maxResults":101}`, `Number must be less than or equal to 100: "maxResults"`},
		{"fractional maxResults", `{"maxResults":2.5}`, `Expected integer for "maxResults"`},
		{"bad orderBy", `{"orderBy":"size"}`, `Invalid enum value for "orderBy": "size"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListSpreadsheets(ctx, g, mustArgs(t, tc.args))
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

func TestListSpreadsheets_HTTPBranches(t *testing.T) {
	t.Run("403", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"Insufficient Permission"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := ListSpreadsheets(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
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
		_, err := ListSpreadsheets(context.Background(), fg.googleFor(newFakeTokenSource()), nil)
		ue := err.(*UserError)
		if ue.Msg != "Failed to list spreadsheets: Backend exploded" {
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
		out, err := ListSpreadsheets(context.Background(), fg.googleFor(tok), nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"spreadsheets": []`) {
			t.Fatalf("out = %s", out)
		}
	})
}

// ---------- writeSpreadsheet ----------

func TestWriteSpreadsheet_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"spreadsheetId":"sid","updatedRange":"Sheet1!A1:B2","updatedRows":2,"updatedColumns":2,"updatedCells":4}`)
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := WriteSpreadsheet(context.Background(), g, mustArgs(t,
		`{"spreadsheetId":"sid","range":"Sheet1!A1:B2","values":[["a",1],[true,null]]}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPut {
		t.Fatalf("method = %q", req.Method)
	}
	if req.Path != "/spreadsheets/sid/values/Sheet1!A1:B2" {
		t.Fatalf("path = %q", req.Path)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("valueInputOption"); got != "USER_ENTERED" { // zod default
		t.Fatalf("valueInputOption = %q", got)
	}
	// JSON cell types round-trip exactly as JS JSON.parse->stringify.
	if req.Body != `{"values":[["a",1],[true,null]]}` {
		t.Fatalf("body = %q", req.Body)
	}
	want := "Successfully wrote 4 cells (2 rows, 2 columns) to range Sheet1!A1:B2."
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestWriteSpreadsheet_ConfinedRAWAndMissingCounts(t *testing.T) {
	// Parity rule 8: the enforce pipeline's RAW forcing arrives as an arg and
	// wins over the USER_ENTERED default; the handler never forces RAW itself.
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"spreadsheetId":"sid"}`) // no updated* fields -> || 0
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := WriteSpreadsheet(context.Background(), g, mustArgs(t,
		`{"spreadsheetId":"sid","range":"A1","values":[["=1+1"]],"valueInputOption":"RAW"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	q, _ := url.ParseQuery(fg.lastRequest().Query)
	if got := q.Get("valueInputOption"); got != "RAW" {
		t.Fatalf("valueInputOption = %q", got)
	}
	want := "Successfully wrote 0 cells (0 rows, 0 columns) to range A1."
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestWriteSpreadsheet_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{}`)
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	// 501-row outer array and a 501-cell inner array cross the zod .max(500).
	overRows := `{"spreadsheetId":"s","range":"A1","values":[` + strings.Repeat(`[1],`, 500) + `[1]]}`
	overCols := `{"spreadsheetId":"s","range":"A1","values":[[` + strings.Repeat(`1,`, 500) + `1]]}`

	cases := []struct {
		name, args, wantMsg string
	}{
		{"unknown key", `{"spreadsheetId":"s","range":"A1","values":[],"bogus":1}`, `Unrecognized key: "bogus"`},
		{"missing values", `{"spreadsheetId":"s","range":"A1"}`, `Required: "values"`},
		{"null values", `{"spreadsheetId":"s","range":"A1","values":null}`, `Expected array for "values"`},
		{"row not array", `{"spreadsheetId":"s","range":"A1","values":[1]}`, `Expected array for "values"`},
		{"cell object", `{"spreadsheetId":"s","range":"A1","values":[[{"x":1}]]}`, `Invalid cell value in "values": expected string, number, boolean or null`},
		{"outer max 500", overRows, `Array must contain at most 500 element(s): "values"`},
		{"inner max 500", overCols, `Array must contain at most 500 element(s): "values"`},
		{"bad valueInputOption", `{"spreadsheetId":"s","range":"A1","values":[],"valueInputOption":"raw"}`, `Invalid enum value for "valueInputOption": "raw"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := WriteSpreadsheet(ctx, g, mustArgs(t, tc.args))
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

func TestWriteSpreadsheet_HTTPBranches(t *testing.T) {
	args := mustArgs(t, `{"spreadsheetId":"sid","range":"A1","values":[["x"]]}`)
	t.Run("404", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"not found"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := WriteSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Spreadsheet not found (ID: sid). Check the ID." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"denied"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := WriteSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Permission denied for spreadsheet (ID: sid). Ensure you have write access." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := WriteSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Failed to write range: Backend exploded" {
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
			cannedJSON(w, 200, `{"updatedCells":1,"updatedRows":1,"updatedColumns":1}`)
		})
		withSheetsBase(t, fg.server.URL)
		out, err := WriteSpreadsheet(context.Background(), fg.googleFor(tok), args)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if out != "Successfully wrote 1 cells (1 rows, 1 columns) to range A1." {
			t.Fatalf("out = %q", out)
		}
	})
}

// ---------- appendRows ----------

func TestAppendRows_HappyPath(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"spreadsheetId":"sid","tableRange":"Sheet1!A1:B1","updates":{"spreadsheetId":"sid","updatedRange":"Sheet1!A2:B3","updatedRows":2,"updatedColumns":2,"updatedCells":4}}`)
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := AppendRows(context.Background(), g, mustArgs(t,
		`{"spreadsheetId":"sid","range":"Sheet1!A1","values":[["a","b"],["c","d"]]}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	req := fg.lastRequest()
	if req.Method != http.MethodPost {
		t.Fatalf("method = %q", req.Method)
	}
	if req.Path != "/spreadsheets/sid/values/Sheet1!A1:append" {
		t.Fatalf("path = %q", req.Path)
	}
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("valueInputOption"); got != "USER_ENTERED" {
		t.Fatalf("valueInputOption = %q", got)
	}
	if got := q.Get("insertDataOption"); got != "INSERT_ROWS" {
		t.Fatalf("insertDataOption = %q", got)
	}
	if req.Body != `{"values":[["a","b"],["c","d"]]}` {
		t.Fatalf("body = %q", req.Body)
	}
	want := "Successfully appended 2 row(s) (4 cells) to spreadsheet. Updated range: Sheet1!A2:B3"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestAppendRows_MissingUpdatesFallsBack(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"spreadsheetId":"sid"}`) // updates undefined
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := AppendRows(context.Background(), g, mustArgs(t,
		`{"spreadsheetId":"sid","range":"A1","values":[["x"]],"valueInputOption":"RAW"}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	// updates?.updatedRange || args.range -> the input range; counts || 0.
	want := "Successfully appended 0 row(s) (0 cells) to spreadsheet. Updated range: A1"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestAppendRows_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{}`)
	})
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"unknown key", `{"spreadsheetId":"s","range":"A1","values":[],"bogus":1}`, `Unrecognized key: "bogus"`},
		{"missing values", `{"spreadsheetId":"s","range":"A1"}`, `Required: "values"`},
		{"cell array nested", `{"spreadsheetId":"s","range":"A1","values":[[[1]]]}`, `Invalid cell value in "values": expected string, number, boolean or null`},
		{"bad valueInputOption", `{"spreadsheetId":"s","range":"A1","values":[],"valueInputOption":"INPUT"}`, `Invalid enum value for "valueInputOption": "INPUT"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AppendRows(ctx, g, mustArgs(t, tc.args))
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

func TestAppendRows_HTTPBranches(t *testing.T) {
	args := mustArgs(t, `{"spreadsheetId":"sid","range":"A1","values":[["x"]]}`)
	t.Run("404", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"not found"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := AppendRows(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Spreadsheet not found (ID: sid). Check the ID." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"denied"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := AppendRows(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Permission denied for spreadsheet (ID: sid). Ensure you have write access." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		_, err := AppendRows(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Failed to append values: Backend exploded" {
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
			cannedJSON(w, 200, `{"updates":{"updatedRows":1,"updatedCells":1,"updatedRange":"A2"}}`)
		})
		withSheetsBase(t, fg.server.URL)
		out, err := AppendRows(context.Background(), fg.googleFor(tok), args)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if out != "Successfully appended 1 row(s) (1 cells) to spreadsheet. Updated range: A2" {
			t.Fatalf("out = %q", out)
		}
	})
}

// ---------- createSpreadsheet ----------

func TestCreateSpreadsheet_HappyPathNoFolder(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"newid","name":"Budget","webViewLink":"https://sheets.example/newid"}`)
	})
	withDriveBase(t, fg.server.URL)
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := CreateSpreadsheet(context.Background(), g, mustArgs(t, `{"title":"Budget"}`))
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
	q, _ := url.ParseQuery(req.Query)
	if got := q.Get("fields"); got != "id,name,webViewLink" {
		t.Fatalf("fields = %q", got)
	}
	if got := q.Get("supportsAllDrives"); got != "true" {
		t.Fatalf("supportsAllDrives = %q", got)
	}
	// parents key absent without parentFolderId (JS: only set when provided).
	if req.Body != `{"name":"Budget","mimeType":"application/vnd.google-apps.spreadsheet"}` {
		t.Fatalf("body = %q", req.Body)
	}
	// No initialData arg -> no initialData key in output.
	want := `{
  "id": "newid",
  "name": "Budget",
  "url": "https://sheets.example/newid"
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestCreateSpreadsheet_FolderAndInitialData(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			cannedJSON(w, 200, `{"id":"newid","name":"T","webViewLink":"https://sheets.example/newid"}`)
			return
		}
		cannedJSON(w, 200, `{"updatedCells":2}`)
	})
	withDriveBase(t, fg.server.URL)
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := CreateSpreadsheet(context.Background(), g, mustArgs(t,
		`{"title":"T","parentFolderId":"folder123","initialData":[["h1","h2"]]}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	fg.mu.Lock()
	reqs := append([]recordedRequest(nil), fg.requests...)
	fg.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0].Body != `{"name":"T","mimeType":"application/vnd.google-apps.spreadsheet","parents":["folder123"]}` {
		t.Fatalf("create body = %q", reqs[0].Body)
	}
	// Initial data write: hard-coded USER_ENTERED at range A1 (upstream).
	if reqs[1].Method != http.MethodPut || reqs[1].Path != "/spreadsheets/newid/values/A1" {
		t.Fatalf("write req = %s %s", reqs[1].Method, reqs[1].Path)
	}
	q, _ := url.ParseQuery(reqs[1].Query)
	if got := q.Get("valueInputOption"); got != "USER_ENTERED" {
		t.Fatalf("valueInputOption = %q", got)
	}
	if reqs[1].Body != `{"values":[["h1","h2"]]}` {
		t.Fatalf("write body = %q", reqs[1].Body)
	}
	want := `{
  "id": "newid",
  "name": "T",
  "url": "https://sheets.example/newid",
  "initialData": "added"
}`
	if out != want {
		t.Fatalf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestCreateSpreadsheet_InitialDataWriteFails(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			cannedJSON(w, 200, `{"id":"newid","name":"T","webViewLink":"https://sheets.example/newid"}`)
			return
		}
		cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
	})
	withDriveBase(t, fg.server.URL)
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	// Upstream swallows the write failure and still returns success.
	out, err := CreateSpreadsheet(context.Background(), g, mustArgs(t,
		`{"title":"T","initialData":[["h1"]]}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !strings.Contains(out, `"initialData": "failed"`) {
		t.Fatalf("out = %s", out)
	}
}

func TestCreateSpreadsheet_EmptyInitialDataSkipped(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"newid","name":"T"}`)
	})
	withDriveBase(t, fg.server.URL)
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())

	out, err := CreateSpreadsheet(context.Background(), g, mustArgs(t, `{"title":"T","initialData":[]}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(fg.requests) != 1 { // no values.update call for an empty array
		t.Fatalf("requests = %d, want 1", len(fg.requests))
	}
	if strings.Contains(out, "initialData") {
		t.Fatalf("out = %s", out)
	}
}

func TestCreateSpreadsheet_NoIDReturned(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{}`)
	})
	withDriveBase(t, fg.server.URL)
	withSheetsBase(t, fg.server.URL)
	_, err := CreateSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()),
		mustArgs(t, `{"title":"T"}`))
	ue := err.(*UserError)
	// Upstream catch has no instanceof-UserError rethrow, so the inner
	// UserError is re-wrapped with the generic prefix. Ported verbatim.
	if ue.Msg != "Failed to create spreadsheet: Failed to create spreadsheet - no ID returned." {
		t.Fatalf("msg = %q", ue.Msg)
	}
}

func TestCreateSpreadsheet_ArgsParity(t *testing.T) {
	fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		cannedJSON(w, 200, `{"id":"x"}`)
	})
	withDriveBase(t, fg.server.URL)
	withSheetsBase(t, fg.server.URL)
	g := fg.googleFor(newFakeTokenSource())
	ctx := context.Background()

	cases := []struct {
		name, args, wantMsg string
	}{
		{"unknown key", `{"title":"T","bogus":1}`, `Unrecognized key: "bogus"`},
		{"missing title", `{}`, `Required: "title"`},
		{"empty title", `{"title":""}`, `String must contain at least 1 character(s): "title"`},
		{"bad initialData cell", `{"title":"T","initialData":[[{"x":1}]]}`, `Invalid cell value in "initialData": expected string, number, boolean or null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateSpreadsheet(ctx, g, mustArgs(t, tc.args))
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

func TestCreateSpreadsheet_HTTPBranches(t *testing.T) {
	args := mustArgs(t, `{"title":"T","parentFolderId":"fold"}`)
	t.Run("404 parent folder", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 404, `{"error":{"message":"File not found: fold."}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := CreateSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Parent folder not found. Check the folder ID." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("403 destination", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 403, `{"error":{"message":"denied"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := CreateSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Permission denied. Make sure you have write access to the destination folder." {
			t.Fatalf("msg = %q", ue.Msg)
		}
	})
	t.Run("500 passthrough", func(t *testing.T) {
		fg := newFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
			cannedJSON(w, 500, `{"error":{"message":"Backend exploded"}}`)
		})
		withDriveBase(t, fg.server.URL)
		_, err := CreateSpreadsheet(context.Background(), fg.googleFor(newFakeTokenSource()), args)
		ue := err.(*UserError)
		if ue.Msg != "Failed to create spreadsheet: Backend exploded" {
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
			cannedJSON(w, 200, `{"id":"newid","name":"T"}`)
		})
		withDriveBase(t, fg.server.URL)
		out, err := CreateSpreadsheet(context.Background(), fg.googleFor(tok), args)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if calls != 2 || tok.invalidated != 1 {
			t.Fatalf("calls=%d invalidated=%d", calls, tok.invalidated)
		}
		if !strings.Contains(out, `"id": "newid"`) {
			t.Fatalf("out = %s", out)
		}
	})
}

// ---------- registry ----------

func TestSheetsHandlers_Registry(t *testing.T) {
	reg := SheetsHandlers()
	want := []string{"readSpreadsheet", "listSpreadsheets", "writeSpreadsheet", "appendRows", "createSpreadsheet"}
	if len(reg) != len(want) {
		t.Fatalf("registry size = %d", len(reg))
	}
	for _, name := range want {
		if reg[name] == nil {
			t.Fatalf("missing handler %q", name)
		}
	}
}
