package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
)

// Sheets tools — upstream references:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/sheets/{readSpreadsheet,
// listGoogleSheets,writeSpreadsheet,appendSpreadsheetRows,createSpreadsheet}.js
// plus dist/googleSheetsApiHelpers.js (readRange/writeRange/appendValues).
//
// Helper-layer catch blocks throw UserError with fixed 404/403 messages; the
// tool-level catch rethrows UserError unchanged (`error instanceof UserError`),
// so the helper messages are what the caller sees. createSpreadsheet and
// listSpreadsheets have NO instanceof rethrow — port that asymmetry exactly.

// encodeURIComponent matches JS encodeURIComponent (what googleapis applies to
// path params like the A1 range): unreserved set A-Z a-z 0-9 - _ . ! ~ * ' ( ),
// everything else percent-encoded as uppercase-hex UTF-8 bytes.
func encodeURIComponent(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// jsNumber formats a JSON number the way JS string interpolation does for the
// values Google returns here (integers print without a decimal point).
func jsNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// cellValues decodes and validates a zod
// z.array(z.array(SpreadsheetCellValueSchema)) argument: each cell must be
// string, number, boolean or null. required mirrors a non-optional schema key;
// maxRows/maxCols mirror zod .max(n) (<=0 = no bound).
func cellValues(a *Args, key string, required bool, maxRows, maxCols int) ([][]any, error) {
	r, ok := a.raw[key]
	if !ok {
		if required {
			return nil, &UserError{Msg: fmt.Sprintf("Required: %q", key)}
		}
		return nil, nil
	}
	if len(bytes.TrimSpace(r)) == 0 || string(bytes.TrimSpace(r)) == "null" {
		return nil, &UserError{Msg: fmt.Sprintf("Expected array for %q", key)}
	}
	// Decode rows as raw messages first: a null row must reject (zod's
	// array-of-arrays), which a [][]any decode would silently accept as nil.
	var rawRows []json.RawMessage
	if err := json.Unmarshal(r, &rawRows); err != nil {
		return nil, &UserError{Msg: fmt.Sprintf("Expected array for %q", key)}
	}
	if maxRows > 0 && len(rawRows) > maxRows {
		return nil, &UserError{Msg: fmt.Sprintf("Array must contain at most %d element(s): %q", maxRows, key)}
	}
	rows := make([][]any, 0, len(rawRows))
	for _, rr := range rawRows {
		trimmed := bytes.TrimSpace(rr)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return nil, &UserError{Msg: fmt.Sprintf("Expected array for %q", key)}
		}
		var row []any
		if err := json.Unmarshal(rr, &row); err != nil {
			return nil, &UserError{Msg: fmt.Sprintf("Expected array for %q", key)}
		}
		rows = append(rows, row)
	}
	for _, row := range rows {
		if maxCols > 0 && len(row) > maxCols {
			return nil, &UserError{Msg: fmt.Sprintf("Array must contain at most %d element(s): %q", maxCols, key)}
		}
		for _, cell := range row {
			switch cell.(type) {
			case string, float64, bool, nil:
			default:
				return nil, &UserError{Msg: fmt.Sprintf("Invalid cell value in %q: expected string, number, boolean or null", key)}
			}
		}
	}
	return rows, nil
}

// valuesBody is the requestBody {values} for values.update/append.
type valuesBody struct {
	Values [][]any `json:"values"`
}

// sheetsNotFound / sheetsDenied are the helper-layer fixed messages.
func sheetsNotFound(id string) *UserError {
	return &UserError{Msg: fmt.Sprintf("Spreadsheet not found (ID: %s). Check the ID.", id)}
}

// accessWord is "read" for readRange and "write" for writeRange/appendValues.
func sheetsDenied(id, accessWord string) *UserError {
	return &UserError{Msg: fmt.Sprintf("Permission denied for spreadsheet (ID: %s). Ensure you have %s access.", id, accessWord)}
}

// --- readSpreadsheet (dist/tools/sheets/readSpreadsheet.js) ---

