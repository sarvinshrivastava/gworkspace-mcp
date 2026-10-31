package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// createDocument — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/drive/createDocument.js
func CreateDocument(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "title", "parentFolderId", "initialContent", "contentFormat")
	if err != nil {
		return "", err
	}
	title, err := a.ReqString("title", true)
	if err != nil {
		return "", err
	}
	parentFolderID, err := a.OptString("parentFolderId")
	if err != nil {
		return "", err
	}
	initialContent, err := a.OptString("initialContent")
	if err != nil {
		return "", err
	}
	contentFormat, err := a.OptEnum("contentFormat", "markdown", "markdown", "raw")
	if err != nil {
		return "", err
	}

	// drive.files.create — key order: name, mimeType, parents
	createBody := struct {
		Name     string   `json:"name"`
		MimeType string   `json:"mimeType"`
		Parents  []string `json:"parents,omitempty"`
	}{Name: title, MimeType: "application/vnd.google-apps.document"}
	if parentFolderID != "" {
		createBody.Parents = []string{parentFolderID}
	}
	u := DriveBase + "/files?" + Query("fields", "id,name,webViewLink", "supportsAllDrives", "true")
	body, status, err := g.Do(ctx, http.MethodPost, u, createBody)
	if err != nil {
		return "", &UserError{Msg: "Failed to create document: " + err.Error()}
	}
	if status == 404 {
		return "", &UserError{Msg: "Parent folder not found. Check the folder ID."}
	}
	if status == 403 {
		return "", &UserError{Msg: "Permission denied. Make sure you have write access to the destination folder."}
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to create document: " + ErrMessage(body, status)}
	}
	var document struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		WebViewLink string `json:"webViewLink"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return "", &UserError{Msg: "Failed to create document: " + err.Error()}
	}

	contentInserted := false
	if initialContent != "" {
		contentErr := func() error {
			if contentFormat == "raw" {
				reqBody := `{"requests":[{"insertText":{"location":{"index":1},"text":` + jsQuote(initialContent) + `}}]}`
				rbody, rstatus, rerr := g.Do(ctx, http.MethodPost,
					JoinURL(DocsBase+"/documents", document.ID)+":batchUpdate", json.RawMessage(reqBody))
				if rerr != nil {
					return rerr
				}
				if rstatus < 200 || rstatus >= 300 {
					return errors.New(ErrMessage(rbody, rstatus))
				}
				return nil
			}
			_, merr := insertMarkdown(ctx, g, document.ID, initialContent, 1, "", true)
			return merr
		}()
		if contentErr != nil {
			detail := contentErr.Error()
			url := document.WebViewLink
			if url == "" {
				url = "undefined" // JS template interpolation of a missing field
			}
			return "", &UserError{Msg: `Document "` + document.Name + `" was created (id: ` + document.ID +
				`, url: ` + url + `) but inserting the initial content FAILED, so it is currently EMPTY. ` +
				`Append the content to this existing document (do not create a new one). ` +
				`Underlying error: ` + detail}
		}
		contentInserted = true
	}
	return Stringify(struct {
		ID              string `json:"id,omitempty"`
		Name            string `json:"name,omitempty"`
		URL             string `json:"url,omitempty"`
		ContentInserted bool   `json:"contentInserted"`
	}{ID: document.ID, Name: document.Name, URL: document.WebViewLink, ContentInserted: contentInserted}), nil
}
