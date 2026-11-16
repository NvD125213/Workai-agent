// Package htmltext turns job posting HTML into plain text ready for AI matching.
// It is shared by the job source adapters (TopCV, VietnamWorks, ...).
package htmltext

import (
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// Clean collapses whitespace (including &nbsp;) into single spaces.
func Clean(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, " ", " ")), " ")
}

// FromHTML renders an HTML fragment as plain text: one line per block, "- " for list items.
func FromHTML(fragment string) string {
	if strings.TrimSpace(fragment) == "" {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + fragment + "</div>"))
	if err != nil {
		return Clean(fragment)
	}
	return FromSelection(doc.Find("body > div").First())
}

// FromSelection renders the nodes of s as plain text, like FromHTML.
func FromSelection(s *goquery.Selection) string {
	var b strings.Builder
	for _, n := range s.Nodes {
		writeNode(&b, n)
	}
	var lines []string
	for _, line := range strings.Split(b.String(), "\n") {
		line = Clean(line)
		// Employers often type their own bullet inside <li> ("<li> - Item"); keep just one.
		if rest, ok := strings.CutPrefix(line, "- "); ok {
			line = "- " + strings.TrimLeft(rest, "-•+*· ")
		}
		if line != "" && line != "-" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func writeNode(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.ElementNode:
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			writeNode(b, c)
		}
		return
	}

	switch n.Data {
	case "script", "style":
		return
	case "br":
		b.WriteString("\n")
		return
	case "li":
		b.WriteString("\n- ")
	case "p", "div", "ul", "ol", "h1", "h2", "h3", "h4", "h5", "h6", "tr", "table":
		// A block right inside <li> stays on the bullet's line.
		if !strings.HasSuffix(b.String(), "- ") {
			b.WriteString("\n")
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		writeNode(b, c)
	}
	switch n.Data {
	case "li", "p", "div", "ul", "ol", "h1", "h2", "h3", "h4", "h5", "h6", "tr", "table":
		b.WriteString("\n")
	}
}
