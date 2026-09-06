package ingest

import (
	"context"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"golang.org/x/net/html"
)

// htmlLoader extracts text in document order. h1/h2 headings start named
// sections (the heading text opens the section body, mirroring the
// Markdown loader); h3-h6 and other block elements are paragraph breaks.
// script/style/head/noscript/template subtrees are dropped entirely,
// pre keeps its raw text, and entities are decoded by the parser.
type htmlLoader struct{}

func (htmlLoader) MediaType() string { return "text/html" }

func (htmlLoader) Load(_ context.Context, in LoadInput) (*Result, error) {
	doc, err := html.Parse(strings.NewReader(string(in.Body)))
	if err != nil {
		return nil, err
	}
	w := &htmlWalker{}
	w.walk(doc)
	w.finish()
	return &Result{Sections: w.sections}, nil
}

// htmlBlock is the set of elements that force a paragraph boundary.
var htmlBlock = map[string]bool{
	"p": true, "div": true, "ul": true, "ol": true, "li": true,
	"table": true, "thead": true, "tbody": true, "tr": true,
	"dl": true, "dt": true, "dd": true, "blockquote": true,
	"section": true, "article": true, "header": true, "footer": true,
	"main": true, "aside": true, "nav": true, "figure": true,
	"figcaption": true, "form": true, "fieldset": true,
	"h3": true, "h4": true, "h5": true, "h6": true,
	"br": true, "hr": true, "address": true,
}

// htmlSkip subtrees never contribute text.
var htmlSkip = map[string]bool{
	"script": true, "style": true, "head": true,
	"noscript": true, "template": true, "iframe": true,
}

type htmlWalker struct {
	sections []memory.DocumentSection
	name     string
	sec      strings.Builder // current section text
	para     strings.Builder // current paragraph
}

// flushPara folds the current paragraph's whitespace and appends it to
// the section, paragraphs separated by a blank line.
func (w *htmlWalker) flushPara() {
	p := strings.Join(strings.Fields(w.para.String()), " ")
	w.para.Reset()
	if p == "" {
		return
	}
	if w.sec.Len() > 0 {
		w.sec.WriteString("\n\n")
	}
	w.sec.WriteString(p)
}

func (w *htmlWalker) flushSection() {
	w.flushPara()
	text := strings.TrimSpace(w.sec.String())
	w.sec.Reset()
	if text != "" {
		w.sections = append(w.sections, memory.DocumentSection{Name: w.name, Text: text})
	}
	w.name = ""
}

func (w *htmlWalker) finish() { w.flushSection() }

// appendRaw adds text as its own paragraph without whitespace folding
// (pre content).
func (w *htmlWalker) appendRaw(text string) {
	text = strings.Trim(text, "\n")
	if text == "" {
		return
	}
	if w.sec.Len() > 0 {
		w.sec.WriteString("\n\n")
	}
	w.sec.WriteString(text)
}

// textContent gathers all text in a subtree (heading names, pre bodies).
func textContent(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		for ch := c.FirstChild; ch != nil; ch = ch.NextSibling {
			rec(ch)
		}
	}
	rec(n)
	return b.String()
}

func (w *htmlWalker) walk(n *html.Node) {
	if n.Type == html.ElementNode {
		tag := n.Data
		if htmlSkip[tag] {
			return
		}
		if tag == "h1" || tag == "h2" {
			w.flushSection()
			w.name = strings.Join(strings.Fields(textContent(n)), " ")
			w.para.WriteString(w.name)
			w.flushPara()
			return
		}
		if tag == "pre" {
			w.flushPara()
			w.appendRaw(textContent(n))
			return
		}
		if htmlBlock[tag] {
			w.flushPara()
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				w.walk(c)
			}
			w.flushPara()
			return
		}
	}
	if n.Type == html.TextNode {
		w.para.WriteString(n.Data)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}
}
