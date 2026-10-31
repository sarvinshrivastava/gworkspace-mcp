package tools

import (
	"context"
	"encoding/json"
	"net/http"
)

// createFolder — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/drive/createFolder.js
func CreateFolder(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "name", "parentFolderId")
	if err != nil {
		return "", err
	}
	// zod parity: null is not a string, and not undefined — reject it.
	m := dcMap(raw)
	if err := dcRejectNull(m, "string", "name", "parentFolderId"); err != nil {
		return "", err
	}
	name, err := a.ReqString("name", true)
	if err != nil {
		return "", err
	}
	parentFolderID, err := a.OptString("parentFolderId")
	if err != nil {
		return "", err
	}

	// requestBody key order matches the upstream literal: name, mimeType,
	// parents. parents is present only when parentFolderId is truthy ("" is
	// Drive root, exactly like the upstream `if (args.parentFolderId)`).
	type folderMetadata struct {
		Name     string   `json:"name"`
		MimeType string   `json:"mimeType"`
		Parents  []string `json:"parents,omitempty"`
	}
	meta := folderMetadata{Name: name, MimeType: "application/vnd.google-apps.folder"}
	if parentFolderID != "" {
		meta.Parents = []string{parentFolderID}
	}

	u := DriveBase + "/files?" + Query(
		"fields", "id,name,parents,webViewLink",
		"supportsAllDrives", "true",
	)
	body, status, err := g.Do(ctx, http.MethodPost, u, meta)
	if err != nil {
		return "", &UserError{Msg: "Failed to create folder: " + err.Error()}
	}
	// Upstream catch order: 404, 403, generic.
	if status == 404 {
		return "", &UserError{Msg: "Parent folder not found. Check the parent folder ID."}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have write access to the parent folder."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to create folder: " + ErrMessage(body, status)}
	}

	// RawMessage fields: a key is dropped only when Google omits it
	// (JSON.stringify drops undefined); an explicit null survives verbatim.
	var folder struct {
		ID          json.RawMessage `json:"id"`
		Name        json.RawMessage `json:"name"`
		WebViewLink json.RawMessage `json:"webViewLink"`
	}
	if err := json.Unmarshal(body, &folder); err != nil {
		return "", &UserError{Msg: "Failed to create folder: " + err.Error()}
	}
	return Stringify(struct {
		ID   json.RawMessage `json:"id,omitempty"`
		Name json.RawMessage `json:"name,omitempty"`
		URL  json.RawMessage `json:"url,omitempty"`
	}{folder.ID, folder.Name, folder.WebViewLink}), nil
}

// DriveHandlers is the Drive tools' registry contribution. listDriveFiles
// is registered by the core registry;
// The Drive tools registry owns only createFolder (PARITY.md exact-20 map).
func DriveHandlers() map[string]Handler {
	return map[string]Handler{
		"createFolder": CreateFolder,
	}
}
