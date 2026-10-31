package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Direct port of upstream markdown-transformer/markdownToDocs.js +
// markdown-transformer/index.js (insertMarkdown / formatInsertResult).
// ALL index arithmetic uses UTF-16 code units (JS string semantics).

// MarkdownConversionError mirrors the upstream error type: appendMarkdown
// rethrows it unchanged (fastmcp wraps non-UserError failures as
// "Tool '<name>' execution failed: <message>").
type MarkdownConversionError struct{ Msg string }

func (e *MarkdownConversionError) Error() string { return e.Msg }

const (
	codeFontFamily     = "Roboto Mono"
	codeTextHex        = "#188038"
	codeBackgroundHex  = "#F1F3F4"
	cellContentOffset  = 4
	empty1x1TableSize  = 6
	codeBlockBGRed     = 0.937
	codeBlockBGGreen   = 0.945
	codeBlockBGBlue    = 0.953
	codeBlockBordRed   = 0.855
	codeBlockBordGreen = 0.863
	codeBlockBordBlue  = 0.878
)

type mdFmtEntry struct {
	kind string // bold, italic, strikethrough, code, link
	link string
}

type mdMergedFmt struct {
	bold, italic, strikethrough, code bool
	hasLink                           bool
	link                              string
}

type mdTextRange struct {
	startIndex, endIndex int
	formatting           mdMergedFmt
}

type mdListItem struct {
	startIndex          int
	endIndex            int
	hasEnd              bool
	nestingLevel        int
	bulletPreset        string
	taskPrefixProcessed bool
}

type mdCell struct {
	text       string
	isHeader   bool
	textRanges []mdTextRange
}

type mdTableState struct {
	rows        [][]mdCell
	currentRow  []mdCell
	inHeader    bool
	currentCell *mdCell
}

type mdCodeBlockRange struct {
	tableStartIndex int
	textStartIndex  int
	textEndIndex    int
	language        string
	hasLanguage     bool
}

type mdContext struct {
	currentIndex    int
	insertRequests  []json.RawMessage
	insertTexts     []string // parallel: insertText text, "" otherwise
	formatRequests  []json.RawMessage
	textRanges      []mdTextRange
	formattingStack []mdFmtEntry
	listStack       []mdFmtEntry // kind: "bullet" or "ordered"; link field reused for nothing
	listLevels      []int
	paragraphRanges []struct {
		startIndex, endIndex int
		namedStyleType       string
	}
	normalParagraphRanges []struct{ startIndex, endIndex int }
	listSpacingRanges     []struct{ startIndex, endIndex int }
	pendingListItems      []*mdListItem
	openListItemStack     []int
	hrRanges              []struct{ startIndex, endIndex int }
	codeBlockRanges       []mdCodeBlockRange
	tableState            *mdTableState
	inTableCell           bool
	tabID                 string
	titleConsumed         bool
	firstHeadingAsTitle   bool
	currentHeadingLevel   int
	hasHeading            bool
	currentParagraphStart int
	hasParagraphStart     bool
}

func convertMarkdownToRequests(markdown string, startIndex int, tabID string, firstHeadingAsTitle bool) (requests []json.RawMessage, err error) {
	if markdown == "" || jsTrim(markdown) == "" {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			if mce, ok := r.(*MarkdownConversionError); ok {
				err = mce
				return
			}
			err = &MarkdownConversionError{Msg: fmt.Sprintf("Failed to convert markdown: %v", r)}
		}
	}()
	tokens := parseMarkdownTokens(markdown)
	c := &mdContext{
		currentIndex:        startIndex,
		tabID:               tabID,
		firstHeadingAsTitle: firstHeadingAsTitle,
	}
	for _, tok := range tokens {
		processToken(tok, c)
	}
	finalizeFormatting(c)
	return append(c.insertRequests, c.formatRequests...), nil
}

