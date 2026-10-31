package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This file ports the pieces of upstream googleDocsApiHelpers.js that the
// Docs tools uses: documents.get, findTabById/getDocumentTab,
// executeBatchUpdate (+WithSplitting), the text/paragraph style request
// builders, and hexToRgbColor.

// --- tab field masks (upstream tools/docs/tabFieldMasks.js) ---

const tabIDProperties = "tabProperties(tabId)"

// buildTabsFieldMask builds a tabs field mask with childTabs recursion 3
// levels deep, exactly as upstream buildTabsFieldMask.
func buildTabsFieldMask(documentTabFields string) string {
	child3 := "childTabs(" + tabIDProperties + "," + documentTabFields + ")"
	child2 := "childTabs(" + tabIDProperties + "," + documentTabFields + "," + child3 + ")"
	child1 := "childTabs(" + tabIDProperties + "," + documentTabFields + "," + child2 + ")"
	return "tabs(" + tabIDProperties + "," + documentTabFields + "," + child1 + ")"
}

// tabBodyEndIndexFields is upstream TAB_BODY_END_INDEX_FIELDS.
var tabBodyEndIndexFields = buildTabsFieldMask("documentTab(body(content(endIndex)))")

// --- documents.get ---

// docsGet performs docs.documents.get with the exact parameters every
// upstream tool passes (includeTabsContent + suggestionsViewMode always sent).
func docsGet(ctx context.Context, g *Google, documentID, fields string, includeTabs bool) ([]byte, int, error) {
	u := JoinURL(DocsBase+"/documents", documentID) + "?" + Query(
		"includeTabsContent", strconv.FormatBool(includeTabs),
		"suggestionsViewMode", "PREVIEW_WITHOUT_SUGGESTIONS",
		"fields", fields,
	)
	return g.Do(ctx, http.MethodGet, u, nil)
}

// --- response shapes ---

type docBody struct {
	Content []docElement `json:"content"`
}

type docElement struct {
	StartIndex   int            `json:"startIndex"`
	EndIndex     int            `json:"endIndex"`
	Paragraph    *docParagraph  `json:"paragraph"`
	Table        *docTable      `json:"table"`
	SectionBreak map[string]any `json:"sectionBreak"`
}

type docParagraph struct {
	Elements       []docParagraphElement `json:"elements"`
	ParagraphStyle *struct {
		NamedStyleType string `json:"namedStyleType"`
	} `json:"paragraphStyle"`
	Bullet *struct {
		ListID       string `json:"listId"`
		NestingLevel int    `json:"nestingLevel"`
	} `json:"bullet"`
}

type docParagraphElement struct {
	TextRun *docTextRun `json:"textRun"`
}

type docTextRun struct {
	Content   string        `json:"content"`
	TextStyle *docTextStyle `json:"textStyle"`
}

type docTextStyle struct {
	Bold          bool `json:"bold"`
	Italic        bool `json:"italic"`
	Underline     bool `json:"underline"`
	Strikethrough bool `json:"strikethrough"`
	Link          *struct {
		URL string `json:"url"`
	} `json:"link"`
	WeightedFontFamily *struct {
		FontFamily string `json:"fontFamily"`
	} `json:"weightedFontFamily"`
}

type docTable struct {
	TableRows []docTableRow `json:"tableRows"`
}

type docTableRow struct {
	TableCells []docTableCell `json:"tableCells"`
}

type docTableCell struct {
	Content        []docElement `json:"content"`
	TableCellStyle *struct {
		BackgroundColor *struct {
			Color *struct {
				RgbColor *struct {
					Red   float64 `json:"red"`
					Green float64 `json:"green"`
					Blue  float64 `json:"blue"`
				} `json:"rgbColor"`
			} `json:"color"`
		} `json:"backgroundColor"`
	} `json:"tableCellStyle"`
}

type docLists map[string]struct {
	ListProperties *struct {
		NestingLevels []struct {
			GlyphType string `json:"glyphType"`
		} `json:"nestingLevels"`
	} `json:"listProperties"`
}

type docTab struct {
	TabProperties *struct {
		TabID string `json:"tabId"`
		Title string `json:"title"`
	} `json:"tabProperties"`
	DocumentTab *struct {
		Body  json.RawMessage `json:"body"`
		Lists docLists        `json:"lists"`
	} `json:"documentTab"`
	ChildTabs []*docTab `json:"childTabs"`
}

// documentGetResult is the parsed documents.get response. BodyRaw keeps the
// raw body for readDocument format=json passthrough.
type documentGetResult struct {
	BodyRaw json.RawMessage `json:"body"`
	Tabs    []*docTab       `json:"tabs"`
}

