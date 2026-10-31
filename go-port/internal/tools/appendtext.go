package tools

import (
	"context"
	"encoding/json"
)

// appendText — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/docs/appendToGoogleDoc.js
func AppendText(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "documentId", "text", "addNewlineIfNeeded", "tabId")
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
	addNewlineIfNeeded, err := a.argOptBoolDefault("addNewlineIfNeeded", true)
	if err != nil {
		return "", err
	}
	tabID, err := a.OptString("tabId")
	if err != nil {
		return "", err
	}

	needsTabsContent := tabID != ""
	fields := "body(content(endIndex)),documentStyle(pageSize)"
	if needsTabsContent {
		fields = tabBodyEndIndexFields
	}
	body, status, err := docsGet(ctx, g, documentID, fields, needsTabsContent)
	if err != nil {
		return "", wrapFailed("append to doc", err)
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to append to doc: " + ErrMessage(body, status)}
	}
	var doc documentGetResult
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", wrapFailed("append to doc", err)
	}

	endIndex := 1
	var bodyContent []docElement
	if tabID != "" {
		targetTab := findTabById(doc.Tabs, tabID)
		if targetTab == nil {
			return "", &UserError{Msg: `Tab with ID "` + tabID + `" not found in document.`}
		}
		if targetTab.DocumentTab == nil {
			return "", &UserError{Msg: `Tab "` + tabID + `" does not have content (may not be a document tab).`}
		}
		if targetTab.DocumentTab.Body != nil {
			var b docBody
			if err := json.Unmarshal(targetTab.DocumentTab.Body, &b); err == nil {
				bodyContent = b.Content
			}
		}
	} else if doc.BodyRaw != nil {
		var b docBody
		if err := json.Unmarshal(doc.BodyRaw, &b); err == nil {
			bodyContent = b.Content
		}
	}
	if bodyContent != nil && len(bodyContent) > 0 {
		lastElement := bodyContent[len(bodyContent)-1]
		if lastElement.EndIndex != 0 {
			endIndex = lastElement.EndIndex - 1
		}
	}
	textToInsert := text
	if addNewlineIfNeeded && endIndex > 1 {
		textToInsert = "\n" + text
	}
	if textToInsert == "" {
		return "Nothing to append.", nil
	}
	location := docLocation{Index: endIndex, TabID: tabID}
	req := mustMarshal(struct {
		InsertText struct {
			Location docLocation `json:"location"`
			Text     string      `json:"text"`
		} `json:"insertText"`
	}{InsertText: struct {
		Location docLocation `json:"location"`
		Text     string      `json:"text"`
	}{Location: location, Text: textToInsert}})
	if _, err := executeBatchUpdate(ctx, g, documentID, []json.RawMessage{req}); err != nil {
		return "", wrapFailed("append to doc", err)
	}
	msg := "Successfully appended text to "
	if tabID != "" {
		msg += "tab " + tabID + " in "
	}
	msg += "document " + documentID + "."
	return msg, nil
}
