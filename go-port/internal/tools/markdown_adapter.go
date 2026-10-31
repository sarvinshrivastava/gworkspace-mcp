package tools

import (
	"html"
	"strings"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// markdown-it → this file: the upstream markdown-transformer consumes a
// markdown-it token stream. goldmark gives us a CommonMark-compliant AST;
// this adapter converts that AST into the exact token shapes the upstream
// processToken loop consumes (heading_open, paragraph_open, inline, text,
// code_inline, strong/em/s open/close, link_open/close, list tokens,
// softbreak/hardbreak, table tokens, fence, code_block, hr, blockquote).
// markdown-it runs with html:false, so HTML becomes literal text — RawHTML
// nodes are folded into text and HTMLBlock nodes become paragraphs.

type mdToken struct {
	typ      string
	tag      string
	content  string
	info     string
	href     string
	children []mdToken
}

// mdParser parses with the extensions matching markdown-it's default preset
// plus its enabled options: tables, ~~strikethrough~~, linkify. Task list
// syntax is NOT enabled (upstream handles [ ]/[x] prefixes itself), and
// typographer is off.
var mdParser = goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify))

func parseMarkdownTokens(markdown string) []mdToken {
	src := []byte(markdown)
	doc := mdParser.Parser().Parse(text.NewReader(src))
	var tokens []mdToken
	blocks := blockChildren(doc)
	for i, b := range blocks {
		// markdown-it with html:false treats what goldmark calls an HTML
		// block as plain paragraph lines. When such a block directly follows
		// a paragraph (no blank line), markdown-it keeps it IN that
		// paragraph as a lazy continuation; goldmark splits the nodes, so we
		// re-merge adjacent paragraph/HTML runs here.
		_, isHTML := b.(*gast.HTMLBlock)
		_, isPara := b.(*gast.Paragraph)
		if (isHTML || isPara) && len(tokens) > 0 && len(tokens[len(tokens)-1].typ) > 0 &&
			tokens[len(tokens)-1].typ == "paragraph_close" && i > 0 {
			prev := blocks[i-1]
			_, prevHTML := prev.(*gast.HTMLBlock)
			_, prevPara := prev.(*gast.Paragraph)
			if (prevHTML || prevPara) && blocksAdjacent(src, prev, b) {
				// drop the close, merge content, re-close
				tokens = tokens[:len(tokens)-1]
				inlineIdx := len(tokens) - 1
				for range tokens {
					if tokens[inlineIdx].typ == "inline" {
						break
					}
					inlineIdx--
				}
				merged := tokens[inlineIdx]
				merged.children = append(merged.children, mdToken{typ: "softbreak", tag: "br"})
				merged.children = append(merged.children, inlineChildren(b, src)...)
				tokens[inlineIdx] = merged
				tokens = append(tokens, mdToken{typ: "paragraph_close", tag: "p"})
				continue
			}
		}
		tokens = append(tokens, blockTokens(b, src)...)
	}
	return tokens
}

func blockChildren(n gast.Node) []gast.Node {
	var out []gast.Node
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		out = append(out, c)
	}
	return out
}

// blocksAdjacent reports whether block b starts on the line immediately
// after block prev ends (no blank line between), using source offsets.
func blocksAdjacent(src []byte, prev, b gast.Node) bool {
	lastLine := func(n gast.Node) (text.Segment, bool) {
		switch t := n.(type) {
		case *gast.Paragraph:
			if t.Lines().Len() > 0 {
				return t.Lines().At(t.Lines().Len() - 1), true
			}
		case *gast.HTMLBlock:
			if t.Lines().Len() > 0 {
				return t.Lines().At(t.Lines().Len() - 1), true
			}
		}
		return text.Segment{}, false
	}
	firstLine := func(n gast.Node) (text.Segment, bool) {
		switch t := n.(type) {
		case *gast.Paragraph:
			if t.Lines().Len() > 0 {
				return t.Lines().At(0), true
			}
		case *gast.HTMLBlock:
			if t.Lines().Len() > 0 {
				return t.Lines().At(0), true
			}
		}
		return text.Segment{}, false
	}
	pseg, ok1 := lastLine(prev)
	bseg, ok2 := firstLine(b)
	if !ok1 || !ok2 {
		return false
	}
	between := src[pseg.Stop:bseg.Start]
	return !strings.Contains(string(between), "\n")
}

