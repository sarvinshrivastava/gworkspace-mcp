package tools

import (
	"context"
	"encoding/json"
)

// appendMarkdown — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/utils/appendMarkdownToGoogleDoc.js
func AppendMarkdown(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "documentId", "markdown", "addNewlineIfNeeded", "tabId", "firstHeadingAsTitle")
	if err != nil {
		return "", err
	}
	documentID, err := a.ReqString("documentId", false)
	if err != nil {
		return "", err
	}
	markdown, err := a.ReqString("markdown", true)
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
	firstHeadingAsTitle, err := a.OptBool("firstHeadingAsTitle") // default false
	if err != nil {
		return "", err
	}

	fields := "body(content(endIndex))"
	if tabID != "" {
		fields = tabBodyEndIndexFields
	}
	body, status, err := docsGet(ctx, g, documentID, fields, tabID != "")
	if err != nil {
		return "", wrapFailed("append markdown", err)
	}
	if status < 200 || status >= 300 {
		return "", &UserError{Msg: "Failed to append markdown: " + ErrMessage(body, status)}
	}
	var doc documentGetResult
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", wrapFailed("append markdown", err)
	}

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
	if bodyContent == nil {
		return "", &UserError{Msg: "No content found in document/tab"}
	}
	if len(bodyContent) == 0 {
		// JS: bodyContent[bodyContent.length-1] is undefined → TypeError,
		// caught and wrapped as "Failed to append markdown: Cannot read
		// properties of undefined (reading 'endIndex')".
		return "", &UserError{Msg: "Failed to append markdown: Cannot read properties of undefined (reading 'endIndex')"}
	}
	startIndex := bodyContent[len(bodyContent)-1].EndIndex - 1

	if addNewlineIfNeeded && startIndex > 1 {
		req := mustMarshal(struct {
			InsertText struct {
				Location docLocation `json:"location"`
				Text     string      `json:"text"`
			} `json:"insertText"`
		}{InsertText: struct {
			Location docLocation `json:"location"`
			Text     string      `json:"text"`
		}{Location: docLocation{Index: startIndex, TabID: tabID}, Text: "\n\n"}})
		if _, err := executeBatchUpdate(ctx, g, documentID, []json.RawMessage{req}); err != nil {
			return "", wrapFailed("append markdown", err)
		}
		startIndex += 2
	}
	result, err := insertMarkdown(ctx, g, documentID, markdown, startIndex, tabID, firstHeadingAsTitle)
	if err != nil {
		return "", wrapFailed("append markdown", err)
	}
	debugSummary := formatInsertResult(result)
	return "Successfully appended " + itoa(utf16Len(markdown)) + " characters of markdown.\n\n" + debugSummary, nil
}