func processToken(token mdToken, c *mdContext) {
	switch token.typ {
	case "heading_open":
		if len(token.tag) == 2 && token.tag[0] == 'h' && token.tag[1] >= '1' && token.tag[1] <= '9' {
			c.currentHeadingLevel = int(token.tag[1] - '0')
			c.hasHeading = true
			c.currentParagraphStart = c.currentIndex
			c.hasParagraphStart = true
		}
	case "heading_close":
		if c.hasHeading {
			useTitle := c.firstHeadingAsTitle && !c.titleConsumed && c.currentHeadingLevel == 1
			if useTitle {
				c.titleConsumed = true
			}
			style := fmt.Sprintf("HEADING_%d", c.currentHeadingLevel)
			if useTitle {
				style = "TITLE"
			}
			c.paragraphRanges = append(c.paragraphRanges, struct {
				startIndex, endIndex int
				namedStyleType       string
			}{c.currentParagraphStart, c.currentIndex, style})
			mdInsertText(c, "\n")
			c.hasHeading = false
		}
	case "paragraph_open":
		if len(c.listStack) == 0 {
			c.currentParagraphStart = c.currentIndex
			c.hasParagraphStart = true
		}
	case "paragraph_close":
		paragraphStart := c.currentParagraphStart
		hasStart := c.hasParagraphStart
		if !lastInsertEndsWithNewline(c) {
			mdInsertText(c, "\n")
		}
		if item := currentOpenListItem(c); item != nil {
			paragraphEndIndex := c.currentIndex
			if lastInsertEndsWithNewline(c) {
				paragraphEndIndex = c.currentIndex - 1
			}
			if paragraphEndIndex > item.startIndex {
				item.endIndex = paragraphEndIndex
				item.hasEnd = true
			}
		}
		if hasStart && len(c.listStack) == 0 {
			c.normalParagraphRanges = append(c.normalParagraphRanges, struct{ startIndex, endIndex int }{paragraphStart, c.currentIndex})
		}
		c.hasParagraphStart = false
	case "text":
		handleTextToken(token.content, c)
	case "code_inline":
		c.formattingStack = append(c.formattingStack, mdFmtEntry{kind: "code"})
		handleTextToken(token.content, c)
		popFormatting(c, "code")
	case "strong_open":
		c.formattingStack = append(c.formattingStack, mdFmtEntry{kind: "bold"})
	case "strong_close":
		popFormatting(c, "bold")
	case "em_open":
		c.formattingStack = append(c.formattingStack, mdFmtEntry{kind: "italic"})
	case "em_close":
		popFormatting(c, "italic")
	case "s_open":
		c.formattingStack = append(c.formattingStack, mdFmtEntry{kind: "strikethrough"})
	case "s_close":
		popFormatting(c, "strikethrough")
	case "link_open":
		if token.href != "" {
			c.formattingStack = append(c.formattingStack, mdFmtEntry{kind: "link", link: token.href})
		}
	case "link_close":
		popFormatting(c, "link")
	case "bullet_list_open":
		c.listStack = append(c.listStack, mdFmtEntry{kind: "bullet"})
		c.listLevels = append(c.listLevels, len(c.listStack)-1)
	case "ordered_list_open":
		c.listStack = append(c.listStack, mdFmtEntry{kind: "ordered"})
		c.listLevels = append(c.listLevels, len(c.listStack)-1)
	case "bullet_list_close", "ordered_list_close":
		handleListClose(c)
	case "list_item_open":
		handleListItemOpen(c)
	case "list_item_close":
		handleListItemClose(c)
	case "softbreak":
		if c.inTableCell && c.tableState != nil && c.tableState.currentCell != nil {
			c.tableState.currentCell.text += " "
		} else {
			mdInsertText(c, " ")
		}
	case "hardbreak":
		if c.inTableCell && c.tableState != nil && c.tableState.currentCell != nil {
			c.tableState.currentCell.text += "\n"
		} else {
			mdInsertText(c, "\n")
		}
	case "inline":
		for _, child := range token.children {
			processToken(child, c)
		}
	case "table_open":
		c.tableState = &mdTableState{}
	case "thead_open":
		if c.tableState != nil {
			c.tableState.inHeader = true
		}
	case "thead_close":
		if c.tableState != nil {
			c.tableState.inHeader = false
		}
	case "tbody_open", "tbody_close":
	case "tr_open":
		if c.tableState != nil {
			c.tableState.currentRow = nil
		}
	case "tr_close":
		if c.tableState != nil && len(c.tableState.currentRow) > 0 {
			c.tableState.rows = append(c.tableState.rows, c.tableState.currentRow)
			c.tableState.currentRow = nil
		}
	case "th_open", "td_open":
		if c.tableState != nil {
			c.tableState.currentCell = &mdCell{isHeader: c.tableState.inHeader || token.typ == "th_open"}
			c.inTableCell = true
		}
	case "th_close", "td_close":
		if c.tableState != nil && c.tableState.currentCell != nil {
			c.tableState.currentRow = append(c.tableState.currentRow, *c.tableState.currentCell)
			c.tableState.currentCell = nil
		}
		c.inTableCell = false
	case "table_close":
		if c.tableState != nil {
			handleTableClose(c.tableState, c)
			c.tableState = nil
			c.inTableCell = false
		}
	case "fence", "code_block":
		handleCodeBlockToken(token, c)
	case "hr":
		handleHorizontalRule(c)
	case "blockquote_open", "blockquote_close":
	}
}

