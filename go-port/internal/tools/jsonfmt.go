package tools

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// jsQuote re-encodes a decoded JSON string exactly the way JSON.stringify
// does: escape only ", \, and control characters (\b \t \n \f \r, otherwise
// \u00XX); every other character is emitted literally (no HTML escaping).
func jsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[byte(r)>>4])
				b.WriteByte(hex[byte(r)&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsStringifyRaw re-emits raw JSON with JSON.stringify(value, null, 2)
// formatting: 2-space indent, insertion (document) order preserved, no HTML
// escaping, empty objects "{}", empty arrays "[]". Numbers are canonicalized
// to JS Number::toString; true/false/null copy verbatim; strings are decoded
// and re-encoded with JS escaping rules.
// baseIndent is the starting indent depth (0 = top level).
func jsStringifyRaw(raw []byte, baseIndent int) string {
	p := &jsonPrinter{s: strings.TrimSpace(string(raw)), indent: baseIndent}
	p.writeValue()
	return p.b.String()
}

type jsonPrinter struct {
	s      string
	pos    int
	indent int
	b      strings.Builder
}

func (p *jsonPrinter) pad() {
	for i := 0; i < p.indent; i++ {
		p.b.WriteString("  ")
	}
}

func (p *jsonPrinter) writeValue() {
	p.skipWS()
	if p.pos >= len(p.s) {
		return
	}
	switch p.s[p.pos] {
	case '{':
		p.writeObject()
	case '[':
		p.writeArray()
	case '"':
		p.writeString()
	default:
		// true, false, null copy verbatim; numbers canonicalize to JS
		// Number::toString (JSON.stringify drops noncanonical lexemes:
		// 1e2 -> "100", 11.50 -> "11.5", -0 -> "0").
		start := p.pos
		for p.pos < len(p.s) && !strings.ContainsRune(",}] \t\n\r", rune(p.s[p.pos])) {
			p.pos++
		}
		lit := p.s[start:p.pos]
		if c := lit[0]; c == '-' || (c >= '0' && c <= '9') {
			lit = jsNumberCanon(lit)
		}
		p.b.WriteString(lit)
	}
}

func (p *jsonPrinter) skipWS() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t' || p.s[p.pos] == '\n' || p.s[p.pos] == '\r') {
		p.pos++
	}
}

func (p *jsonPrinter) writeString() {
	start := p.pos
	p.pos++
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == '\\' {
			p.pos += 2
			continue
		}
		if c == '"' {
			p.pos++
			break
		}
		p.pos++
	}
	var decoded string
	if err := json.Unmarshal([]byte(p.s[start:p.pos]), &decoded); err != nil {
		// Should not happen on Google-produced JSON; copy verbatim as fallback.
		p.b.WriteString(p.s[start:p.pos])
		return
	}
	p.b.WriteString(jsQuote(decoded))
}

func (p *jsonPrinter) writeObject() {
	p.pos++ // consume '{'
	p.skipWS()
	if p.pos < len(p.s) && p.s[p.pos] == '}' {
		p.pos++
		p.b.WriteString("{}")
		return
	}
	p.b.WriteString("{\n")
	p.indent++
	first := true
	for {
		p.skipWS()
		if !first {
			p.b.WriteString(",\n")
		}
		first = false
		p.pad()
		p.writeString() // key
		p.skipWS()
		p.pos++ // consume ':'
		p.b.WriteString(": ")
		p.writeValue()
		p.skipWS()
		if p.pos < len(p.s) && p.s[p.pos] == ',' {
			p.pos++
			continue
		}
		break
	}
	p.indent--
	p.b.WriteByte('\n')
	p.pad()
	p.skipWS()
	p.pos++ // consume '}'
	p.b.WriteByte('}')
}

func (p *jsonPrinter) writeArray() {
	p.pos++ // consume '['
	p.skipWS()
	if p.pos < len(p.s) && p.s[p.pos] == ']' {
		p.pos++
		p.b.WriteString("[]")
		return
	}
	p.b.WriteString("[\n")
	p.indent++
	first := true
	for {
		p.skipWS()
		if !first {
			p.b.WriteString(",\n")
		}
		first = false
		p.pad()
		p.writeValue()
		p.skipWS()
		if p.pos < len(p.s) && p.s[p.pos] == ',' {
			p.pos++
			continue
		}
		break
	}
	p.indent--
	p.b.WriteByte('\n')
	p.pad()
	p.skipWS()
	p.pos++ // consume ']'
	p.b.WriteByte(']')
}

// indentUnderKey renders { "<key>": <raw> } with JSON.stringify indent-2
// formatting, matching JSON.stringify({key: value}, null, 2).
func indentUnderKey(key string, raw []byte) string {
	var b strings.Builder
	b.WriteString("{\n  ")
	b.WriteString(jsQuote(key))
	b.WriteString(": ")
	b.WriteString(jsStringifyRaw(raw, 1))
	b.WriteString("\n}")
	return b.String()
}

// jsCanonicalNumber renders f exactly as ECMAScript Number::toString (what
// JSON.stringify uses): shortest round-trip decimal, plain notation in
// [1e-6, 1e21), exponential outside with an unpadded, always-signed exponent.
func jsCanonicalNumber(f float64) string {
	if f == 0 {
		return "0" // JSON.stringify(-0) is "0"
	}
	mant := strconv.FormatFloat(f, 'e', -1, 64) // "-1.5e-07"
	ei := strings.IndexByte(mant, 'e')
	exp, _ := strconv.Atoi(mant[ei+1:])
	mantissa := mant[:ei]
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		digits := strings.Replace(mantissa, ".", "", 1)
		point := exp + 1
		switch {
		case point >= len(digits):
			return sign + digits + strings.Repeat("0", point-len(digits))
		case point > 0:
			return sign + digits[:point] + "." + digits[point:]
		default:
			return sign + "0." + strings.Repeat("0", -point) + digits
		}
	}
	es := strconv.Itoa(exp)
	if exp >= 0 {
		es = "+" + es
	}
	return sign + mantissa + "e" + es
}

// jsNumberCanon canonicalizes a raw JSON number lexeme the way JSON.stringify
// re-emits it. Unparseable input (unreachable for valid JSON) passes through.
func jsNumberCanon(lit string) string {
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return lit
	}
	return jsCanonicalNumber(f)
}
