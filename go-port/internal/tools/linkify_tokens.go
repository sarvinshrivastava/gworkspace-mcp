package tools

// linkify_tokens.go — ports markdown-it's rules_core/linkify.mjs (fuzzy
// link replacement over inline children) and rules_core/text_join.mjs, as
// configured by the upstream markdown-transformer ({html: false,
// linkify: true}). With html:false no html_inline tokens exist, so the
// html-link-level tracking of the upstream rule is inert and omitted.
// markdown links are skipped via link_open/link_close depth.

import "strings"

// linkifyFuzzyTokens splits text tokens containing fuzzy bare-domain links
// into text/link_open/text/link_close sequences, mirroring the upstream
// core rule. urlText uses normalizeLinkText on the http://-prefixed URL
// with the prefix stripped back off; href uses normalizeLink.
func linkifyFuzzyTokens(tokens []mdToken) []mdToken {
	out := make([]mdToken, 0, len(tokens)+4)
	linkDepth := 0
	for i, tok := range tokens {
		switch tok.typ {
		case "link_open":
			linkDepth++
			out = append(out, tok)
			continue
		case "link_close":
			if linkDepth > 0 {
				linkDepth--
			}
			out = append(out, tok)
			continue
		}
		if tok.typ != "text" || linkDepth > 0 || tok.content == "" {
			out = append(out, tok)
			continue
		}
		matches := scanFuzzyLinks(tok.content)
		if len(matches) == 0 {
			out = append(out, tok)
			continue
		}
		// Forbid escape sequence at the start of the string: if the first
		// link starts at index 0 and the previous token is a text_special,
		// drop that first link (markdown-it's http\://example.com guard).
		if matches[0].start == 0 && i > 0 && tokens[i-1].typ == "text_special" {
			matches = matches[1:]
		}
		if len(matches) == 0 {
			out = append(out, tok)
			continue
		}
		text := tok.content
		lastPos := 0
		for _, m := range matches {
			raw := text[m.start:m.end]
			fullURL := normalizeMDLink("http://" + raw)
			urlText := normalizeMDLinkText("http://" + raw)
			urlText = strings.TrimPrefix(urlText, "http://")
			if m.start > lastPos {
				out = append(out, mdToken{typ: "text", content: text[lastPos:m.start]})
			}
			out = append(out, mdToken{typ: "link_open", tag: "a", href: fullURL})
			out = append(out, mdToken{typ: "text", content: urlText})
			out = append(out, mdToken{typ: "link_close", tag: "a"})
			lastPos = m.end
		}
		if lastPos < len(text) {
			out = append(out, mdToken{typ: "text", content: text[lastPos:]})
		}
	}
	return out
}

// textJoinTokens ports rules_core/text_join.mjs: text_special becomes text,
// then adjacent text tokens collapse.
func textJoinTokens(tokens []mdToken) []mdToken {
	out := make([]mdToken, 0, len(tokens))
	for _, tok := range tokens {
		if tok.typ == "text_special" {
			tok.typ = "text"
		}
		if tok.typ == "text" && len(out) > 0 && out[len(out)-1].typ == "text" {
			out[len(out)-1].content += tok.content
			continue
		}
		out = append(out, tok)
	}
	return out
}