func handleHorizontalRule(c *mdContext) {
	if !lastInsertEndsWithNewline(c) {
		mdInsertText(c, "\n")
	}
	start := c.currentIndex
	mdInsertText(c, "\n")
	c.hrRanges = append(c.hrRanges, struct{ startIndex, endIndex int }{start, c.currentIndex})
}

func handleListItemOpen(c *mdContext) {
	if len(c.listStack) == 0 {
		panic(&MarkdownConversionError{Msg: "List item found outside of list context"})
	}
	currentList := c.listStack[len(c.listStack)-1]
	level := c.listLevels[len(c.listLevels)-1]
	itemStart := c.currentIndex
	if level > 0 {
		mdInsertText(c, strings.Repeat("\t", level))
	}
	preset := "BULLET_DISC_CIRCLE_SQUARE"
	if currentList.kind == "ordered" {
		preset = "NUMBERED_DECIMAL_ALPHA_ROMAN"
	}
	item := &mdListItem{startIndex: itemStart, nestingLevel: level, bulletPreset: preset}
	c.pendingListItems = append(c.pendingListItems, item)
	c.openListItemStack = append(c.openListItemStack, len(c.pendingListItems)-1)
}

func handleListItemClose(c *mdContext) {
	if len(c.openListItemStack) == 0 {
		return
	}
	openIndex := c.openListItemStack[len(c.openListItemStack)-1]
	c.openListItemStack = c.openListItemStack[:len(c.openListItemStack)-1]
	listItem := c.pendingListItems[openIndex]
	if !listItem.hasEnd {
		computedEndIndex := c.currentIndex
		if lastInsertEndsWithNewline(c) {
			computedEndIndex = c.currentIndex - 1
		}
		if computedEndIndex > listItem.startIndex {
			listItem.endIndex = computedEndIndex
			listItem.hasEnd = true
		}
	}
	if !lastInsertEndsWithNewline(c) {
		mdInsertText(c, "\n")
	}
}

func handleListClose(c *mdContext) {
	c.listStack = c.listStack[:len(c.listStack)-1]
	c.listLevels = c.listLevels[:len(c.listLevels)-1]
	if len(c.listStack) == 0 {
		for i := len(c.pendingListItems) - 1; i >= 0; i-- {
			item := c.pendingListItems[i]
			if item.hasEnd && item.endIndex > item.startIndex {
				c.listSpacingRanges = append(c.listSpacingRanges, struct{ startIndex, endIndex int }{item.startIndex, item.endIndex})
				break
			}
		}
	}
}

// jsTaskPrefixRe mirrors /^\[( |x|X)\]\s+/ with JS \s (Unicode-aware).
func matchTaskPrefix(text string) int {
	if len(text) < 3 || text[0] != '[' {
		return 0
	}
	if text[1] != ' ' && text[1] != 'x' && text[1] != 'X' {
		return 0
	}
	if text[2] != ']' {
		return 0
	}
	i := 3
	for i < len(text) {
		r, size := utf8DecodeRuneInString(text[i:])
		if !isJSSpace(r) {
			break
		}
		i += size
	}
	if i == 3 {
		return 0 // \s+ requires at least one
	}
	return i
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003,
		0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return false
}

func utf8DecodeRuneInString(s string) (rune, int) {
	return utf8.DecodeRuneInString(s)
}

