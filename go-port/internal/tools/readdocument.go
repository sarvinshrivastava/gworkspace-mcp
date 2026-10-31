package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// readDocument — upstream reference:
// @a-bonus/google-docs-mcp@1.11.3 dist/tools/docs/readGoogleDoc.js
func ReadDocument(ctx context.Context, g *Google, raw json.RawMessage) (string, error) {
	a, err := ParseArgs(raw, "documentId", "format", "maxLength", "tabId")
	if err != nil {
		return "", err
	}
	documentID, err := a.ReqString("documentId", false)
	if err != nil {
		return "", err
	}
	format, err := a.OptEnum("format", "text", "text", "json", "markdown")
	if err != nil {
		return "", err
	}
	maxLength, hasMaxLength, err := a.argOptNumber("maxLength")
	if err != nil {
		return "", err
	}
	if !hasMaxLength {
		maxLength = 0 // JS: undefined is falsy → no truncation
	}
	tabID, err := a.OptString("tabId")
	if err != nil {
		return "", err
	}

	needsTabsContent := tabID != ""
	var fields string
	if needsTabsContent {
		fields = "title,documentId," + buildTabsFieldMask("documentTab(body,documentStyle,namedStyles,lists)")
	} else if format == "json" || format == "markdown" {
		fields = "*"
	} else {
		fields = "body(content(paragraph(elements(textRun(content)))))"
	}
	body, status, err := docsGet(ctx, g, documentID, fields, needsTabsContent)
	if err != nil {
		return "", readDocGenericError(documentID, err.Error(), 0, false)
	}
	if status == 404 {
		return "", &UserError{Msg: "Doc not found (ID: " + documentID + ")."}
	}
	if status == 403 {
		// Drive export fallback for plain-text format.
		if format == "text" {
			exportURL := JoinURL(DriveBase+"/files", documentID) + "/export?" + Query("mimeType", "text/plain")
			ebody, estatus, eerr := g.Do(ctx, http.MethodGet, exportURL, nil)
			if eerr == nil && estatus >= 200 && estatus < 300 {
				textContent := string(ebody)
				if jsTrim(textContent) == "" {
					return "Document found, but appears empty.", nil
				}
				if maxLength != 0 && float64(utf16Len(textContent)) > maxLength {
					return "Content (truncated to " + jsNum(maxLength) + " chars of " +
						itoa(utf16Len(textContent)) + " total):\n---\n" +
						jsSubstr(textContent, maxLength) +
						"\n\n... [Document continues. Use maxLength parameter to adjust limit or remove it to get full content.]", nil
				}
				return "Content (" + itoa(utf16Len(textContent)) + " characters):\n---\n" + textContent, nil
			}
			// export failed: fall through to the 403 UserError
		}
		return "", &UserError{Msg: "Permission denied for doc (ID: " + documentID + "). The Google Docs API may be restricted by your Workspace admin."}
	}
	if status < 200 || status >= 300 {
		return "", readDocGenericError(documentID, httpErrorText(body, status), httpErrorCode(body, status), true)
	}

	var doc documentGetResult
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", readDocGenericError(documentID, err.Error(), 0, false)
	}

	// Resolve the content source (whole doc or one tab).
	var bodyRaw json.RawMessage
	var lists docLists
	if tabID != "" {
		targetTab := findTabById(doc.Tabs, tabID)
		if targetTab == nil {
			return "", &UserError{Msg: `Tab with ID "` + tabID + `" not found in document.`}
		}
		if targetTab.DocumentTab == nil {
			return "", &UserError{Msg: `Tab "` + tabID + `" does not have content (may not be a document tab).`}
		}
		bodyRaw = targetTab.DocumentTab.Body
		// upstream: contentSource = { body: targetTab.documentTab.body } — no lists
	} else {
		bodyRaw = doc.BodyRaw
		var root struct {
			Lists docLists `json:"lists"`
		}
		_ = json.Unmarshal(body, &root)
		lists = root.Lists
	}

	switch format {
	case "json":
		var jsonContent string
		if tabID != "" {
			if bodyRaw == nil {
				jsonContent = "{}" // JSON.stringify({body: undefined}, null, 2)
			} else {
				jsonContent = indentUnderKey("body", bodyRaw)
			}
		} else {
			jsonContent = jsStringifyRaw(body, 0)
		}
		if maxLength != 0 && float64(utf16Len(jsonContent)) > maxLength {
			return jsSubstr(jsonContent, maxLength) +
				"\n... [JSON truncated: " + itoa(utf16Len(jsonContent)) + " total chars]", nil
		}
		return jsonContent, nil

	case "markdown":
		var parsedBody *docBody
		if bodyRaw != nil {
			var b docBody
			if err := json.Unmarshal(bodyRaw, &b); err != nil {
				return "", readDocGenericError(documentID, err.Error(), 0, false)
			}
			parsedBody = &b
		}
		markdownContent := docsJSONToMarkdown(parsedBody, lists)
		totalLength := utf16Len(markdownContent)
		if maxLength != 0 && float64(totalLength) > maxLength {
			return jsSubstr(markdownContent, maxLength) +
				"\n\n... [Markdown truncated to " + jsNum(maxLength) + " chars of " + itoa(totalLength) +
				" total. Use maxLength parameter to adjust limit or remove it to get full content.]", nil
		}
		return markdownContent, nil

	default: // text
		var parsedBody *docBody
		if bodyRaw != nil {
			var b docBody
			if err := json.Unmarshal(bodyRaw, &b); err != nil {
				return "", readDocGenericError(documentID, err.Error(), 0, false)
			}
			parsedBody = &b
		}
		var textContent strings.Builder
		if parsedBody != nil {
			extractTextElements(parsedBody.Content, &textContent)
		}
		text := textContent.String()
		if jsTrim(text) == "" {
			return "Document found, but appears empty.", nil
		}
		totalLength := utf16Len(text)
		if maxLength != 0 && float64(totalLength) > maxLength {
			return "Content (truncated to " + jsNum(maxLength) + " chars of " + itoa(totalLength) + " total):\n---\n" +
				jsSubstr(text, maxLength) +
				"\n\n... [Document continues for " + jsNum(float64(totalLength)-maxLength) + " more characters. Use maxLength parameter to adjust limit or remove it to get full content.]", nil
		}
		return "Content (" + itoa(totalLength) + " characters):\n---\n" + text, nil
	}
}

// extractTextElements ports the upstream extractFromElements recursion
// (paragraph textRuns and table cells, nesting included).
func extractTextElements(elements []docElement, sb *strings.Builder) {
	for _, el := range elements {
		if el.Paragraph != nil && el.Paragraph.Elements != nil {
			for _, pe := range el.Paragraph.Elements {
				if pe.TextRun != nil && pe.TextRun.Content != "" {
					sb.WriteString(pe.TextRun.Content)
				}
			}
		}
		if el.Table != nil && el.Table.TableRows != nil {
			for _, row := range el.Table.TableRows {
				for _, cell := range row.TableCells {
					extractTextElements(cell.Content, sb)
				}
			}
		}
	}
}

// readDocGenericError is the upstream generic catch:
// `Failed to read doc: ${errorDetails}${errorCode ? ` (Code: ${errorCode})` : ”}`
func readDocGenericError(documentID, details string, code int, hasCode bool) error {
	if details == "" {
		details = "Unknown error"
	}
	msg := "Failed to read doc: " + details
	if hasCode {
		msg += " (Code: " + itoa(code) + ")"
	}
	return &UserError{Msg: msg}
}

func itoa(n int) string { return strconv.Itoa(n) }
