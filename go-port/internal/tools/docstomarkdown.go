package tools

import (
	"strings"
)

// docsJsonToMarkdown is a direct port of upstream
// markdown-transformer/docsToMarkdown.js: converts the (already fetched)
// Docs JSON of a body (+lists) into markdown. Deterministic; all trimming
// follows JS String.prototype.trim semantics via jsTrim.

var codeFontFamilies = map[string]bool{
	"Roboto Mono": true, "Courier New": true, "Consolas": true, "monospace": true,
}

func docsJSONToMarkdown(body *docBody, lists docLists) string {
	if body == nil || body.Content == nil {
		return ""
	}
	if lists == nil {
		lists = docLists{}
	}
	var md strings.Builder
	for _, element := range body.Content {
		if element.Paragraph != nil {
			md.WriteString(convertParagraph(element.Paragraph, lists))
		} else if element.Table != nil {
			md.WriteString(convertTable(element.Table))
		} else if element.SectionBreak != nil {
			md.WriteString("\n---\n\n")
		}
	}
	return jsTrim(md.String())
}

func convertParagraph(paragraph *docParagraph, lists docLists) string {
	headingLevel := getHeadingLevel(paragraph)
	listInfo := getListInfo(paragraph, lists)
	text := extractFormattedText(paragraph.Elements)
	trimmed := jsTrim(text)
	if headingLevel > 0 && trimmed != "" {
		level := headingLevel
		if level > 6 {
			level = 6
		}
		return strings.Repeat("#", level) + " " + trimmed + "\n\n"
	}
	if listInfo != nil && trimmed != "" {
		indent := strings.Repeat("  ", listInfo.nestingLevel)
		marker := "-"
		if listInfo.ordered {
			marker = "1."
		}
		return indent + marker + " " + trimmed + "\n"
	}
	if trimmed != "" {
		return trimmed + "\n\n"
	}
	return "\n"
}

func getHeadingLevel(paragraph *docParagraph) int {
	if paragraph.ParagraphStyle == nil || paragraph.ParagraphStyle.NamedStyleType == "" {
		return 0
	}
	st := paragraph.ParagraphStyle.NamedStyleType
	if st == "TITLE" {
		return 1
	}
	if st == "SUBTITLE" {
		return 2
	}
	if strings.HasPrefix(st, "HEADING_") && len(st) == len("HEADING_")+1 {
		d := st[len(st)-1]
		if d >= '0' && d <= '9' {
			// JS parseInt(match[1], 10) on a single digit
			return int(d - '0')
		}
	}
	return 0
}

type listInfo struct {
	ordered      bool
	nestingLevel int
}

func getListInfo(paragraph *docParagraph, lists docLists) *listInfo {
	if paragraph.Bullet == nil {
		return nil
	}
	nestingLevel := paragraph.Bullet.NestingLevel
	listID := paragraph.Bullet.ListID
	ordered := false
	if listID != "" {
		if l, ok := lists[listID]; ok && l.ListProperties != nil {
			nestingLevels := l.ListProperties.NestingLevels
			if nestingLevel < len(nestingLevels) {
				level := nestingLevels[nestingLevel]
				if level.GlyphType != "" && level.GlyphType != "GLYPH_TYPE_UNSPECIFIED" {
					ordered = true
				}
			}
		}
	}
	return &listInfo{ordered: ordered, nestingLevel: nestingLevel}
}

func extractFormattedText(elements []docParagraphElement) string {
	var result strings.Builder
	for _, element := range elements {
		if element.TextRun != nil {
			result.WriteString(convertTextRun(element.TextRun))
		}
	}
	return result.String()
}

