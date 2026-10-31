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

// listDriveFiles — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/drive/listDriveFiles.js
var mimeShortcuts = map[string]string{
	"document":     "application/vnd.google-apps.document",
	"spreadsheet":  "application/vnd.google-apps.spreadsheet",
	"presentation": "application/vnd.google-apps.presentation",
	"folder":       "application/vnd.google-apps.folder",
	"form":         "application/vnd.google-apps.form",
	"pdf":          "application/pdf",
	"zip":          "application/zip",
}

func ListDriveFiles(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "maxResults", "mimeType", "folderId", "orderBy",
		"sortDirection", "ownedByMe", "sharedWithMe", "modifiedAfter")
	if err != nil {
		return "", err
	}
	maxResults, err := a.OptInt("maxResults", 20, 1, 100)
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
	orderBy, err := a.OptEnum("orderBy", "modifiedTime", "name", "modifiedTime", "createdTime", "quotaBytesUsed")
	if err != nil {
		return "", err
	}
	sortDirection, err := a.OptEnum("sortDirection", "desc", "asc", "desc")
	if err != nil {
		return "", err
	}
	ownedByMe, err := a.OptBool("ownedByMe")
	if err != nil {
		return "", err
	}
	sharedWithMe, err := a.OptBool("sharedWithMe")
	if err != nil {
		return "", err
	}
	modifiedAfter, err := a.OptString("modifiedAfter")
	if err != nil {
		return "", err
	}
	if ownedByMe && sharedWithMe {
		return "", &UserError{Msg: "ownedByMe and sharedWithMe cannot both be true."}
	}

	conditions := []string{"trashed=false"}
	if mimeType != "" {
		resolved := mimeType
		if short, ok := mimeShortcuts[mimeType]; ok {
			resolved = short
		}
		conditions = append(conditions, "mimeType='"+enforce.EscapeQ(resolved)+"'")
	}
	if folderID != "" {
		conditions = append(conditions, "'"+enforce.EscapeQ(folderID)+"' in parents")
	}
	if ownedByMe {
		conditions = append(conditions, "'me' in owners")
	} else if sharedWithMe {
		conditions = append(conditions, "sharedWithMe=true")
	}
	if modifiedAfter != "" {
		cutoff, perr := parseJSDateToRFC3339(modifiedAfter)
		if perr != nil {
			return "", &UserError{Msg: "Failed to list files: " + perr.Error()}
		}
		conditions = append(conditions, "modifiedTime > '"+enforce.EscapeQ(cutoff)+"'")
	}
	orderByParam := orderBy
	if sortDirection == "desc" {
		orderByParam += " desc"
	}

	u := DriveBase + "/files?" + Query(
		"q", strings.Join(conditions, " and "),
		"pageSize", fmt.Sprintf("%d", maxResults),
		"orderBy", orderByParam,
		"fields", "files(id,name,mimeType,size,modifiedTime,createdTime,webViewLink,owners(displayName,emailAddress))",
		"supportsAllDrives", "true",
		"includeItemsFromAllDrives", "true",
	)
	body, status, err := g.Do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", &UserError{Msg: "Failed to list files: " + err.Error()}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have granted Google Drive access to the application."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to list files: " + ErrMessage(body, status)}
	}

	var page struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			MimeType     string `json:"mimeType"`
			Size         any    `json:"size"` // Drive returns a quoted string or omits it
			ModifiedTime string `json:"modifiedTime"`
			CreatedTime  string `json:"createdTime"`
			WebViewLink  string `json:"webViewLink"`
			Owners       []struct {
				DisplayName string `json:"displayName"`
			} `json:"owners"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", &UserError{Msg: "Failed to list files: " + err.Error()}
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
	// Pointer + omitempty reproduces JSON.stringify dropping keys whose value
	// is undefined; owner and size are always emitted because the upstream
	// coerces them to null explicitly.
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
		} else {
			out.Owner = nil
		}
		files = append(files, out)
	}
	return Stringify(struct {
		Files []outFile `json:"files"`
		Total int       `json:"total"`
	}{files, len(files)}), nil
}
