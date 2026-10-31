package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/enforce"
)

// searchDriveFiles (legacy relay path) — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/drive/searchDriveFiles.js
// Confined agents never reach this handler: the pipeline serves their search
// with folder scoping and a pageToken denial (internal/enforce/search.go).
// The TS gateway relayed the legacy call to the child; the port runs the same
// shortcut natively.
func SearchDriveFiles(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "query", "searchIn", "mimeType", "folderId",
		"orderBy", "sortDirection", "maxResults", "modifiedAfter", "pageToken")
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
	mimeType, err := a.OptString("mimeType")
	if err != nil {
		return "", err
	}
	folderID, err := a.OptString("folderId")
	if err != nil {
		return "", err
	}
	orderBy, err := a.OptEnum("orderBy", "modifiedTime", "name", "modifiedTime", "createdTime")
	if err != nil {
		return "", err
	}
	sortDirection, err := a.OptEnum("sortDirection", "desc", "asc", "desc")
	if err != nil {
		return "", err
	}
	maxResults, err := a.OptInt("maxResults", 10, 1, 100)
	if err != nil {
		return "", err
	}
	modifiedAfter, err := a.OptString("modifiedAfter")
	if err != nil {
		return "", err
	}
	pageToken, err := a.OptString("pageToken")
	if err != nil {
		return "", err
	}

	conditions := []string{"trashed=false"}
	escaped := enforce.EscapeQ(query)
	switch searchIn {
	case "name":
		conditions = append(conditions, "name contains '"+escaped+"'")
	case "content":
		conditions = append(conditions, "fullText contains '"+escaped+"'")
	default:
		conditions = append(conditions, "(name contains '"+escaped+"' or fullText contains '"+escaped+"')")
	}
	if mimeType != "" {
		resolved := mimeType
		if short, ok := mimeShortcuts[mimeType]; ok {
			resolved = short
		}
		conditions = append(conditions, "mimeType='"+enforce.EscapeQ(resolved)+"'")
	}
	if folderID != "" {
		conditions = append(conditions, "'"+enforce.EscapeQ(folderID)+"' in ancestors")
	}
	if modifiedAfter != "" {
		cutoff, perr := parseJSDateToRFC3339(modifiedAfter)
		if perr != nil {
			return "", &UserError{Msg: "Failed to search files: " + perr.Error()}
		}
		conditions = append(conditions, "modifiedTime > '"+enforce.EscapeQ(cutoff)+"'")
	}
	orderByParam := orderBy
	if sortDirection == "desc" {
		orderByParam += " desc"
	}

	pairs := []string{
		"q", strings.Join(conditions, " and "),
		"pageSize", fmt.Sprintf("%d", maxResults),
		"orderBy", orderByParam,
		"fields", "nextPageToken,files(id,name,mimeType,size,modifiedTime,createdTime,webViewLink,owners(displayName,emailAddress),parents)",
		"supportsAllDrives", "true",
		"includeItemsFromAllDrives", "true",
	}
	if pageToken != "" {
		pairs = append(pairs, "pageToken", pageToken)
	}
	body, status, err := g.Do(ctx, http.MethodGet, DriveBase+"/files?"+Query(pairs...), nil)
	if err != nil {
		return "", &UserError{Msg: "Failed to search files: " + err.Error()}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have granted Google Drive access to the application."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to search files: " + ErrMessage(body, status)}
	}

	var page struct {
		NextPageToken string `json:"nextPageToken"`
		Files         []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			MimeType     string `json:"mimeType"`
			Size         any    `json:"size"`
			ModifiedTime string `json:"modifiedTime"`
			CreatedTime  string `json:"createdTime"`
			WebViewLink  string `json:"webViewLink"`
			Owners       []struct {
				DisplayName string `json:"displayName"`
			} `json:"owners"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", &UserError{Msg: "Failed to search files: " + err.Error()}
	}
	sizeOf := func(v any) *int64 {
		// upstream: file.size != null ? Number(file.size) : null
		switch sv := v.(type) {
		case string:
			n, cerr := strconv.ParseInt(sv, 10, 64)
			if cerr != nil {
				return nil
			}
			return &n
		case float64:
			n := int64(sv)
			return &n
		}
		return nil
	}
	type outFile struct {
		ID           string `json:"id,omitempty"`
		Name         string `json:"name,omitempty"`
		MimeType     string `json:"mimeType,omitempty"`
		Size         *int64 `json:"size"`
		ModifiedTime string `json:"modifiedTime,omitempty"`
		CreatedTime  string `json:"createdTime,omitempty"`
		Owner        any    `json:"owner"`
		URL          string `json:"url,omitempty"`
	}
	files := make([]outFile, 0, len(page.Files))
	for _, f := range page.Files {
		out := outFile{
			ID: f.ID, Name: f.Name, MimeType: f.MimeType, Size: sizeOf(f.Size),
			ModifiedTime: f.ModifiedTime, CreatedTime: f.CreatedTime, URL: f.WebViewLink,
		}
		if len(f.Owners) > 0 && f.Owners[0].DisplayName != "" {
			out.Owner = f.Owners[0].DisplayName
		}
		files = append(files, out)
	}
	// Key order matches JS insertion: files, total, then nextPageToken and
	// hasMore assigned after the literal.
	return Stringify(struct {
		Files         []outFile `json:"files"`
		Total         int       `json:"total"`
		NextPageToken string    `json:"nextPageToken,omitempty"`
		HasMore       bool      `json:"hasMore"`
	}{files, len(files), page.NextPageToken, page.NextPageToken != ""}), nil
}