func handleTextToken(text string, c *mdContext) {
	if text == "" {
		return
	}
	if c.inTableCell && c.tableState != nil && c.tableState.currentCell != nil {
		cell := c.tableState.currentCell
		startIndex := utf16Len(cell.text)
		cell.text += text
		formatting := mergeFormattingStack(c.formattingStack)
		if hasFormatting(formatting) {
			cell.textRanges = append(cell.textRanges, mdTextRange{startIndex, utf16Len(cell.text), formatting})
		}
		return
	}
	currentListItem := currentOpenListItem(c)
	if currentListItem != nil && !currentListItem.taskPrefixProcessed {
		currentListItem.taskPrefixProcessed = true
		if n := matchTaskPrefix(text); n > 0 {
			currentListItem.bulletPreset = "BULLET_CHECKBOX"
			text = text[n:]
			if text == "" {
				return
			}
		}
	}
	startIndex := c.currentIndex
	endIndex := startIndex + utf16Len(text)
	mdInsertText(c, text)
	formatting := mergeFormattingStack(c.formattingStack)
	if hasFormatting(formatting) {
		c.textRanges = append(c.textRanges, mdTextRange{startIndex, endIndex, formatting})
	}
}

func handleCodeBlockToken(token mdToken, c *mdContext) {
	normalizedContent := strings.TrimSuffix(token.content, "\n")
	language := strings.TrimSpace(token.info)
	// Ensure previous content ends with a newline before inserting the table
	if len(c.insertRequests) > 0 && !lastInsertEndsWithNewline(c) {
		mdInsertText(c, "\n")
	}
	tableStartIndex := c.currentIndex
	// 1. Insert a 1x1 table
	c.insertRequests = append(c.insertRequests, mustMarshal(struct {
		InsertTable struct {
			Location docLocation `json:"location"`
			Rows     int         `json:"rows"`
			Columns  int         `json:"columns"`
		} `json:"insertTable"`
	}{InsertTable: struct {
		Location docLocation `json:"location"`
		Rows     int         `json:"rows"`
		Columns  int         `json:"columns"`
	}{Location: docLocation{Index: tableStartIndex, TabID: c.tabID}, Rows: 1, Columns: 1}}))
	c.insertTexts = append(c.insertTexts, "")
	// 2. Insert code text into the cell paragraph
	cellContentIndex := tableStartIndex + cellContentOffset
	textLength := utf16Len(normalizedContent)
	if textLength > 0 {
		mdInsertTextAt(c, cellContentIndex, normalizedContent)
	}
	// 3. Track for formatting
	c.codeBlockRanges = append(c.codeBlockRanges, mdCodeBlockRange{
		tableStartIndex: tableStartIndex,
		textStartIndex:  cellContentIndex,
		textEndIndex:    cellContentIndex + textLength,
		language:        language,
		hasLanguage:     language != "",
	})
	// 4. Advance past the table
	c.currentIndex = tableStartIndex + empty1x1TableSize + textLength
	// 5. Newline after the table
	mdInsertText(c, "\n")
}

func handleTableClose(state *mdTableState, c *mdContext) {
	rows := state.rows
	if len(rows) == 0 {
		return
	}
	numRows := len(rows)
	numCols := 0
	for _, row := range rows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	if numCols == 0 {
		return
	}
	if !lastInsertEndsWithNewline(c) {
		mdInsertText(c, "\n")
	}
	tableStartIndex := c.currentIndex
	c.insertRequests = append(c.insertRequests, mustMarshal(struct {
		InsertTable struct {
			Location docLocation `json:"location"`
			Rows     int         `json:"rows"`
			Columns  int         `json:"columns"`
		} `json:"insertTable"`
	}{InsertTable: struct {
		Location docLocation `json:"location"`
		Rows     int         `json:"rows"`
		Columns  int         `json:"columns"`
	}{Location: docLocation{Index: tableStartIndex, TabID: c.tabID}, Rows: numRows, Columns: numCols}}))
	c.insertTexts = append(c.insertTexts, "")

	cumulativeTextLength := 0
	for r := 0; r < numRows; r++ {
		for col := 0; col < numCols; col++ {
			var cell *mdCell
			if r < len(rows) && col < len(rows[r]) {
				cell = &rows[r][col]
			}
			if cell == nil || cell.text == "" {
				continue
			}
			baseCellIndex := tableStartIndex + 4 + r*(1+2*numCols) + 2*col
			adjustedIndex := baseCellIndex + cumulativeTextLength
			mdInsertTextAt(c, adjustedIndex, cell.text)
			for _, rng := range cell.textRanges {
				absStart := adjustedIndex + rng.startIndex
				absEnd := adjustedIndex + rng.endIndex
				if rng.formatting.bold || rng.formatting.italic || rng.formatting.strikethrough || rng.formatting.code {
					opts := textStyleOptions{}
					if rng.formatting.bold {
						opts.Bold = boolPtr(true)
					}
					if rng.formatting.italic {
						opts.Italic = boolPtr(true)
					}
					if rng.formatting.strikethrough {
						opts.Strikethrough = boolPtr(true)
					}
					if rng.formatting.code {
						opts.FontFamily = strPtr(codeFontFamily)
						opts.ForegroundColor = strPtr(codeTextHex)
						opts.BackgroundColor = strPtr(codeBackgroundHex)
					}
					req, err := buildUpdateTextStyleRequest(absStart, absEnd, opts, c.tabID)
					if err != nil {
						panic(err)
					}
					if req != nil {
						c.formatRequests = append(c.formatRequests, req)
					}
				}
				if rng.formatting.hasLink {
					req, err := buildUpdateTextStyleRequest(absStart, absEnd, textStyleOptions{LinkURL: strPtr(rng.formatting.link)}, c.tabID)
					if err != nil {
						panic(err)
					}
					if req != nil {
						c.formatRequests = append(c.formatRequests, req)
					}
				}
			}
			if cell.isHeader && utf16Len(cell.text) > 0 {
				req, err := buildUpdateTextStyleRequest(adjustedIndex, adjustedIndex+utf16Len(cell.text), textStyleOptions{Bold: boolPtr(true)}, c.tabID)
				if err != nil {
					panic(err)
				}
				if req != nil {
					c.formatRequests = append(c.formatRequests, req)
				}
			}
			cumulativeTextLength += utf16Len(cell.text)
		}
	}
	emptyTableSize := 3 + numRows*(1+2*numCols)
	c.currentIndex = tableStartIndex + emptyTableSize + cumulativeTextLength
	mdInsertText(c, "\n")
}