func convertTextRun(textRun *docTextRun) string {
	text := textRun.Content
	style := textRun.TextStyle
	if style == nil {
		return text
	}
	// Code-styled (monospace) text: wrap in backticks, skip other formatting.
	if isCodeStyled(style) {
		trimmed := strings.TrimSuffix(text, "\n") // JS text.replace(/\n$/, '') — one trailing newline
		if trimmed != "" {
			out := "`" + trimmed + "`"
			if strings.HasSuffix(text, "\n") {
				out += "\n"
			}
			return out
		}
		return text
	}
	trailingNewline := strings.HasSuffix(text, "\n")
	content := text
	if trailingNewline {
		content = text[:len(text)-1]
	}
	if content == "" {
		return text
	}
	formatted := content
	if style.Bold && style.Italic {
		formatted = "***" + formatted + "***"
	} else if style.Bold {
		formatted = "**" + formatted + "**"
	} else if style.Italic {
		formatted = "*" + formatted + "*"
	}
	if style.Strikethrough {
		formatted = "~~" + formatted + "~~"
	}
	if style.Underline && style.Link == nil {
		formatted = "<u>" + formatted + "</u>"
	}
	if style.Link != nil && style.Link.URL != "" {
		formatted = "[" + formatted + "](" + style.Link.URL + ")"
	}
	if trailingNewline {
		formatted += "\n"
	}
	return formatted
}

func isCodeStyled(style *docTextStyle) bool {
	return style.WeightedFontFamily != nil && codeFontFamilies[style.WeightedFontFamily.FontFamily]
}

func convertTable(table *docTable) string {
	if len(table.TableRows) == 0 {
		return ""
	}
	if isCodeBlockTable(table) {
		return convertCodeBlockTable(table)
	}
	var md strings.Builder
	md.WriteString("\n")
	isFirstRow := true
	for _, row := range table.TableRows {
		if row.TableCells == nil {
			continue
		}
		rowText := "|"
		for _, cell := range row.TableCells {
			rowText += " " + extractCellText(&cell) + " |"
		}
		md.WriteString(rowText + "\n")
		if isFirstRow {
			separator := "|"
			for i := 0; i < len(row.TableCells); i++ {
				separator += " --- |"
			}
			md.WriteString(separator + "\n")
			isFirstRow = false
		}
	}
	md.WriteString("\n")
	return md.String()
}

func isCodeBlockTable(table *docTable) bool {
	if len(table.TableRows) != 1 {
		return false
	}
	row := table.TableRows[0]
	if len(row.TableCells) != 1 {
		return false
	}
	cell := row.TableCells[0]
	if cell.TableCellStyle != nil &&
		cell.TableCellStyle.BackgroundColor != nil &&
		cell.TableCellStyle.BackgroundColor.Color != nil &&
		cell.TableCellStyle.BackgroundColor.Color.RgbColor != nil {
		bg := cell.TableCellStyle.BackgroundColor.Color.RgbColor
		r, g, b := bg.Red, bg.Green, bg.Blue
		if r > 0.85 && g > 0.85 && b > 0.85 && r < 1 && g < 1 && b < 1 {
			return true
		}
	}
	for _, element := range cell.Content {
		if element.Paragraph != nil {
			for _, pe := range element.Paragraph.Elements {
				if pe.TextRun != nil && pe.TextRun.TextStyle != nil && isCodeStyled(pe.TextRun.TextStyle) {
					return true
				}
			}
		}
	}
	return false
}

func convertCodeBlockTable(table *docTable) string {
	cell := table.TableRows[0].TableCells[0]
	var codeText strings.Builder
	for _, element := range cell.Content {
		if element.Paragraph != nil {
			for _, pe := range element.Paragraph.Elements {
				if pe.TextRun != nil && pe.TextRun.Content != "" {
					codeText.WriteString(pe.TextRun.Content)
				}
			}
		}
	}
	ct := codeText.String()
	ct = strings.TrimSuffix(ct, "\n") // one trailing newline
	return "\n```\n" + ct + "\n```\n\n"
}

func extractCellText(cell *docTableCell) string {
	var text strings.Builder
	for _, element := range cell.Content {
		if element.Paragraph != nil {
			for _, pe := range element.Paragraph.Elements {
				if pe.TextRun != nil && pe.TextRun.Content != "" {
					text.WriteString(jsTrim(strings.ReplaceAll(pe.TextRun.Content, "\n", " ")))
				}
			}
		}
	}
	return text.String()
}

// jsTrim implements JavaScript's String.prototype.trim: strips WhiteSpace
// (tab, VT, FF, space, NBSP, ZWNBSP, Unicode space separators) and line
// terminators from both ends.
func jsTrim(s string) string {
	return strings.Trim(s, " \t\n\v\f\r\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff")
}
