package docfetch

import (
	"strings"
	"testing"
)

func TestHTMLToMarkdown_StripsTagsAndChrome(t *testing.T) {
	in := `<html><head><title>T</title><style>.x{color:red}</style></head>
<body>
<nav>navigation links</nav>
<h1>Hello</h1>
<p>A <strong>bold</strong> word and a <a href="https://e.com">link</a>.</p>
<script>alert(1)</script>
<ul><li>one</li><li>two</li></ul>
<pre><code>code block</code></pre>
<footer>copyright chrome</footer>
</body></html>`

	got, err := htmlToMarkdown(strings.NewReader(in))
	if err != nil {
		t.Fatalf("htmlToMarkdown: %v", err)
	}
	for _, want := range []string{
		"# Hello", "**bold**", "[link](https://e.com)",
		"- one", "- two", "```", "code block",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"<", ">", "alert(1)", "color:red",
		"navigation links", "copyright chrome"} {
		if strings.Contains(got, bad) {
			t.Errorf("output still contains %q:\n%s", bad, got)
		}
	}
}

func TestHTMLToMarkdown_HeadingLevels(t *testing.T) {
	cases := map[string]string{
		"<h1>A</h1>": "# A",
		"<h2>B</h2>": "## B",
		"<h3>C</h3>": "### C",
		"<h6>F</h6>": "###### F",
	}
	for in, want := range cases {
		got, err := htmlToMarkdown(strings.NewReader(in))
		if err != nil {
			t.Fatalf("htmlToMarkdown(%q): %v", in, err)
		}
		if strings.TrimSpace(got) != want {
			t.Errorf("htmlToMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTMLToMarkdown_Blockquote(t *testing.T) {
	got, err := htmlToMarkdown(strings.NewReader("<blockquote>quoted text</blockquote>"))
	if err != nil {
		t.Fatalf("htmlToMarkdown: %v", err)
	}
	if !strings.Contains(got, "> quoted text") {
		t.Errorf("blockquote not rendered: %q", got)
	}
}

func TestHTMLToMarkdown_PlainTextInput(t *testing.T) {
	// html.Parse tolerates tag-free input — it should round-trip as text.
	got, err := htmlToMarkdown(strings.NewReader("just some plain words"))
	if err != nil {
		t.Fatalf("htmlToMarkdown: %v", err)
	}
	if strings.TrimSpace(got) != "just some plain words" {
		t.Errorf("got %q, want plain words", got)
	}
}

func TestHTMLToMarkdown_Table(t *testing.T) {
	got, err := htmlToMarkdown(strings.NewReader(
		"<table><tr><th>Name</th><th>Type</th></tr><tr><td>id</td><td>int</td></tr></table>"))
	if err != nil {
		t.Fatalf("htmlToMarkdown: %v", err)
	}
	if !strings.Contains(got, "| Name | Type |") || !strings.Contains(got, "| id | int |") {
		t.Errorf("table not rendered as pipe rows: %q", got)
	}
}

func TestCollapseWS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", " "},
		{"hello", "hello"},
		{"  hello   world  ", " hello world "},
		{"a\n\tb", "a b"},
		{"trailing\n", "trailing "},
	}
	for _, c := range cases {
		if got := collapseWS(c.in); got != c.want {
			t.Errorf("collapseWS(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCollapseBlankLines(t *testing.T) {
	got := collapseBlankLines("a\n\n\n\n\nb")
	if got != "a\n\nb" {
		t.Errorf("collapseBlankLines = %q, want %q", got, "a\n\nb")
	}
}
