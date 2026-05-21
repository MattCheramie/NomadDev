package docfetch

import (
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipElements are dropped together with their whole subtree: they carry no
// readable documentation text, only chrome, styling or scripting.
var skipElements = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Head:     true,
	atom.Noscript: true,
	atom.Template: true,
	atom.Svg:      true,
	atom.Iframe:   true,
	atom.Form:     true,
	atom.Nav:      true,
	atom.Footer:   true,
	atom.Aside:    true,
	atom.Button:   true,
}

// htmlToMarkdown parses an HTML document and returns its readable content as
// markdown text — every tag, inline style and script stripped. Malformed
// input is tolerated: html.Parse always yields a well-formed tree.
func htmlToMarkdown(r io.Reader) (string, error) {
	root, err := html.Parse(r)
	if err != nil {
		return "", err
	}
	m := &mdBuf{}
	render(m, root, false, 0)
	return cleanup(m.String()), nil
}

// mdBuf accumulates markdown output while tracking the last byte written so
// inline text can be trimmed at line starts without rescanning the buffer.
type mdBuf struct {
	sb   strings.Builder
	last byte
}

func (m *mdBuf) String() string { return m.sb.String() }

// writeRaw appends s verbatim (used for markdown syntax and verbatim <pre>).
func (m *mdBuf) writeRaw(s string) {
	if s == "" {
		return
	}
	m.sb.WriteString(s)
	m.last = s[len(s)-1]
}

// writeText appends already-whitespace-collapsed inline text, dropping a
// leading space when the buffer is at the start of a line.
func (m *mdBuf) writeText(s string) {
	if s == "" {
		return
	}
	if m.last == 0 || m.last == '\n' {
		s = strings.TrimLeft(s, " ")
	}
	m.writeRaw(s)
}

// render walks one node. inPre preserves whitespace verbatim; depth is the
// current list-nesting level.
func render(m *mdBuf, n *html.Node, inPre bool, depth int) {
	switch n.Type {
	case html.CommentNode, html.DoctypeNode:
		return
	case html.TextNode:
		if inPre {
			m.writeRaw(n.Data)
		} else {
			m.writeText(collapseWS(n.Data))
		}
		return
	case html.DocumentNode:
		renderChildren(m, n, inPre, depth)
		return
	case html.ElementNode:
		// handled below
	default:
		return
	}

	if skipElements[n.DataAtom] {
		return
	}

	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		inner := strings.TrimSpace(childrenString(n, false, depth))
		if inner != "" {
			m.writeRaw("\n\n" + strings.Repeat("#", headingLevel(n.DataAtom)) + " " + oneLine(inner) + "\n\n")
		}

	case atom.P, atom.Div, atom.Section, atom.Article, atom.Main, atom.Header,
		atom.Dl, atom.Dt, atom.Dd, atom.Figure, atom.Figcaption, atom.Address:
		m.writeRaw("\n\n")
		renderChildren(m, n, inPre, depth)
		m.writeRaw("\n\n")

	case atom.Br:
		m.writeRaw("\n")

	case atom.Hr:
		m.writeRaw("\n\n---\n\n")

	case atom.A:
		inner := strings.TrimSpace(childrenString(n, inPre, depth))
		href := strings.TrimSpace(getAttr(n, "href"))
		switch {
		case inner == "":
			// nothing to render
		case href == "" || strings.HasPrefix(href, "#") ||
			strings.HasPrefix(strings.ToLower(href), "javascript:"):
			m.writeText(inner)
		default:
			m.writeRaw("[" + oneLine(inner) + "](" + href + ")")
		}

	case atom.Ul, atom.Ol:
		m.writeRaw("\n\n")
		renderChildren(m, n, inPre, depth+1)
		m.writeRaw("\n\n")

	case atom.Li:
		inner := strings.TrimSpace(childrenString(n, inPre, depth))
		if inner != "" {
			indent := ""
			if depth > 1 {
				indent = strings.Repeat("  ", depth-1)
			}
			m.writeRaw("\n" + indent + "- " + inner + "\n")
		}

	case atom.Pre:
		code := strings.Trim(childrenString(n, true, depth), "\n")
		if code != "" {
			m.writeRaw("\n\n```\n" + code + "\n```\n\n")
		}

	case atom.Code, atom.Kbd, atom.Samp:
		if inPre {
			renderChildren(m, n, inPre, depth)
		} else {
			inner := strings.TrimSpace(oneLine(childrenString(n, false, depth)))
			if inner != "" {
				m.writeRaw("`" + inner + "`")
			}
		}

	case atom.Blockquote:
		inner := strings.TrimSpace(childrenString(n, inPre, depth))
		if inner != "" {
			var b strings.Builder
			for _, line := range strings.Split(inner, "\n") {
				b.WriteString("> ")
				b.WriteString(line)
				b.WriteString("\n")
			}
			m.writeRaw("\n\n" + b.String() + "\n")
		}

	case atom.Strong, atom.B:
		if inner := strings.TrimSpace(childrenString(n, inPre, depth)); inner != "" {
			m.writeRaw("**" + oneLine(inner) + "**")
		}

	case atom.Em, atom.I:
		if inner := strings.TrimSpace(childrenString(n, inPre, depth)); inner != "" {
			m.writeRaw("*" + oneLine(inner) + "*")
		}

	case atom.Table:
		m.writeRaw("\n\n")
		renderChildren(m, n, inPre, depth)
		m.writeRaw("\n\n")

	case atom.Tr:
		var cells []string
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.DataAtom == atom.Td || c.DataAtom == atom.Th) {
				cells = append(cells, oneLine(strings.TrimSpace(childrenString(c, inPre, depth))))
			}
		}
		if len(cells) > 0 {
			m.writeRaw("\n| " + strings.Join(cells, " | ") + " |\n")
		}

	default:
		// body, html, span, thead/tbody, td/th, etc.: no markdown of their
		// own, just descend into the children.
		renderChildren(m, n, inPre, depth)
	}
}