func blockTokens(n gast.Node, src []byte) []mdToken {
	switch t := n.(type) {
	case *gast.Paragraph, *gast.TextBlock:
		return []mdToken{
			{typ: "paragraph_open", tag: "p"},
			{typ: "inline", children: inlineChildren(n, src)},
			{typ: "paragraph_close", tag: "p"},
		}
	case *gast.HTMLBlock:
		// html:false — literal text paragraph; lines joined by softbreaks.
		var children []mdToken
		var buf strings.Builder
		flush := func() {
			if buf.Len() > 0 {
				children = append(children, mdToken{typ: "text", content: buf.String()})
				buf.Reset()
			}
		}
		for i := 0; i < t.Lines().Len(); i++ {
			seg := t.Lines().At(i)
			line := strings.TrimSuffix(string(seg.Value(src)), "\n")
			if i > 0 {
				flush()
				children = append(children, mdToken{typ: "softbreak", tag: "br"})
			}
			buf.WriteString(line)
		}
		flush()
		return []mdToken{
			{typ: "paragraph_open", tag: "p"},
			{typ: "inline", children: children},
			{typ: "paragraph_close", tag: "p"},
		}
	case *gast.Heading:
		tag := "h" + string(rune('0'+t.Level))
		return []mdToken{
			{typ: "heading_open", tag: tag},
			{typ: "inline", children: inlineChildren(n, src)},
			{typ: "heading_close", tag: tag},
		}
	case *gast.List:
		var out []mdToken
		open := "bullet_list_open"
		close_ := "bullet_list_close"
		tag := "ul"
		if t.IsOrdered() {
			open, close_, tag = "ordered_list_open", "ordered_list_close", "ol"
		}
		out = append(out, mdToken{typ: open, tag: tag})
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if _, ok := c.(*gast.ListItem); ok {
				out = append(out, mdToken{typ: "list_item_open", tag: "li"})
				for _, sub := range blockChildren(c) {
					out = append(out, blockTokens(sub, src)...)
				}
				out = append(out, mdToken{typ: "list_item_close", tag: "li"})
			}
		}
		out = append(out, mdToken{typ: close_, tag: tag})
		return out
	case *gast.Blockquote:
		out := []mdToken{{typ: "blockquote_open", tag: "blockquote"}}
		for _, sub := range blockChildren(t) {
			out = append(out, blockTokens(sub, src)...)
		}
		out = append(out, mdToken{typ: "blockquote_close", tag: "blockquote"})
		return out
	case *gast.FencedCodeBlock:
		var content strings.Builder
		for i := 0; i < t.Lines().Len(); i++ {
			seg := t.Lines().At(i)
			content.Write(seg.Value(src))
		}
		info := ""
		if t.Info != nil {
			info = strings.TrimSpace(string(t.Info.Value(src)))
		}
		return []mdToken{{typ: "fence", tag: "code", content: content.String(), info: info}}
	case *gast.CodeBlock:
		var content strings.Builder
		for i := 0; i < t.Lines().Len(); i++ {
			seg := t.Lines().At(i)
			content.Write(seg.Value(src))
		}
		// markdown-it code_block strips trailing blank lines, ends with one \n.
		c := strings.TrimRight(content.String(), "\n") + "\n"
		return []mdToken{{typ: "code_block", tag: "code", content: c}}
	case *gast.ThematicBreak:
		return []mdToken{{typ: "hr", tag: "hr"}}
	case *east.Table:
		return tableTokens(t, src)
	}
	return nil
}