func ReadSpreadsheet(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "spreadsheetId", "range", "valueRenderOption")
	if err != nil {
		return "", err
	}
	spreadsheetID, err := a.ReqString("spreadsheetId", false)
	if err != nil {
		return "", err
	}
	rng, err := a.ReqString("range", false)
	if err != nil {
		return "", err
	}
	vro, err := a.OptEnum("valueRenderOption", "FORMATTED_VALUE", "FORMATTED_VALUE", "UNFORMATTED_VALUE", "FORMULA")
	if err != nil {
		return "", err
	}

	u := JoinURL(SheetsBase+"/spreadsheets", spreadsheetID) +
		"/values/" + encodeURIComponent(rng) +
		"?" + Query("valueRenderOption", vro)
	body, status, err := g.Do(ctx, http.MethodGet, u, nil)
	if err != nil {
		// JS: a network failure is caught inside readRange.
		return "", &UserError{Msg: "Failed to read range: " + err.Error()}
	}
	if status == 404 {
		return "", sheetsNotFound(spreadsheetID)
	}
	if status == 403 {
		return "", sheetsDenied(spreadsheetID, "read")
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to read range: " + ErrMessage(body, status)}
	}

	var resp struct {
		Values [][]any `json:"values"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", &UserError{Msg: "Failed to read range: " + err.Error()}
	}
	values := resp.Values // upstream: response.values || []
	if values == nil {
		values = [][]any{}
	}
	return Stringify(struct {
		Range  string  `json:"range"`
		Values [][]any `json:"values"`
	}{rng, values}), nil
}

// --- listSpreadsheets (dist/tools/sheets/listGoogleSheets.js) ---

func ListSpreadsheets(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "maxResults", "query", "orderBy")
	if err != nil {
		return "", err
	}
	maxResults, err := a.OptInt("maxResults", 20, 1, 100)
	if err != nil {
		return "", err
	}
	query, err := a.OptString("query")
	if err != nil {
		return "", err
	}
	orderBy, err := a.OptEnum("orderBy", "modifiedTime", "name", "modifiedTime", "createdTime")
	if err != nil {
		return "", err
	}

	queryString := "mimeType='application/vnd.google-apps.spreadsheet' and trashed=false"
	if query != "" {
		escaped := enforce.EscapeQ(query)
		queryString += " and (name contains '" + escaped + "' or fullText contains '" + escaped + "')"
	}
	// upstream: orderBy: args.orderBy === 'name' ? 'name' : args.orderBy (no-op),
	// and no direction suffix is ever added here.
	u := DriveBase + "/files?" + Query(
		"q", queryString,
		"pageSize", strconv.Itoa(maxResults),
		"orderBy", orderBy,
		"fields", "files(id,name,modifiedTime,createdTime,size,webViewLink,owners(displayName,emailAddress))",
		"supportsAllDrives", "true",
		"includeItemsFromAllDrives", "true",
	)
	body, status, err := g.Do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", &UserError{Msg: "Failed to list spreadsheets: " + err.Error()}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have granted Google Drive access to the application."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to list spreadsheets: " + ErrMessage(body, status)}
	}

	var page struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ModifiedTime string `json:"modifiedTime"`
			WebViewLink  string `json:"webViewLink"`
			Owners       []struct {
				DisplayName string `json:"displayName"`
			} `json:"owners"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", &UserError{Msg: "Failed to list spreadsheets: " + err.Error()}
	}
	// owner is coerced to null by the upstream (always emitted); id/name/
	// modifiedTime/url drop out when undefined → string + omitempty.
	type outSheet struct {
		ID           string `json:"id,omitempty"`
		Name         string `json:"name,omitempty"`
		ModifiedTime string `json:"modifiedTime,omitempty"`
		Owner        any    `json:"owner"`
		URL          string `json:"url,omitempty"`
	}
	sheets := make([]outSheet, 0, len(page.Files))
	for _, f := range page.Files {
		out := outSheet{ID: f.ID, Name: f.Name, ModifiedTime: f.ModifiedTime, URL: f.WebViewLink}
		if len(f.Owners) > 0 && f.Owners[0].DisplayName != "" {
			out.Owner = f.Owners[0].DisplayName
		} else {
			out.Owner = nil
		}
		sheets = append(sheets, out)
	}
	return Stringify(struct {
		Spreadsheets []outSheet `json:"spreadsheets"`
	}{sheets}), nil
}

