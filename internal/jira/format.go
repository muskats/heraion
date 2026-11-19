package jira

import (
	"strconv"
	"strings"
)

// The model writes descriptions in simple Markdown. These converters handle
// the parts it uses: "# headings", "- bullets", "```code```" and paragraphs.
// Anything else stays plain text, which both formats accept.

type block struct {
	kind  string // heading, bullets, code, para
	level int
	lines []string
}

func parse(md string) []block {
	var out []block
	var cur *block
	flush := func() {
		if cur != nil && len(cur.lines) > 0 {
			out = append(out, *cur)
		}
		cur = nil
	}
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "```"):
			flush()
			b := block{kind: "code"}
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				b.lines = append(b.lines, lines[i])
			}
			if len(b.lines) == 0 {
				b.lines = []string{""}
			}
			out = append(out, b)
		case t == "":
			flush()
		case strings.HasPrefix(t, "#"):
			flush()
			lvl := len(t) - len(strings.TrimLeft(t, "#"))
			out = append(out, block{kind: "heading", level: min(lvl, 6), lines: []string{strings.TrimSpace(t[lvl:])}})
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			if cur == nil || cur.kind != "bullets" {
				flush()
				cur = &block{kind: "bullets"}
			}
			cur.lines = append(cur.lines, strings.TrimSpace(t[2:]))
		default:
			if cur == nil || cur.kind != "para" {
				flush()
				cur = &block{kind: "para"}
			}
			cur.lines = append(cur.lines, t)
		}
	}
	flush()
	return out
}

type node = map[string]any

func text(s string) []node {
	if s == "" {
		return []node{} // ADF rejects null content and empty text nodes
	}
	return []node{{"type": "text", "text": s}}
}

// ToADF converts Markdown to an Atlassian Document Format doc (Cloud v3).
func ToADF(md string) node {
	content := []node{}
	for _, b := range parse(md) {
		switch b.kind {
		case "heading":
			content = append(content, node{"type": "heading", "attrs": node{"level": b.level}, "content": text(b.lines[0])})
		case "code":
			content = append(content, node{"type": "codeBlock", "content": text(strings.Join(b.lines, "\n"))})
		case "bullets":
			items := []node{}
			for _, l := range b.lines {
				items = append(items, node{"type": "listItem", "content": []node{{"type": "paragraph", "content": text(l)}}})
			}
			content = append(content, node{"type": "bulletList", "content": items})
		default:
			// A hard break keeps the model's line breaks inside a paragraph.
			var inline []node
			for i, l := range b.lines {
				if i > 0 {
					inline = append(inline, node{"type": "hardBreak"})
				}
				inline = append(inline, text(l)...)
			}
			content = append(content, node{"type": "paragraph", "content": inline})
		}
	}
	return node{"type": "doc", "version": 1, "content": content}
}

// ToWiki converts Markdown to Jira wiki markup (Data Center v2).
func ToWiki(md string) string {
	var parts []string
	for _, b := range parse(md) {
		switch b.kind {
		case "heading":
			parts = append(parts, "h"+strconv.Itoa(b.level)+". "+b.lines[0])
		case "code":
			parts = append(parts, "{code}\n"+strings.Join(b.lines, "\n")+"\n{code}")
		case "bullets":
			var ls []string
			for _, l := range b.lines {
				ls = append(ls, "* "+l)
			}
			parts = append(parts, strings.Join(ls, "\n"))
		default:
			parts = append(parts, strings.Join(b.lines, "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}