// findTabById is upstream GDocsHelpers.findTabById: recursive search through
// child tabs; nil when absent.
func findTabById(tabs []*docTab, tabID string) *docTab {
	for _, tab := range tabs {
		if tab.TabProperties != nil && tab.TabProperties.TabID == tabID {
			return tab
		}
		if len(tab.ChildTabs) > 0 {
			if found := findTabById(tab.ChildTabs, tabID); found != nil {
				return found
			}
		}
	}
	return nil
}

// --- batchUpdate ---

const maxBatchUpdateRequests = 50 // Google API limits batch size

// batchUpdateResponse mirrors the pieces of the batchUpdate reply the tools
// read.
type batchUpdateResponse struct {
	Replies []struct {
		ReplaceAllText *struct {
			OccurrencesChanged int `json:"occurrencesChanged"`
		} `json:"replaceAllText"`
	} `json:"replies"`
}

// executeBatchUpdate is upstream GDocsHelpers.executeBatchUpdate: POST
// documents.batchUpdate with {requests}, with the upstream's error
// translation table (400 Invalid requests / 404 / 403 / generic Error).
func executeBatchUpdate(ctx context.Context, g *Google, documentID string, requests []json.RawMessage) (*batchUpdateResponse, error) {
	if len(requests) == 0 {
		return &batchUpdateResponse{}, nil
	}
	body := `{"requests":[` + string(joinRaw(requests, ",")) + `]}`
	u := JoinURL(DocsBase+"/documents", documentID) + ":batchUpdate"
	respBody, status, err := g.Do(ctx, http.MethodPost, u, json.RawMessage(body))
	if err != nil {
		return nil, err
	}
	if status >= 200 && status < 300 {
		var out batchUpdateResponse
		if err := json.Unmarshal(respBody, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	msg := ErrMessage(respBody, status)
	if status == 400 && strings.Contains(msg, "Invalid requests") {
		var parsed struct {
			Error struct {
				Details []struct {
					Description string `json:"description"`
				} `json:"details"`
			} `json:"error"`
		}
		detailMsg := ""
		if json.Unmarshal(respBody, &parsed) == nil && len(parsed.Error.Details) > 0 {
			parts := make([]string, 0, len(parsed.Error.Details))
			for _, d := range parsed.Error.Details {
				if d.Description != "" {
					parts = append(parts, d.Description)
				} else {
					raw, _ := json.Marshal(d)
					parts = append(parts, string(raw))
				}
			}
			detailMsg = strings.Join(parts, "; ")
		}
		if detailMsg == "" {
			detailMsg = msg
		}
		return nil, &UserError{Msg: "Invalid request sent to Google Docs API. Details: " + detailMsg}
	}
	if status == 404 {
		return nil, &UserError{Msg: fmt.Sprintf("Document not found (ID: %s). Check the ID.", documentID)}
	}
	if status == 403 {
		return nil, &UserError{Msg: fmt.Sprintf("Permission denied for document (ID: %s). Ensure the authenticated user has edit access.", documentID)}
	}
	// Generic internal error (upstream throws a plain Error here; the calling
	// tool wraps it into its own UserError template).
	return nil, fmt.Errorf("Google API Error (%d): %s", status, msg)
}

// marshalNoEscape is json.Marshal without HTML escaping: JSON.stringify does
// not escape <, >, & and neither may the request bodies we send.
func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func joinRaw(items []json.RawMessage, sep string) []byte {
	var b []byte
	for i, it := range items {
		if i > 0 {
			b = append(b, sep...)
		}
		b = append(b, it...)
	}
	return b
}

// --- executeBatchUpdateWithSplitting (upstream googleDocsApiHelpers.js) ---

type batchPhase struct {
	Requests  int `json:"requests"`
	APICalls  int `json:"apiCalls"`
	ElapsedMs int `json:"elapsedMs"`
}

type batchUpdateResult struct {
	TotalRequests int `json:"totalRequests"`
	Phases        struct {
		Delete batchPhase `json:"delete"`
		Insert batchPhase `json:"insert"`
		Format batchPhase `json:"format"`
	} `json:"phases"`
	TotalAPICalls  int `json:"totalApiCalls"`
	TotalElapsedMs int `json:"totalElapsedMs"`
}

var insertRequestKeys = map[string]bool{
	"insertText": true, "insertTable": true, "insertPageBreak": true,
	"insertInlineImage": true, "insertSectionBreak": true,
}

func requestKind(r json.RawMessage) string {
	var probe map[string]json.RawMessage
	if json.Unmarshal(r, &probe) != nil {
		return "format"
	}
	if _, ok := probe["deleteContentRange"]; ok {
		return "delete"
	}
	for k := range probe {
		if insertRequestKeys[k] {
			return "insert"
		}
	}
	return "format"
}

func roundMs(d time.Duration) int {
	return int(math.Round(float64(d) / float64(time.Millisecond)))
}

// executeBatchUpdateWithSplitting ports the upstream splitter: delete phase,
// then insert, then format; each phase chunked into <=50-request calls.
func executeBatchUpdateWithSplitting(ctx context.Context, g *Google, documentID string, requests []json.RawMessage) (*batchUpdateResult, error) {
	overallStart := time.Now()
	res := &batchUpdateResult{}
	if len(requests) == 0 {
		return res, nil
	}
	var deletes, inserts, formats []json.RawMessage
	for _, r := range requests {
		switch requestKind(r) {
		case "delete":
			deletes = append(deletes, r)
		case "insert":
			inserts = append(inserts, r)
		default:
			formats = append(formats, r)
		}
	}
	totalAPICalls := 0
	runPhase := func(batch []json.RawMessage, dst *batchPhase) error {
		start := time.Now()
		for i := 0; i < len(batch); i += maxBatchUpdateRequests {
			end := i + maxBatchUpdateRequests
			if end > len(batch) {
				end = len(batch)
			}
			if _, err := executeBatchUpdate(ctx, g, documentID, batch[i:end]); err != nil {
				return err
			}
			totalAPICalls++
		}
		dst.Requests = len(batch)
		if len(batch) > 0 {
			dst.APICalls = (len(batch) + maxBatchUpdateRequests - 1) / maxBatchUpdateRequests
		}
		dst.ElapsedMs = roundMs(time.Since(start))
		return nil
	}
	if len(deletes) > 0 {
		if err := runPhase(deletes, &res.Phases.Delete); err != nil {
			return nil, err
		}
	}
	if len(inserts) > 0 {
		if err := runPhase(inserts, &res.Phases.Insert); err != nil {
			return nil, err
		}
	}
	if len(formats) > 0 {
		if err := runPhase(formats, &res.Phases.Format); err != nil {
			return nil, err
		}
	}
	res.TotalRequests = len(requests)
	res.TotalAPICalls = totalAPICalls
	res.TotalElapsedMs = roundMs(time.Since(overallStart))
	return res, nil
}

// --- request builders (ordered key emission, JSON.stringify parity) ---

type dimension struct {
	Magnitude float64 `json:"magnitude"`
	Unit      string  `json:"unit"`
}

type rgbColor struct {
	Red   float64 `json:"red"`
	Green float64 `json:"green"`
	Blue  float64 `json:"blue"`
}

// colorObject is { color: { rgbColor: {...} } }.
type colorObject struct {
	Color struct {
		RgbColor rgbColor `json:"rgbColor"`
	} `json:"color"`
}

func makeColor(c rgbColor) colorObject {
	var o colorObject
	o.Color.RgbColor = c
	return o
}

type docRange struct {
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	TabID      string `json:"tabId,omitempty"`
}

type docLocation struct {
	Index int    `json:"index"`
	TabID string `json:"tabId,omitempty"`
}

// textStyleOptions mirrors the style argument of
// buildUpdateTextStyleRequest; pointers distinguish undefined from false.
type textStyleOptions struct {
	Bold            *bool
	Italic          *bool
	Underline       *bool
	Strikethrough   *bool
	FontSize        *float64
	FontFamily      *string
	ForegroundColor *string
	BackgroundColor *string
	LinkURL         *string
}

// orderedTextStyle serializes with the upstream key order: bold, italic,
// underline, strikethrough, fontSize, weightedFontFamily, foregroundColor,
// backgroundColor, link.
type orderedTextStyle struct {
	Bold               *bool        `json:"bold,omitempty"`
	Italic             *bool        `json:"italic,omitempty"`
	Underline          *bool        `json:"underline,omitempty"`
	Strikethrough      *bool        `json:"strikethrough,omitempty"`
	FontSize           *dimension   `json:"fontSize,omitempty"`
	WeightedFontFamily *fontFamily  `json:"weightedFontFamily,omitempty"`
	ForegroundColor    *colorObject `json:"foregroundColor,omitempty"`
	BackgroundColor    *colorObject `json:"backgroundColor,omitempty"`
	Link               *linkObj     `json:"link,omitempty"`
}

type fontFamily struct {
	FontFamily string `json:"fontFamily"`
}

type linkObj struct {
	URL string `json:"url"`
}

// hexToRgbColor is upstream hexToRgbColor: #RGB or #RRGGBB (with or without
// the leading #), each channel /255. Returns nil when invalid.
func hexToRgbColor(hex string) *rgbColor {
	if hex == "" {
		return nil
	}
	clean := strings.TrimPrefix(hex, "#")
	if len(clean) == 3 {
		clean = string([]byte{clean[0], clean[0], clean[1], clean[1], clean[2], clean[2]})
	}
	if len(clean) != 6 {
		return nil
	}
	n, err := strconv.ParseUint(clean, 16, 32)
	if err != nil {
		return nil
	}
	return &rgbColor{
		Red:   float64((n>>16)&255) / 255,
		Green: float64((n>>8)&255) / 255,
		Blue:  float64(n&255) / 255,
	}
}

// buildUpdateTextStyleRequest is upstream buildUpdateTextStyleRequest.
// Returns nil when no styles apply; UserError on invalid hex colors.
func buildUpdateTextStyleRequest(startIndex, endIndex int, style textStyleOptions, tabID string) (json.RawMessage, error) {
	var ts orderedTextStyle
	var fields []string
	if style.Bold != nil {
		ts.Bold = style.Bold
		fields = append(fields, "bold")
	}
	if style.Italic != nil {
		ts.Italic = style.Italic
		fields = append(fields, "italic")
	}
	if style.Underline != nil {
		ts.Underline = style.Underline
		fields = append(fields, "underline")
	}
	if style.Strikethrough != nil {
		ts.Strikethrough = style.Strikethrough
		fields = append(fields, "strikethrough")
	}
	if style.FontSize != nil {
		ts.FontSize = &dimension{Magnitude: *style.FontSize, Unit: "PT"}
		fields = append(fields, "fontSize")
	}
	if style.FontFamily != nil {
		ts.WeightedFontFamily = &fontFamily{FontFamily: *style.FontFamily}
		fields = append(fields, "weightedFontFamily")
	}
	if style.ForegroundColor != nil {
		rgb := hexToRgbColor(*style.ForegroundColor)
		if rgb == nil {
			return nil, &UserError{Msg: "Invalid foreground hex color format: " + *style.ForegroundColor}
		}
		c := makeColor(*rgb)
		ts.ForegroundColor = &c
		fields = append(fields, "foregroundColor")
	}
	if style.BackgroundColor != nil {
		rgb := hexToRgbColor(*style.BackgroundColor)
		if rgb == nil {
			return nil, &UserError{Msg: "Invalid background hex color format: " + *style.BackgroundColor}
		}
		c := makeColor(*rgb)
		ts.BackgroundColor = &c
		fields = append(fields, "backgroundColor")
	}
	if style.LinkURL != nil {
		ts.Link = &linkObj{URL: *style.LinkURL}
		fields = append(fields, "link")
	}
	if len(fields) == 0 {
		return nil, nil
	}
	req := struct {
		UpdateTextStyle struct {
			Range     docRange         `json:"range"`
			TextStyle orderedTextStyle `json:"textStyle"`
			Fields    string           `json:"fields"`
		} `json:"updateTextStyle"`
	}{}
	req.UpdateTextStyle.Range = docRange{StartIndex: startIndex, EndIndex: endIndex, TabID: tabID}
	req.UpdateTextStyle.TextStyle = ts
	req.UpdateTextStyle.Fields = strings.Join(fields, ",")
	return marshalNoEscape(req)
}

// buildUpdateParagraphStyleRequest is upstream
// buildUpdateParagraphStyleRequest (only the options the Docs tools uses:
// namedStyleType plus the general fields).
func buildUpdateParagraphStyleRequest(startIndex, endIndex int, namedStyleType string, tabID string) (json.RawMessage, error) {
	if namedStyleType == "" {
		return nil, nil
	}
	req := struct {
		UpdateParagraphStyle struct {
			Range          docRange `json:"range"`
			ParagraphStyle struct {
				NamedStyleType string `json:"namedStyleType"`
			} `json:"paragraphStyle"`
			Fields string `json:"fields"`
		} `json:"updateParagraphStyle"`
	}{}
	req.UpdateParagraphStyle.Range = docRange{StartIndex: startIndex, EndIndex: endIndex, TabID: tabID}
	req.UpdateParagraphStyle.ParagraphStyle.NamedStyleType = namedStyleType
	req.UpdateParagraphStyle.Fields = "namedStyleType"
	return marshalNoEscape(req)
}