// --- writeSpreadsheet (dist/tools/sheets/writeSpreadsheet.js) ---

func WriteSpreadsheet(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "spreadsheetId", "range", "values", "valueInputOption")
	if err != nil {
		return "", err
	}
	spreadsheetID, err := a.ReqString("spreadsheetId", false)
	if err != nil {
		return "", err
	}
	rng, err := a.ReqString("range", false)
	if err != nil {
		return "", err
	}
	// zod: z.array(z.array(cell).max(500)).max(500), required.
	values, err := cellValues(a, "values", true, 500, 500)
	if err != nil {
		return "", err
	}
	vio, err := a.OptEnum("valueInputOption", "USER_ENTERED", "RAW", "USER_ENTERED")
	if err != nil {
		return "", err
	}
	// Parity rule 8: a confined agent's pipeline-injected valueInputOption:"RAW"
	// arrives as an ordinary arg and wins over the zod default — the handler
	// itself never forces RAW.

	u := JoinURL(SheetsBase+"/spreadsheets", spreadsheetID) +
		"/values/" + encodeURIComponent(rng) +
		"?" + Query("valueInputOption", vio)
	body, status, err := g.Do(ctx, http.MethodPut, u, valuesBody{Values: values})
	if err != nil {
		return "", &UserError{Msg: "Failed to write range: " + err.Error()}
	}
	if status == 404 {
		return "", sheetsNotFound(spreadsheetID)
	}
	if status == 403 {
		return "", sheetsDenied(spreadsheetID, "write")
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to write range: " + ErrMessage(body, status)}
	}

	var resp struct {
		UpdatedCells   *float64 `json:"updatedCells"`
		UpdatedRows    *float64 `json:"updatedRows"`
		UpdatedColumns *float64 `json:"updatedColumns"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", &UserError{Msg: "Failed to write range: " + err.Error()}
	}
	deref := func(p *float64) float64 {
		if p == nil {
			return 0 // upstream: response.updatedCells || 0
		}
		return *p
	}
	return fmt.Sprintf("Successfully wrote %s cells (%s rows, %s columns) to range %s.",
		jsNumber(deref(resp.UpdatedCells)), jsNumber(deref(resp.UpdatedRows)),
		jsNumber(deref(resp.UpdatedColumns)), rng), nil
}

// --- appendRows (dist/tools/sheets/appendSpreadsheetRows.js) ---

func AppendRows(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "spreadsheetId", "range", "values", "valueInputOption")
	if err != nil {
		return "", err
	}
	spreadsheetID, err := a.ReqString("spreadsheetId", false)
	if err != nil {
		return "", err
	}
	rng, err := a.ReqString("range", false)
	if err != nil {
		return "", err
	}
	// zod: z.array(z.array(cell)) — required, NO .max() here.
	values, err := cellValues(a, "values", true, 0, 0)
	if err != nil {
		return "", err
	}
	vio, err := a.OptEnum("valueInputOption", "USER_ENTERED", "RAW", "USER_ENTERED")
	if err != nil {
		return "", err
	}

	u := JoinURL(SheetsBase+"/spreadsheets", spreadsheetID) +
		"/values/" + encodeURIComponent(rng) + ":append" +
		"?" + Query("valueInputOption", vio, "insertDataOption", "INSERT_ROWS")
	body, status, err := g.Do(ctx, http.MethodPost, u, valuesBody{Values: values})
	if err != nil {
		return "", &UserError{Msg: "Failed to append values: " + err.Error()}
	}
	if status == 404 {
		return "", sheetsNotFound(spreadsheetID)
	}
	if status == 403 {
		return "", sheetsDenied(spreadsheetID, "write")
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to append values: " + ErrMessage(body, status)}
	}

	var resp struct {
		Updates *struct {
			UpdatedCells *float64 `json:"updatedCells"`
			UpdatedRows  *float64 `json:"updatedRows"`
			UpdatedRange *string  `json:"updatedRange"`
		} `json:"updates"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", &UserError{Msg: "Failed to append values: " + err.Error()}
	}
	var cells, rows float64
	updatedRange := rng // upstream: response.updates?.updatedRange || args.range
	if resp.Updates != nil {
		if resp.Updates.UpdatedCells != nil {
			cells = *resp.Updates.UpdatedCells
		}
		if resp.Updates.UpdatedRows != nil {
			rows = *resp.Updates.UpdatedRows
		}
		if resp.Updates.UpdatedRange != nil && *resp.Updates.UpdatedRange != "" {
			updatedRange = *resp.Updates.UpdatedRange
		}
	}
	return fmt.Sprintf("Successfully appended %s row(s) (%s cells) to spreadsheet. Updated range: %s",
		jsNumber(rows), jsNumber(cells), updatedRange), nil
}

