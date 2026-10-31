package tools

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
)

// searchDocuments — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/drive/searchGoogleDocs.js
func SearchDocuments(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "query", "searchIn", "maxResults", "modifiedAfter")
	if err != nil {
		return "", err
	}
	query, err := a.ReqString("query", true)
	if err != nil {
		return "", err
	}
	searchIn, err := a.OptEnum("searchIn", "both", "name", "content", "both")
	if err != nil {
		return "", err
	}
	maxResults, err := a.OptInt("maxResults", 10, 1, 50)
	if err != nil {
		return "", err
	}
	modifiedAfter, err := a.OptString("modifiedAfter")
	if err != nil {
		return "", err
	}

	esc := enforce.EscapeQ(query)
	queryString := "mimeType='application/vnd.google-apps.document' and trashed=false"
	switch searchIn {
	case "name":
		queryString += " and name contains '" + esc + "'"
	case "content":
		queryString += " and fullText contains '" + esc + "'"
	default:
		queryString += " and (name contains '" + esc + "' or fullText contains '" + esc + "')"
	}
	if modifiedAfter != "" {
		// upstream pins the raw string here — no new Date().toISOString()
		queryString += " and modifiedTime > '" + enforce.EscapeQ(modifiedAfter) + "'"
	}
	u := DriveBase + "/files?" + Query(
		"q", queryString,
		"pageSize", itoa(maxResults),
		"orderBy", "modifiedTime desc",
		"fields", "files(id,name,modifiedTime,createdTime,webViewLink,owners(displayName),parents)",
		"supportsAllDrives", "true",
		"includeItemsFromAllDrives", "true",
	)
	body, status, err := g.Do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", &UserError{Msg: "Failed to search documents: " + err.Error()}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have granted Google Drive access to the application."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to search documents: " + ErrMessage(body, status)}
	}
	var page struct {
		Files []docListFile `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", &UserError{Msg: "Failed to search documents: " + err.Error()}
	}
	documents := mapDocFiles(page.Files)
	return Stringify(struct {
		Documents []outDoc `json:"documents"`
	}{Documents: documents}), nil
}
