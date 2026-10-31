package tools

import (
	"context"
	"encoding/json"
)

// insertText — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/docs/insertText.js
func InsertText(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "documentId", "text", "index", "tabId")
	if err != nil {
		return "", err
	}
	documentID, err := a.ReqString("documentId", false)
	if err != nil {
		return "", err
	}
	text, err := a.ReqString("text", true)
	if err != nil {
		return "", err
	}
	index, err := a.argReqIntMin("index", 1)
	if err != nil {
		return "", err
	}
	tabID, err := a.OptString("tabId")
	if err != nil {
		return "", err
	}

	if tabID != "" {
		// upstream: GDocsHelpers.getDocumentTab validates the tab first
		body, status, gerr := docsGet(ctx, g, documentID, tabBodyEndIndexFields, true)
		if gerr != nil {
			return "", wrapFailed("insert text", gerr)
		}
		if status < 200 || status >= 300 {
			return "", &UserError{Msg: "Failed to insert text: " + ErrMessage(body, status)}
		}
		var doc documentGetResult
		if err := json.Unmarshal(body, &doc); err != nil {
			return "", wrapFailed("insert text", err)
		}
		targetTab := findTabById(doc.Tabs, tabID)
		if targetTab == nil {
			return "", &UserError{Msg: `Tab with ID "` + tabID + `" not found in document.`}
		}
		if targetTab.DocumentTab == nil {
			return "", &UserError{Msg: `Tab "` + tabID + `" does not have content (may not be a document tab).`}
		}
		req := mustMarshal(struct {
			InsertText struct {
				Location docLocation `json:"location"`
				Text     string      `json:"text"`
			} `json:"insertText"`
		}{InsertText: struct {
			Location docLocation `json:"location"`
			Text     string      `json:"text"`
		}{Location: docLocation{Index: index, TabID: tabID}, Text: text}})
		if _, err := executeBatchUpdate(ctx, g, documentID, []json.RawMessage{req}); err != nil {
			return "", wrapFailed("insert text", err)
		}
	} else {
		// upstream: GDocsHelpers.insertText (no tab validation call)
		if text == "" {
			return "Successfully inserted text at index " + itoa(index) + ".", nil
		}
		req := mustMarshal(struct {
			InsertText struct {
				Location docLocation `json:"location"`
				Text     string      `json:"text"`
			} `json:"insertText"`
		}{InsertText: struct {
			Location docLocation `json:"location"`
			Text     string      `json:"text"`
		}{Location: docLocation{Index: index}, Text: text}})
		if _, err := executeBatchUpdate(ctx, g, documentID, []json.RawMessage{req}); err != nil {
			return "", wrapFailed("insert text", err)
		}
	}
	msg := "Successfully inserted text at index " + itoa(index)
	if tabID != "" {
		msg += " in tab " + tabID
	}
	return msg + ".", nil
}