func tableTokens(t *east.Table, src []byte) []mdToken {
	out := []mdToken{{typ: "table_open", tag: "table"}}
	out = append(out, mdToken{typ: "thead_open", tag: "thead"})
	out = append(out, mdToken{typ: "tr_open", tag: "tr"})
	if t.FirstChild() != nil {
		if header, ok := t.FirstChild().(*east.TableHeader); ok {
			for c := header.FirstChild(); c != nil; c = c.NextSibling() {
				if cell, ok := c.(*east.TableCell); ok {
					out = append(out, mdToken{typ: "th_open", tag: "th"})
					out = append(out, mdToken{typ: "inline", children: inlineChildren(cell, src)})
					out = append(out, mdToken{typ: "th_close", tag: "th"})
				}
			}
		}
	}
	out = append(out, mdToken{typ: "tr_close", tag: "tr"})
	out = append(out, mdToken{typ: "thead_close", tag: "thead"})
	// body rows: markdown-it emits tbody only when body rows exist
	var rows []*east.TableRow
	for c := t.FirstChild(); c != nil; c = c.NextSibling() {
		if row, ok := c.(*east.TableRow); ok {
			rows = append(rows, row)
		}
	}
	if len(rows) > 0 {
		out = append(out, mdToken{typ: "tbody_open", tag: "tbody"})
		for _, row := range rows {
			out = append(out, mdToken{typ: "tr_open", tag: "tr"})
			for c := row.FirstChild(); c != nil; c = c.NextSibling() {
				if cell, ok := c.(*east.TableCell); ok {
					out = append(out, mdToken{typ: "td_open", tag: "td"})
					out = append(out, mdToken{typ: "inline", children: inlineChildren(cell, src)})
					out = append(out, mdToken{typ: "td_close", tag: "td"})
				}
			}
			out = append(out, mdToken{typ: "tr_close", tag: "tr"})
		}
		out = append(out, mdToken{typ: "tbody_close", tag: "tbody"})
	}
	out = append(out, mdToken{typ: "table_close", tag: "table"})
	return out
}

// inlineChildren converts goldmark inline nodes into markdown-it inline
// children tokens, merging adjacent literal text the way markdown-it's
// text_join rule does.
func inlineChildren(n gast.Node, src []byte) []mdToken {
	var out []mdToken
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			out = append(out, mdToken{typ: "text", content: buf.String()})
			buf.Reset()
		}
	}
	var walk func(m gast.Node)
	walk = func(m gast.Node) {
		switch t := m.(type) {
		case *gast.Text:
			if t.IsRaw() {
				// raw text (code span content): no escape/entity resolution
				buf.Write(t.Segment.Value(src))
			} else {
				// markdown-it splits backslash escapes and entity
				// references into separate text_special tokens (merged back
				// by text_join only AFTER the linkify rule runs), so we
				// emit them as their own tokens here.
				scanInlineSegments(string(t.Segment.Value(src)), func(special bool, content string) {
					if special {
						flush()
						out = append(out, mdToken{typ: "text_special", content: content})
					} else {
						buf.WriteString(content)
					}
				})
			}
			switch {
			case t.HardLineBreak():
				flush()
				out = append(out, mdToken{typ: "hardbreak", tag: "br"})
			case t.SoftLineBreak():
				flush()
				out = append(out, mdToken{typ: "softbreak", tag: "br"})
			}
		case *gast.String:
			buf.WriteString(string(t.Value))
		case *gast.CodeSpan:
			var raw strings.Builder
			for c := t.FirstChild(); c != nil; c = c.NextSibling() {
				if tt, ok := c.(*gast.Text); ok {
					raw.Write(tt.Segment.Value(src))
				}
			}
			flush()
			out = append(out, mdToken{typ: "code_inline", tag: "code", content: normalizeCodeSpan(raw.String())})
		case *gast.Emphasis:
			open, close_ := "em_open", "em_close"
			tag := "em"
			if t.Level == 2 {
				open, close_, tag = "strong_open", "strong_close", "strong"
			}
			flush()
			out = append(out, mdToken{typ: open, tag: tag})
			for c := t.FirstChild(); c != nil; c = c.NextSibling() {
				walk(c)
			}
			flush()
			out = append(out, mdToken{typ: close_, tag: tag})
		case *east.Strikethrough:
			flush()
			out = append(out, mdToken{typ: "s_open", tag: "s"})
			for c := t.FirstChild(); c != nil; c = c.NextSibling() {
				walk(c)
			}
			flush()
			out = append(out, mdToken{typ: "s_close", tag: "s"})
		case *gast.Link:
			flush()
			out = append(out, mdToken{typ: "link_open", tag: "a", href: string(t.Destination)})
			for c := t.FirstChild(); c != nil; c = c.NextSibling() {
				walk(c)
			}
			flush()
			out = append(out, mdToken{typ: "link_close", tag: "a"})
		case *gast.AutoLink:
			label := string(t.Label(src))
			href := string(t.URL(src))
			if t.AutoLinkType == gast.AutoLinkEmail {
				href = "mailto:" + label
			}
			flush()
			out = append(out, mdToken{typ: "link_open", tag: "a", href: href})
			out = append(out, mdToken{typ: "text", content: label})
			out = append(out, mdToken{typ: "link_close", tag: "a"})
		case *gast.Image:
			// Upstream's processToken ignores image tokens AND their children.
		case *gast.RawHTML:
			// html:false — the markup is literal text.
			for i := 0; i < t.Segments.Len(); i++ {
				seg := t.Segments.At(i)
				buf.Write(seg.Value(src))
			}
		default:
			for c := m.FirstChild(); c != nil; c = c.NextSibling() {
				walk(c)
			}
		}
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		walk(c)
	}
	flush()
	out = linkifyFuzzyTokens(out)
	return textJoinTokens(out)
}