func renderChildren(m *mdBuf, n *html.Node, inPre bool, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		render(m, c, inPre, depth)
	}
}

// childrenString renders n's children into a fresh buffer — used for inline
// elements whose text must be wrapped in markdown syntax.
func childrenString(n *html.Node, inPre bool, depth int) string {
	tmp := &mdBuf{}
	renderChildren(tmp, n, inPre, depth)
	return tmp.String()
}

func headingLevel(a atom.Atom) int {
	switch a {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	default:
		return 6
	}
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// collapseWS collapses every run of HTML whitespace in a text node to a single
// space, keeping at most one leading and one trailing space so spacing between
// adjacent inline elements survives. An all-whitespace node collapses to a
// single space for the same reason.
func collapseWS(s string) string {
	if strings.TrimSpace(s) == "" {
		if s == "" {
			return ""
		}
		return " "
	}
	var b strings.Builder
	if isASCIISpace(rune(s[0])) {
		b.WriteByte(' ')
	}
	pendingSpace := false
	started := false
	for _, r := range s {
		if isASCIISpace(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && started {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		started = true
		pendingSpace = false
	}
	if pendingSpace {
		b.WriteByte(' ')
	}
	return b.String()
}

func isASCIISpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// oneLine flattens any internal whitespace to single spaces — used for text
// that must stay on one line (headings, link labels, table cells).
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cleanup trims trailing whitespace per line, collapses runs of blank lines to
// a single blank line, and trims the document ends.
func cleanup(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(collapseBlankLines(strings.Join(lines, "\n")))
}

// collapseBlankLines reduces any run of 3+ newlines to exactly 2 in a single
// pass.
func collapseBlankLines(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	newlines := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			newlines++
			if newlines <= 2 {
				b.WriteByte('\n')
			}
			continue
		}
		newlines = 0
		b.WriteByte(s[i])
	}
	return b.String()
}
