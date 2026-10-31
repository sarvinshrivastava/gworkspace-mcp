package tools

// mdurl.go — port of the mdurl parse/format/encode/decode pipeline plus
// punycode.js toASCII/toUnicode, scoped to what markdown-it's normalizeLink
// and normalizeLinkText do for linkify-produced URLs
// (markdown-it/lib/index.mjs). normalizeLink: parse(url, true) -> punycode
// toASCII hostname -> format -> encode. normalizeLinkText: same with
// toUnicode and decode with '%' added to the keep-encoded set.
// JS string semantics (UTF-16) are reproduced where observable: the >127
// hostname check counts code units, and encode's surrogate handling maps to
// UTF-8 byte emission.

import (
	"strings"
	"unicode/utf8"
)

type mdURLParts struct {
	protocol  string
	slashes   bool
	auth      string
	port      string
	hostname  string
	hash      string
	search    string
	pathname  string
	hasPath   bool
	hasHostnm bool
}

var mdNonHostChars = []byte{'%', '/', '?', ';', '#', '\'', '{', '}', '|', '\\', '^', '`', '<', '>', '"', ' ', '\r', '\n', '\t'}
var mdHostEndingChars = []byte{'/', '?', '#'}

func indexOfByte(s string, c byte) int { return strings.IndexByte(s, c) }

func firstIndexOfAny(s string, chars []byte) int {
	best := -1
	for _, c := range chars {
		if i := indexOfByte(s, c); i != -1 && (best == -1 || i < best) {
			best = i
		}
	}
	return best
}

