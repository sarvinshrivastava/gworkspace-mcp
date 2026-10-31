package tools

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
)

// outDoc is the shared listDocuments/searchDocuments output element. Key
// order matches the upstream object literal; owner is always emitted (null
// when absent) because the upstream coerces with `|| null`; the rest drop
// when Google omits them.
type outDoc struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name,omitempty"`
	ModifiedTime string `json:"modifiedTime,omitempty"`
	Owner        any    `json:"owner"`
	URL          string `json:"url,omitempty"`
}

type docListFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ModifiedTime string `json:"modifiedTime"`
	WebViewLink  string `json:"webViewLink"`
	Owners       []struct {
		DisplayName string `json:"displayName"`
	} `json:"owners"`
}

func mapDocFiles(files []docListFile) []outDoc {
	documents := make([]outDoc, 0, len(files))
	for _, f := range files {
		var owner any
		if len(f.Owners) > 0 && f.Owners[0].DisplayName != "" {
			owner = f.Owners[0].DisplayName
		}
		documents = append(documents, outDoc{
			ID: f.ID, Name: f.Name, ModifiedTime: f.ModifiedTime,
			Owner: owner, URL: f.WebViewLink,
		})
	}
	return documents
}

// listDocuments — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/drive/listGoogleDocs.js
func ListDocuments(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "maxResults", "query", "orderBy", "modifiedAfter")
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
	modifiedAfter, err := a.OptString("modifiedAfter")
	if err != nil {
		return "", err
	}

	queryString := "mimeType='application/vnd.google-apps.document' and trashed=false"
	if query != "" {
		esc := enforce.EscapeQ(query)
		queryString += " and (name contains '" + esc + "' or fullText contains '" + esc + "')"
	}
	if modifiedAfter != "" {
		cutoff, perr := parseJSDateToRFC3339(modifiedAfter)
		if perr != nil {
			return "", &UserError{Msg: "Failed to list documents: " + perr.Error()}
		}
		queryString += " and modifiedTime > '" + enforce.EscapeQ(cutoff) + "'"
	}
	// upstream: orderBy: args.orderBy === 'name' ? 'name' : args.orderBy — identity
	u := DriveBase + "/files?" + Query(
		"q", queryString,
		"pageSize", itoa(maxResults),
		"orderBy", orderBy,
		"fields", "files(id,name,modifiedTime,createdTime,size,webViewLink,owners(displayName,emailAddress))",
		"supportsAllDrives", "true",
		"includeItemsFromAllDrives", "true",
	)
	body, status, err := g.Do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", &UserError{Msg: "Failed to list documents: " + err.Error()}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have granted Google Drive access to the application."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to list documents: " + ErrMessage(body, status)}
	}
	var page struct {
		Files []docListFile `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", &UserError{Msg: "Failed to list documents: " + err.Error()}
	}
	documents := mapDocFiles(page.Files)
	return Stringify(struct {
		Documents []outDoc `json:"documents"`
	}{Documents: documents}), nil
}