func mdInsertText(c *mdContext, text string) {
	mdInsertTextAt(c, c.currentIndex, text)
}

func mdInsertTextAt(c *mdContext, index int, text string) {
	c.insertRequests = append(c.insertRequests, mustMarshal(struct {
		InsertText struct {
			Location docLocation `json:"location"`
			Text     string      `json:"text"`
		} `json:"insertText"`
	}{InsertText: struct {
		Location docLocation `json:"location"`
		Text     string      `json:"text"`
	}{Location: docLocation{Index: index, TabID: c.tabID}, Text: text}}))
	c.insertTexts = append(c.insertTexts, text)
	c.currentIndex = index + utf16Len(text)
}

func mustMarshal(v any) json.RawMessage {
	raw, err := marshalNoEscape(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

func mergeFormattingStack(stack []mdFmtEntry) mdMergedFmt {
	var merged mdMergedFmt
	for _, state := range stack {
		switch state.kind {
		case "bold":
			merged.bold = true
		case "italic":
			merged.italic = true
		case "strikethrough":
			merged.strikethrough = true
		case "code":
			merged.code = true
		case "link":
			merged.hasLink = true
			merged.link = state.link
		}
	}
	return merged
}

func hasFormatting(f mdMergedFmt) bool {
	return f.bold || f.italic || f.strikethrough || f.code || f.hasLink
}

func popFormatting(c *mdContext, kind string) {
	for i := len(c.formattingStack) - 1; i >= 0; i-- {
		if c.formattingStack[i].kind == kind {
			c.formattingStack = append(c.formattingStack[:i], c.formattingStack[i+1:]...)
			return
		}
	}
}

func currentOpenListItem(c *mdContext) *mdListItem {
	if len(c.openListItemStack) == 0 {
		return nil
	}
	return c.pendingListItems[c.openListItemStack[len(c.openListItemStack)-1]]
}

func lastInsertEndsWithNewline(c *mdContext) bool {
	if len(c.insertTexts) == 0 {
		return false
	}
	return strings.HasSuffix(c.insertTexts[len(c.insertTexts)-1], "\n")
}

// --- finalization ---

func finalizeFormatting(c *mdContext) {
	// Character-level formatting
	for _, rng := range c.textRanges {
		if rng.formatting.bold || rng.formatting.italic || rng.formatting.strikethrough || rng.formatting.code {
			opts := textStyleOptions{}
			if rng.formatting.bold {
				opts.Bold = boolPtr(true)
			}
			if rng.formatting.italic {
				opts.Italic = boolPtr(true)
			}
			if rng.formatting.strikethrough {
				opts.Strikethrough = boolPtr(true)
			}
			if rng.formatting.code {
				opts.FontFamily = strPtr(codeFontFamily)
				opts.ForegroundColor = strPtr(codeTextHex)
				opts.BackgroundColor = strPtr(codeBackgroundHex)
			}
			req, err := buildUpdateTextStyleRequest(rng.startIndex, rng.endIndex, opts, c.tabID)
			if err != nil {
				panic(err)
			}
			if req != nil {
				c.formatRequests = append(c.formatRequests, req)
			}
		}
		if rng.formatting.hasLink {
			req, err := buildUpdateTextStyleRequest(rng.startIndex, rng.endIndex, textStyleOptions{LinkURL: strPtr(rng.formatting.link)}, c.tabID)
			if err != nil {
				panic(err)
			}
			if req != nil {
				c.formatRequests = append(c.formatRequests, req)
			}
		}
	}
	// Paragraph-level formatting (headings)
	for _, pr := range c.paragraphRanges {
		if pr.namedStyleType != "" {
			req, err := buildUpdateParagraphStyleRequest(pr.startIndex, pr.endIndex, pr.namedStyleType, c.tabID)
			if err != nil {
				panic(err)
			}
			if req != nil {
				c.formatRequests = append(c.formatRequests, req)
			}
		}
	}
	// Normal paragraph spacing
	for _, nr := range c.normalParagraphRanges {
		c.formatRequests = append(c.formatRequests, spaceBelowRequest(nr.startIndex, nr.endIndex, c.tabID))
	}
	// List trailing spacing
	for _, lr := range c.listSpacingRanges {
		c.formatRequests = append(c.formatRequests, spaceBelowRequest(lr.startIndex, lr.endIndex, c.tabID))
	}
	// Code block table formatting
	for _, cb := range c.codeBlockRanges {
		if cb.textEndIndex > cb.textStartIndex {
			req, err := buildUpdateTextStyleRequest(cb.textStartIndex, cb.textEndIndex, textStyleOptions{FontFamily: strPtr(codeFontFamily)}, c.tabID)
			if err != nil {
				panic(err)
			}
			if req != nil {
				c.formatRequests = append(c.formatRequests, req)
			}
		}
		c.formatRequests = append(c.formatRequests, codeBlockTableStyleRequest(cb.tableStartIndex+1, c.tabID))
	}
	// Horizontal rules
	for _, hr := range c.hrRanges {
		c.formatRequests = append(c.formatRequests, hrBorderRequest(hr.startIndex, hr.endIndex, c.tabID))
	}
	// List formatting: merge adjacent same-preset items, apply bottom-to-top.
	var valid []*mdListItem
	for _, item := range c.pendingListItems {
		if item.hasEnd && item.endIndex > item.startIndex {
			valid = append(valid, item)
		}
	}
	// sort by startIndex ascending (insertion order is already ascending)
	type mergedRange struct {
		startIndex, endIndex int
		bulletPreset         string
	}
	var mergedRanges []mergedRange
	for _, item := range valid {
		if len(mergedRanges) > 0 {
			last := &mergedRanges[len(mergedRanges)-1]
			if last.bulletPreset == item.bulletPreset && item.startIndex <= last.endIndex+1 {
				if item.endIndex > last.endIndex {
					last.endIndex = item.endIndex
				}
				continue
			}
		}
		mergedRanges = append(mergedRanges, mergedRange{item.startIndex, item.endIndex, item.bulletPreset})
	}
	for i := len(mergedRanges) - 1; i >= 0; i-- {
		m := mergedRanges[i]
		c.formatRequests = append(c.formatRequests, mustMarshal(struct {
			CreateParagraphBullets struct {
				Range        docRange `json:"range"`
				BulletPreset string   `json:"bulletPreset"`
			} `json:"createParagraphBullets"`
		}{CreateParagraphBullets: struct {
			Range        docRange `json:"range"`
			BulletPreset string   `json:"bulletPreset"`
		}{Range: docRange{StartIndex: m.startIndex, EndIndex: m.endIndex, TabID: c.tabID}, BulletPreset: m.bulletPreset}}))
	}
}

func spaceBelowRequest(startIndex, endIndex int, tabID string) json.RawMessage {
	return mustMarshal(struct {
		UpdateParagraphStyle struct {
			Range          docRange `json:"range"`
			ParagraphStyle struct {
				SpaceBelow dimension `json:"spaceBelow"`
			} `json:"paragraphStyle"`
			Fields string `json:"fields"`
		} `json:"updateParagraphStyle"`
	}{UpdateParagraphStyle: struct {
		Range          docRange `json:"range"`
		ParagraphStyle struct {
			SpaceBelow dimension `json:"spaceBelow"`
		} `json:"paragraphStyle"`
		Fields string `json:"fields"`
	}{Range: docRange{StartIndex: startIndex, EndIndex: endIndex, TabID: tabID},
		ParagraphStyle: struct {
			SpaceBelow dimension `json:"spaceBelow"`
		}{SpaceBelow: dimension{Magnitude: 8, Unit: "PT"}},
		Fields: "spaceBelow"}})
}

func codeBlockTableStyleRequest(tableStartLocationIndex int, tabID string) json.RawMessage {
	borderStyle := struct {
		Color     colorObject `json:"color"`
		Width     dimension   `json:"width"`
		DashStyle string      `json:"dashStyle"`
	}{
		Color:     makeColor(rgbColor{Red: codeBlockBordRed, Green: codeBlockBordGreen, Blue: codeBlockBordBlue}),
		Width:     dimension{Magnitude: 0.5, Unit: "PT"},
		DashStyle: "SOLID",
	}
	req := struct {
		UpdateTableCellStyle struct {
			TableRange struct {
				TableCellLocation struct {
					TableStartLocation docLocation `json:"tableStartLocation"`
					RowIndex           int         `json:"rowIndex"`
					ColumnIndex        int         `json:"columnIndex"`
				} `json:"tableCellLocation"`
				RowSpan    int `json:"rowSpan"`
				ColumnSpan int `json:"columnSpan"`
			} `json:"tableRange"`
			TableCellStyle struct {
				BackgroundColor colorObject `json:"backgroundColor"`
				PaddingTop      dimension   `json:"paddingTop"`
				PaddingBottom   dimension   `json:"paddingBottom"`
				PaddingLeft     dimension   `json:"paddingLeft"`
				PaddingRight    dimension   `json:"paddingRight"`
				BorderTop       interface{} `json:"borderTop"`
				BorderBottom    interface{} `json:"borderBottom"`
				BorderLeft      interface{} `json:"borderLeft"`
				BorderRight     interface{} `json:"borderRight"`
			} `json:"tableCellStyle"`
			Fields string `json:"fields"`
		} `json:"updateTableCellStyle"`
	}{}
	ts := &req.UpdateTableCellStyle
	ts.TableRange.TableCellLocation.TableStartLocation = docLocation{Index: tableStartLocationIndex, TabID: tabID}
	ts.TableRange.TableCellLocation.RowIndex = 0
	ts.TableRange.TableCellLocation.ColumnIndex = 0
	ts.TableRange.RowSpan = 1
	ts.TableRange.ColumnSpan = 1
	ts.TableCellStyle.BackgroundColor = makeColor(rgbColor{Red: codeBlockBGRed, Green: codeBlockBGGreen, Blue: codeBlockBGBlue})
	ts.TableCellStyle.PaddingTop = dimension{Magnitude: 8, Unit: "PT"}
	ts.TableCellStyle.PaddingBottom = dimension{Magnitude: 8, Unit: "PT"}
	ts.TableCellStyle.PaddingLeft = dimension{Magnitude: 12, Unit: "PT"}
	ts.TableCellStyle.PaddingRight = dimension{Magnitude: 12, Unit: "PT"}
	ts.TableCellStyle.BorderTop = borderStyle
	ts.TableCellStyle.BorderBottom = borderStyle
	ts.TableCellStyle.BorderLeft = borderStyle
	ts.TableCellStyle.BorderRight = borderStyle
	ts.Fields = "backgroundColor,paddingTop,paddingBottom,paddingLeft,paddingRight,borderTop,borderBottom,borderLeft,borderRight"
	return mustMarshal(req)
}

func hrBorderRequest(startIndex, endIndex int, tabID string) json.RawMessage {
	req := struct {
		UpdateParagraphStyle struct {
			Range          docRange `json:"range"`
			ParagraphStyle struct {
				BorderBottom struct {
					Color     colorObject `json:"color"`
					Width     dimension   `json:"width"`
					Padding   dimension   `json:"padding"`
					DashStyle string      `json:"dashStyle"`
				} `json:"borderBottom"`
			} `json:"paragraphStyle"`
			Fields string `json:"fields"`
		} `json:"updateParagraphStyle"`
	}{}
	ps := &req.UpdateParagraphStyle
	ps.Range = docRange{StartIndex: startIndex, EndIndex: endIndex, TabID: tabID}
	ps.ParagraphStyle.BorderBottom.Color = makeColor(rgbColor{Red: 0.75, Green: 0.75, Blue: 0.75})
	ps.ParagraphStyle.BorderBottom.Width = dimension{Magnitude: 1, Unit: "PT"}
	ps.ParagraphStyle.BorderBottom.Padding = dimension{Magnitude: 6, Unit: "PT"}
	ps.ParagraphStyle.BorderBottom.DashStyle = "SOLID"
	ps.Fields = "borderBottom"
	return mustMarshal(req)
}

// --- insertMarkdown (upstream markdown-transformer/index.js) ---

type insertMarkdownResult struct {
	totalRequests  int
	requestsByType []struct {
		kind  string
		count int
	}
	parseElapsedMs int
	batchUpdate    *batchUpdateResult
	totalElapsedMs int
}

func insertMarkdown(ctx context.Context, g *Google, documentID, markdown string, startIndex int, tabID string, firstHeadingAsTitle bool) (*insertMarkdownResult, error) {
	overallStart := time.Now()
	parseStart := time.Now()
	requests, err := convertMarkdownToRequests(markdown, startIndex, tabID, firstHeadingAsTitle)
	if err != nil {
		return nil, err
	}
	parseElapsedMs := roundMs(time.Since(parseStart))
	res := &insertMarkdownResult{totalRequests: len(requests), parseElapsedMs: parseElapsedMs}
	seen := map[string]int{}
	for _, r := range requests {
		kind := firstRequestKey(r)
		if idx, ok := seen[kind]; ok {
			res.requestsByType[idx].count++
		} else {
			seen[kind] = len(res.requestsByType)
			res.requestsByType = append(res.requestsByType, struct {
				kind  string
				count int
			}{kind, 1})
		}
	}
	if len(requests) == 0 {
		res.batchUpdate = &batchUpdateResult{}
		res.totalElapsedMs = roundMs(time.Since(overallStart))
		return res, nil
	}
	bu, err := executeBatchUpdateWithSplitting(ctx, g, documentID, requests)
	if err != nil {
		return nil, err
	}
	res.batchUpdate = bu
	res.totalElapsedMs = roundMs(time.Since(overallStart))
	return res, nil
}

func firstRequestKey(r json.RawMessage) string {
	dec := json.NewDecoder(strings.NewReader(string(r)))
	t, err := dec.Token()
	if err != nil {
		return ""
	}
	if delim, ok := t.(json.Delim); ok && delim == '{' {
		t, err := dec.Token()
		if err != nil {
			return ""
		}
		if s, ok := t.(string); ok {
			return s
		}
	}
	return ""
}

// formatInsertResult ports the upstream debug summary. Timings are measured
// live, like the upstream's performance.now() readings.
func formatInsertResult(result *insertMarkdownResult) string {
	lines := []string{}
	lines = append(lines, fmt.Sprintf("Markdown insert completed in %dms", result.totalElapsedMs))
	lines = append(lines, fmt.Sprintf("  Parse: %dms", result.parseElapsedMs))
	parts := []string{}
	for _, e := range result.requestsByType {
		parts = append(parts, fmt.Sprintf("%d %s", e.count, e.kind))
	}
	lines = append(lines, fmt.Sprintf("  Requests: %d total (%s)", result.totalRequests, strings.Join(parts, ", ")))
	lines = append(lines, fmt.Sprintf("  API calls: %d batchUpdate calls in %dms", result.batchUpdate.TotalAPICalls, result.batchUpdate.TotalElapsedMs))
	ph := result.batchUpdate.Phases
	if ph.Delete.Requests > 0 {
		lines = append(lines, fmt.Sprintf("    Delete phase: %d requests, %d calls, %dms", ph.Delete.Requests, ph.Delete.APICalls, ph.Delete.ElapsedMs))
	}
	if ph.Insert.Requests > 0 {
		lines = append(lines, fmt.Sprintf("    Insert phase: %d requests, %d calls, %dms", ph.Insert.Requests, ph.Insert.APICalls, ph.Insert.ElapsedMs))
	}
	if ph.Format.Requests > 0 {
		lines = append(lines, fmt.Sprintf("    Format phase: %d requests, %d calls, %dms", ph.Format.Requests, ph.Format.APICalls, ph.Format.ElapsedMs))
	}
	return strings.Join(lines, "\n")
}