// normalizeCodeSpan applies markdown-it's backtick rule normalization:
// newlines become spaces, then one leading+trailing space is stripped when
// the content is at least 3 chars and starts/ends with a space (markdown-it
// regex /^ (.+) $/ — note this strips even all-space content, a deliberate
// markdown-it deviation from CommonMark). goldmark already applies the
// CommonMark strip (which skips all-space content), so we only need the
// newline replacement plus the all-space re-strip.
func normalizeCodeSpan(raw string) string {
	s := strings.ReplaceAll(raw, "\n", " ")
	if len(s) >= 3 && strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") && strings.Trim(s, " ") == "" {
		s = s[1 : len(s)-1]
	}
	return s
}

// scanInlineSegments resolves CommonMark backslash escapes and HTML5 entity
// references the way markdown-it's inline rules do: a backslash before
// ASCII punctuation yields a text_special token with the literal
// punctuation; &...; entities (named or numeric, semicolon required) yield
// a text_special token with the decoded text. Plain runs are emitted as
// (false, text).
func scanInlineSegments(raw string, emit func(special bool, content string)) {
	i := 0
	start := 0
	for i < len(raw) {
		c := raw[i]
		if c == '\\' && i+1 < len(raw) && isASCIIPunct(raw[i+1]) {
			if start < i {
				emit(false, raw[start:i])
			}
			emit(true, raw[i+1:i+2])
			i += 2
			start = i
			continue
		}
		if c == '&' {
			if decoded, n := matchEntity(raw[i:]); n > 0 {
				if start < i {
					emit(false, raw[start:i])
				}
				emit(true, decoded)
				i += n
				start = i
				continue
			}
		}
		i++
	}
	if start < len(raw) {
		emit(false, raw[start:])
	}
}

func isASCIIPunct(c byte) bool {
	return (c >= '!' && c <= '/') || (c >= ':' && c <= '@') || (c >= '[' && c <= '`') || (c >= '{' && c <= '~')
}

// matchEntity matches a semicolon-terminated entity reference at the start
// of s (which begins with '&'), following markdown-it's entity rule regex:
// /^&(#x[a-f0-9]{1,6}|#[0-9]{1,7}|[a-z][a-z0-9]{1,31});/i. Returns the
// decoded text and the matched length, or ("", 0).
func matchEntity(s string) (string, int) {
	if len(s) < 3 || s[0] != '&' {
		return "", 0
	}
	i := 1
	if s[i] == '#' {
		i++
		hex := i < len(s) && (s[i] == 'x' || s[i] == 'X')
		if hex {
			i++
		}
		start := i
		max := 7
		if hex {
			max = 6
		}
		for i < len(s) && i-start < max {
			c := s[i]
			if hex {
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
					break
				}
			} else if c < '0' || c > '9' {
				break
			}
			i++
		}
		if i == start || i >= len(s) || s[i] != ';' {
			return "", 0
		}
		ent := s[:i+1]
		decoded := unescapeHTML5(ent)
		if decoded == ent {
			return "", 0
		}
		return decoded, i + 1
	}
	// named: [a-z][a-z0-9]{1,31};
	if !((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')) {
		return "", 0
	}
	start := i
	i++
	for i < len(s) && i-start <= 31 {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	if i-start < 1 || i >= len(s) || s[i] != ';' {
		return "", 0
	}
	ent := s[:i+1]
	decoded := unescapeHTML5(ent)
	if decoded == ent {
		return "", 0
	}
	return decoded, i + 1
}

// unescapeHTML5 decodes one semicolon-terminated entity; unknown entities
// come back unchanged (Go's html package implements the HTML5 named/numeric
// entity rules, the same table markdown-it's entities dependency uses).
func unescapeHTML5(s string) string { return html.UnescapeString(s) }