// hostnamePartPattern: ^[+a-z0-9A-Z_-]{0,63}$
func hostnamePartOK(part string) bool {
	if len(part) > 63 {
		return false
	}
	for i := 0; i < len(part); i++ {
		c := part[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// hostnamePartStart: ^([+a-z0-9A-Z_-]{0,63})(.*)$ — greedy first run.
func hostnamePartSplit(part string) (string, string) {
	i := 0
	for i < len(part) && i < 63 {
		c := part[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '_' || c == '-') {
			break
		}
		i++
	}
	return part[:i], part[i:]
}

// utf16LenLocal counts JS string length (UTF-16 code units).
func utf16LenLocal(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func (u *mdURLParts) parseHost(host string) {
	// portPattern = /:[0-9]*$/
	i := len(host)
	for i > 0 && host[i-1] >= '0' && host[i-1] <= '9' {
		i--
	}
	if i > 0 && host[i-1] == ':' {
		port := host[i-1:]
		if port != ":" {
			u.port = port[1:]
		}
		host = host[:len(host)-len(port)]
	}
	if host != "" {
		u.hostname = host
	}
}

// mdurlParse ports Url.prototype.parse for slashesDenoteHost=true.
func mdurlParse(url string) *mdURLParts {
	u := &mdURLParts{}
	rest := jsTrim(url)

	// protocolPattern = /^([a-z0-9.+-]+:)/i
	if i := mdProtocolEnd(rest); i > 0 {
		u.protocol = rest[:i]
		rest = rest[i:]
	}
	// slashes (proto is never hostless here: http: is slashed)
	if len(rest) >= 2 && rest[:2] == "//" {
		rest = rest[2:]
		u.slashes = true
	}

	if u.slashes || (u.protocol != "" && !mdSlashedProtocol(u.protocol)) {
		// hostname present
		hostEnd := firstIndexOfAny(rest, mdHostEndingChars)
		var atSign int
		if hostEnd == -1 {
			atSign = strings.LastIndex(rest, "@")
		} else {
			atSign = strings.LastIndex(rest[:hostEnd], "@")
		}
		if atSign != -1 {
			u.auth = rest[:atSign]
			rest = rest[atSign+1:]
		}
		hostEnd = firstIndexOfAny(rest, mdNonHostChars)
		if hostEnd == -1 {
			hostEnd = len(rest)
		}
		if hostEnd > 0 && rest[hostEnd-1] == ':' {
			hostEnd--
		}
		host := rest[:hostEnd]
		rest = rest[hostEnd:]
		u.parseHost(host)
		u.hasHostnm = true // hostname = this.hostname || ''

		ipv6 := len(u.hostname) >= 2 && u.hostname[0] == '[' && u.hostname[len(u.hostname)-1] == ']'
		if !ipv6 {
			hostparts := strings.Split(u.hostname, ".")
			for i, part := range hostparts {
				if part == "" {
					continue
				}
				if !hostnamePartOK(part) {
					// replace >127 chars with 'x' per UTF-16 code unit, retest
					var nb strings.Builder
					for _, r := range part {
						if r > 127 {
							if r > 0xFFFF {
								nb.WriteString("xx")
							} else {
								nb.WriteString("x")
							}
						} else {
							nb.WriteRune(r)
						}
					}
					if !hostnamePartOK(nb.String()) {
						validParts := append([]string{}, hostparts[:i]...)
						notHost := append([]string{}, hostparts[i+1:]...)
						bit1, bit2 := hostnamePartSplit(part)
						validParts = append(validParts, bit1)
						notHost = append([]string{bit2}, notHost...)
						if len(notHost) > 0 {
							rest = strings.Join(notHost, ".") + rest
						}
						u.hostname = strings.Join(validParts, ".")
						break
					}
				}
			}
		}
		if utf16LenLocal(u.hostname) > 255 {
			u.hostname = ""
		}
		if ipv6 {
			u.hostname = u.hostname[1 : len(u.hostname)-1]
		}
	}

	if h := indexOfByte(rest, '#'); h != -1 {
		u.hash = rest[h:]
		rest = rest[:h]
	}
	if q := indexOfByte(rest, '?'); q != -1 {
		u.search = rest[q:]
		rest = rest[:q]
	}
	if rest != "" {
		u.pathname = rest
		u.hasPath = true
	}
	if mdSlashedProtocol(strings.ToLower(u.protocol)) && u.hasHostnm && u.hostname != "" && !u.hasPath {
		u.pathname = ""
	}
	return u
}

func mdProtocolEnd(s string) int {
	// ^([a-z0-9.+-]+:)/i — first char must be in the class too (the regex
	// allows digits/dots first, faithfully: [a-z0-9.+-]+:)
	i := 0
	for i < len(s) {
		c := s[i]
		if c == ':' {
			if i > 0 {
				return i + 1
			}
			return 0
		}
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '+' || c == '-') {
			return 0
		}
		i++
	}
	return 0
}

func mdSlashedProtocol(lowerProto string) bool {
	switch lowerProto {
	case "http", "https", "ftp", "gopher", "file", "http:", "https:", "ftp:", "gopher:", "file:":
		return true
	}
	return false
}

// mdurlFormat ports format.mjs.
func mdurlFormat(u *mdURLParts) string {
	var b strings.Builder
	b.WriteString(u.protocol)
	if u.slashes {
		b.WriteString("//")
	}
	if u.auth != "" {
		b.WriteString(u.auth)
		b.WriteString("@")
	}
	if u.hostname != "" && strings.Contains(u.hostname, ":") {
		b.WriteString("[" + u.hostname + "]")
	} else if u.hasHostnm {
		b.WriteString(u.hostname)
	}
	if u.port != "" {
		b.WriteString(":" + u.port)
	}
	if u.hasPath {
		b.WriteString(u.pathname)
	}
	b.WriteString(u.search)
	b.WriteString(u.hash)
	return b.String()
}

const mdEncodeDefaultChars = ";/?:@&=+$,-_.!~*'()#"
const mdDecodeDefaultChars = ";/?:@&=+$,#"

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// mdurlEncode ports encode.mjs with keepEscaped=true.
func mdurlEncode(s, exclude string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c < 128 {
			if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.IndexByte(exclude, c) != -1 {
				b.WriteByte(c)
				i++
				continue
			}
			if c == '%' && i+2 < len(s) && isHexByte(s[i+1]) && isHexByte(s[i+2]) {
				b.WriteString(s[i : i+3])
				i += 3
				continue
			}
			b.WriteString("%")
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// invalid byte → JS lone-surrogate path emits %EF%BF%BD
			b.WriteString("%EF%BF%BD")
			i++
			continue
		}
		var buf [4]byte
		n := utf8.EncodeRune(buf[:], r)
		const hex = "0123456789ABCDEF"
		for _, by := range buf[:n] {
			b.WriteString("%")
			b.WriteByte(hex[by>>4])
			b.WriteByte(hex[by&15])
		}
		i += size
	}
	return b.String()
}

// mdurlDecode ports decode.mjs: decodes consecutive percent-triplet runs,
// reassembling UTF-8 sequences; encodings of chars in exclude stay encoded.
func mdurlDecode(s, exclude string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) && isHexByte(s[i+1]) && isHexByte(s[i+2]) {
			// collect the full run of triplets
			j := i
			for j+2 < len(s) && s[j] == '%' && isHexByte(s[j+1]) && isHexByte(s[j+2]) {
				j += 3
			}
			b.WriteString(mdDecodeRun(s[i:j], exclude))
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func mdDecodeRun(seq, exclude string) string {
	b1 := func(i int) int {
		var v int
		for _, c := range []byte{seq[i+1], seq[i+2]} {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= int(c - '0')
			case c >= 'a' && c <= 'f':
				v |= int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v |= int(c-'A') + 10
			}
		}
		return v
	}
	keepPct := func(b int) string {
		// excluded chars decode to their %XX form (kept encoded)
		ch := byte(b)
		if strings.IndexByte(exclude, ch) != -1 {
			const hex = "0123456789ABCDEF"
			return string([]byte{'%', hex[byte(b)>>4], hex[byte(b)&15]})
		}
		return string(ch)
	}
	var out strings.Builder
	for i := 0; i < len(seq); i += 3 {
		b1v := b1(i)
		if b1v < 0x80 {
			out.WriteString(keepPct(b1v))
			continue
		}
		if b1v&0xE0 == 0xC0 && i+3 < len(seq) {
			b2 := b1(i + 3)
			if b2&0xC0 == 0x80 {
				chr := (b1v<<6)&0x7C0 | b2&0x3F
				if chr < 0x80 {
					out.WriteString("��")
				} else {
					out.WriteRune(rune(chr))
				}
				i += 3
				continue
			}
		}
		if b1v&0xF0 == 0xE0 && i+6 < len(seq) {
			b2, b3 := b1(i+3), b1(i+6)
			if b2&0xC0 == 0x80 && b3&0xC0 == 0x80 {
				chr := (b1v<<12)&0xF000 | (b2<<6)&0xFC0 | b3&0x3F
				if chr < 0x800 || (chr >= 0xD800 && chr <= 0xDFFF) {
					out.WriteString("���")
				} else {
					out.WriteRune(rune(chr))
				}
				i += 6
				continue
			}
		}
		if b1v&0xF8 == 0xF0 && i+9 < len(seq) {
			b2, b3, b4 := b1(i+3), b1(i+6), b1(i+9)
			if b2&0xC0 == 0x80 && b3&0xC0 == 0x80 && b4&0xC0 == 0x80 {
				chr := (b1v<<18)&0x1C0000 | (b2<<12)&0x3F000 | (b3<<6)&0xFC0 | b4&0x3F
				if chr < 0x10000 || chr > 0x10FFFF {
					out.WriteString("����")
				} else {
					out.WriteRune(rune(chr))
				}
				i += 9
				continue
			}
		}
		out.WriteString("�")
	}
	return out.String()
}

// --- punycode (RFC 3492, punycode.js semantics) ---

const (
	pcBase        = 36
	pcTMin        = 1
	pcTMax        = 26
	pcSkew        = 38
	pcDamp        = 700
	pcInitialBias = 72
	pcInitialN    = 128
)

func pcDigitToBasic(d int) byte {
	if d < 26 {
		return byte('a' + d)
	}
	return byte('0' + d - 26)
}

func pcBasicToDigit(c byte) int {
	switch {
	case c >= 'a' && c <= 'z':
		return int(c - 'a')
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= '0' && c <= '9':
		return int(c-'0') + 26
	}
	return -1
}

func pcAdapt(delta, numPoints int, firstTime bool) int {
	if firstTime {
		delta /= pcDamp
	} else {
		delta /= 2
	}
	delta += delta / numPoints
	k := 0
	for delta > ((pcBase-pcTMin)*pcTMax)/2 {
		delta /= pcBase - pcTMin
		k += pcBase
	}
	return k + (pcBase-pcTMin+1)*delta/(delta+pcSkew)
}

// punycodeEncodeLabel ports punycode.js encode() for one label.
func punycodeEncodeLabel(input string) string {
	runes := []rune(input)
	n := pcInitialN
	delta := 0
	bias := pcInitialBias
	var out strings.Builder
	b := 0
	for _, r := range runes {
		if r < 0x80 {
			out.WriteRune(r)
			b++
		}
	}
	h := b
	if b > 0 {
		out.WriteByte('-')
	}
	for h < len(runes) {
		m := int32(0x7FFFFFFF)
		for _, r := range runes {
			if int32(r) >= int32(n) && int32(r) < m {
				m = int32(r)
			}
		}
		delta += (int(m) - n) * (h + 1)
		n = int(m)
		for _, r := range runes {
			if int32(r) < int32(n) {
				delta++
			}
			if int32(r) == int32(n) {
				q := delta
				for k := pcBase; ; k += pcBase {
					t := 0
					if k <= bias {
						t = pcTMin
					} else if k >= bias+pcTMax {
						t = pcTMax
					} else {
						t = k - bias
					}
					if q < t {
						break
					}
					out.WriteByte(pcDigitToBasic(t + (q-t)%(pcBase-t)))
					q = (q - t) / (pcBase - t)
				}
				out.WriteByte(pcDigitToBasic(q))
				bias = pcAdapt(delta, h+1, h == b)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return out.String()
}

// punycodeDecodeLabel ports punycode.js decode() for one label (without the
// xn-- prefix; caller lowercases as punycode.js does).
func punycodeDecodeLabel(input string) string {
	var out []rune
	// basic code points before the last delimiter
	if i := strings.LastIndex(input, "-"); i >= 0 {
		for _, c := range input[:i] {
			out = append(out, rune(c))
		}
		input = input[i+1:]
	}
	n := pcInitialN
	bias := pcInitialBias
	idx := 0
	for len(input) > 0 {
		oldi := idx
		w := 1
		for k := pcBase; ; k += pcBase {
			d := pcBasicToDigit(input[0])
			input = input[1:]
			idx += d * w
			t := 0
			if k <= bias {
				t = pcTMin
			} else if k >= bias+pcTMax {
				t = pcTMax
			} else {
				t = k - bias
			}
			if d < t {
				break
			}
			w *= pcBase - t
		}
		outLen := len(out) + 1
		bias = pcAdapt(idx-oldi, outLen, oldi == 0)
		n += idx / outLen
		idx %= outLen
		// insert n at idx
		out = append(out, 0)
		copy(out[idx+1:], out[idx:])
		out[idx] = rune(n)
	}
	return string(out)
}

var pcDotReplacer = strings.NewReplacer("。", ".", "．", ".", "｡", ".")

// punycodeToASCII maps domain labels; non-ASCII labels get xn-- encoded.
func punycodeToASCII(domain string) string {
	mapped := pcDotReplacer.Replace(domain)
	parts := strings.Split(mapped, ".")
	for i, p := range parts {
		if !isAllASCII(p) {
			parts[i] = "xn--" + punycodeEncodeLabel(p)
		}
	}
	return strings.Join(parts, ".")
}

// punycodeToUnicode decodes xn-- labels.
func punycodeToUnicode(domain string) string {
	mapped := pcDotReplacer.Replace(domain)
	parts := strings.Split(mapped, ".")
	for i, p := range parts {
		if len(p) >= 4 && p[:4] == "xn--" {
			parts[i] = punycodeDecodeLabel(strings.ToLower(p[4:]))
		}
	}
	return strings.Join(parts, ".")
}

func isAllASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// --- markdown-it normalizeLink / normalizeLinkText ---

var mdRecodeHostnameFor = map[string]bool{"http:": true, "https:": true, "mailto:": true}

// normalizeMDLink ports markdown-it's normalizeLink.
func normalizeMDLink(url string) string {
	parsed := mdurlParse(url)
	if parsed.hasHostnm && parsed.hostname != "" {
		if parsed.protocol == "" || mdRecodeHostnameFor[strings.ToLower(parsed.protocol)] {
			parsed.hostname = punycodeToASCII(parsed.hostname)
		}
	}
	return mdurlEncode(mdurlFormat(parsed), mdEncodeDefaultChars)
}

// normalizeMDLinkText ports markdown-it's normalizeLinkText.
func normalizeMDLinkText(url string) string {
	parsed := mdurlParse(url)
	if parsed.hasHostnm && parsed.hostname != "" {
		if parsed.protocol == "" || mdRecodeHostnameFor[strings.ToLower(parsed.protocol)] {
			parsed.hostname = punycodeToUnicode(parsed.hostname)
		}
	}
	return mdurlDecode(mdurlFormat(parsed), mdDecodeDefaultChars+"%")
}