// --- createSpreadsheet (dist/tools/sheets/createSpreadsheet.js) ---

func CreateSpreadsheet(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "title", "parentFolderId", "initialData")
	if err != nil {
		return "", err
	}
	title, err := a.ReqString("title", true) // z.string().min(1)
	if err != nil {
		return "", err
	}
	parentFolderID, err := a.OptString("parentFolderId")
	if err != nil {
		return "", err
	}
	// zod: optional, no .max(). The enforce pipeline denies initialData for
	// confined agents BEFORE the handler runs; the handler implements the
	// upstream behavior for whatever arrives.
	initialData, err := cellValues(a, "initialData", false, 0, 0)
	if err != nil {
		return "", err
	}

	// Step 1: drive.files.create. Key order in the JS requestBody literal is
	// name, mimeType, parents.
	meta := struct {
		Name     string   `json:"name"`
		MimeType string   `json:"mimeType"`
		Parents  []string `json:"parents,omitempty"`
	}{Name: title, MimeType: "application/vnd.google-apps.spreadsheet"}
	if parentFolderID != "" {
		meta.Parents = []string{parentFolderID}
	}
	u := DriveBase + "/files?" + Query("fields", "id,name,webViewLink", "supportsAllDrives", "true")
	body, status, err := g.Do(ctx, http.MethodPost, u, meta)
	if err != nil {
		return "", &UserError{Msg: "Failed to create spreadsheet: " + err.Error()}
	}
	// Upstream tool catch: 404 then 403 then generic — and NO
	// `error instanceof UserError` rethrow, so the no-ID UserError below is
	// re-wrapped with the "Failed to create spreadsheet:" prefix.
	if status == 404 {
		return "", &UserError{Msg: "Parent folder not found. Check the folder ID."}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have write access to the destination folder."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to create spreadsheet: " + ErrMessage(body, status)}
	}

	var created struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		WebViewLink string `json:"webViewLink"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return "", &UserError{Msg: "Failed to create spreadsheet: " + err.Error()}
	}
	if created.ID == "" {
		return "", &UserError{Msg: "Failed to create spreadsheet: Failed to create spreadsheet - no ID returned."}
	}

	// Step 2: initial data. Upstream hard-codes 'USER_ENTERED' and swallows
	// ALL write failures (initialDataStatus = 'failed'); only a non-empty
	// initialData produces the key at all.
	var initialDataStatus string
	if len(initialData) > 0 {
		wu := JoinURL(SheetsBase+"/spreadsheets", created.ID) +
			"/values/A1?" + Query("valueInputOption", "USER_ENTERED")
		wbody, wstatus, werr := g.Do(ctx, http.MethodPut, wu, valuesBody{Values: initialData})
		if werr != nil || wstatus < 200 || wstatus >= 300 {
			_ = wbody // upstream logs and continues
			initialDataStatus = "failed"
		} else {
			initialDataStatus = "added"
		}
	}

	out := struct {
		ID          string  `json:"id"`
		Name        string  `json:"name,omitempty"`
		URL         string  `json:"url,omitempty"`
		InitialData *string `json:"initialData,omitempty"`
	}{ID: created.ID, Name: created.Name, URL: created.WebViewLink}
	if initialDataStatus != "" {
		out.InitialData = &initialDataStatus
	}
	return Stringify(out), nil
}
