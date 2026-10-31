package tools

import (
	"context"
	"encoding/json"
)

// findAndReplace — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/docs/findAndReplace.js
func FindAndReplace(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "documentId", "findText", "replaceText", "matchCase", "tabId")
	if err != nil {
		return "", err
	}
	documentID, err := a.ReqString("documentId", false)
	if err != nil {
		return "", err
	}
	findText, err := a.ReqString("findText", true)
	if err != nil {
		return "", err
	}
	replaceText, err := a.ReqString("replaceText", false) // required, may be empty
	if err != nil {
		return "", err
	}
	matchCase, err := a.OptBool("matchCase") // optional, no zod default → ?? false
	if err != nil {
		return "", err
	}
	tabID, err := a.OptString("tabId")
	if err != nil {
		return "", err
	}

	// Key order matches the upstream object literal:
	// containsText, replaceText, tabsCriteria (only when tabId).
	type containsText struct {
		Text      string `json:"text"`
		MatchCase bool   `json:"matchCase"`
	}
	type replaceAllTextReq struct {
		ContainsText containsText `json:"containsText"`
		ReplaceText  string       `json:"replaceText"`
		TabsCriteria *struct {
			TabIDs []string `json:"tabIds"`
		} `json:"tabsCriteria,omitempty"`
	}
	reqBody := struct {
		ReplaceAllText replaceAllTextReq `json:"replaceAllText"`
	}{ReplaceAllText: replaceAllTextReq{
		ContainsText: containsText{Text: findText, MatchCase: matchCase},
		ReplaceText:  replaceText,
	}}
	if tabID != "" {
		reqBody.ReplaceAllText.TabsCriteria = &struct {
			TabIDs []string `json:"tabIds"`
		}{TabIDs: []string{tabID}}
	}
	req := mustMarshal(reqBody)
	resp, err := executeBatchUpdate(ctx, g, documentID, []json.RawMessage{req})
	if err != nil {
		return "", wrapFailed("find and replace", err)
	}
	changed := 0
	if len(resp.Replies) > 0 && resp.Replies[0].ReplaceAllText != nil {
		changed = resp.Replies[0].ReplaceAllText.OccurrencesChanged
	}
	return `Replaced ` + itoa(changed) + ` occurrence(s) of "` + findText + `" with "` + replaceText + `".`, nil
}
