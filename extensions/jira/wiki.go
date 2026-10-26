package jira

import (
	"regexp"
	"strings"
)

// Jira's v2 API speaks wiki markup; agents write Markdown. These convert the common parts both ways. They are
// deliberately small: headings, emphasis, inline and fenced code, links, images and lists.

var (
	mdFence   = regexp.MustCompile("^```\\s*([\\w+-]*)\\s*$")
	mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdBullet  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdNumber  = regexp.MustCompile(`^(\s*)\d+[.)]\s+(.*)$`)
	mdCode    = regexp.MustCompile("`([^`]+)`")
	mdBold    = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	mdImage   = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)\)`)
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdRunAt   = regexp.MustCompile(`^\[runAt:`)

	wikiCode    = regexp.MustCompile(`^\{(?:code|noformat)(?::([^}|]*))?[^}]*\}\s*$`)
	wikiHeading = regexp.MustCompile(`^h([1-6])\.\s+(.*)$`)
	wikiBullet  = regexp.MustCompile(`^(\*+|-)\s+(.*)$`)
	wikiNumber  = regexp.MustCompile(`^(#+)\s+(.*)$`)
	wikiMono    = regexp.MustCompile(`\{\{([^}]+)\}\}`)
	wikiBold    = regexp.MustCompile(`(^|[^\w*])\*([^*\s][^*]*?)\*([^\w*]|$)`)
	wikiImage   = regexp.MustCompile(`!([^!\s|]+)(?:\|[^!]*)?!`)
	wikiLink    = regexp.MustCompile(`\[([^\]|]+)\|([^\]]+)\]`)
)

// ToWiki turns Markdown into Jira wiki markup.
func ToWiki(md string) string {
	var out []string
	inCode := false
	for _, line := range strings.Split(md, "\n") {
		if m := mdFence.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if inCode {
				out = append(out, "{code}")
			} else if m[1] != "" {
				out = append(out, "{code:"+m[1]+"}")
			} else {
				out = append(out, "{code}")
			}
			inCode = !inCode
			continue
		}
		if inCode {
			out = append(out, line)
			continue
		}
		if mdRunAt.MatchString(strings.TrimSpace(line)) {
			// A bracket would start a link; escaped, it shows as typed and still reads as a marker.
			out = append(out, `\`+strings.TrimSpace(line))
			continue
		}
		prefix := ""
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			prefix, line = "h"+string(rune('0'+len(m[1])))+". ", m[2]
		} else if m := mdBullet.FindStringSubmatch(line); m != nil {
			prefix, line = strings.Repeat("*", depth(m[1]))+" ", m[2]
		} else if m := mdNumber.FindStringSubmatch(line); m != nil {
			prefix, line = strings.Repeat("#", depth(m[1]))+" ", m[2]
		}
		out = append(out, prefix+inlineToWiki(line))
	}
	if inCode {
		out = append(out, "{code}")
	}
	return strings.Join(out, "\n")
}

func depth(indent string) int {
	return 1 + len(strings.ReplaceAll(indent, "\t", "  "))/2
}

// inlineToWiki converts a line's inline marks, leaving code spans alone.
func inlineToWiki(line string) string {
	parts := mdCode.Split(line, -1)
	codes := mdCode.FindAllStringSubmatch(line, -1)
	var b strings.Builder
	for i, part := range parts {
		part = mdImage.ReplaceAllString(part, "!$1!")
		part = mdLink.ReplaceAllString(part, "[$1|$2]")
		part = mdBold.ReplaceAllStringFunc(part, func(s string) string {
			m := mdBold.FindStringSubmatch(s)
			return "*" + m[1] + m[2] + "*"
		})
		b.WriteString(part)
		if i < len(codes) {
			b.WriteString("{{" + codes[i][1] + "}}")
		}
	}
	return b.String()
}

// ToMarkdown turns Jira wiki markup into Markdown, roughly.
func ToMarkdown(wiki string) string {
	var out []string
	inCode := false
	for _, line := range strings.Split(strings.ReplaceAll(wiki, "\r\n", "\n"), "\n") {
		if m := wikiCode.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if inCode {
				out = append(out, "```")
			} else {
				out = append(out, "```"+strings.TrimSpace(m[1]))
			}
			inCode = !inCode
			continue
		}
		if inCode {
			out = append(out, line)
			continue
		}
		prefix := ""
		if m := wikiHeading.FindStringSubmatch(line); m != nil {
			prefix, line = strings.Repeat("#", int(m[1][0]-'0'))+" ", m[2]
		} else if m := wikiBullet.FindStringSubmatch(line); m != nil {
			prefix, line = strings.Repeat("  ", max(0, len(m[1])-1))+"- ", m[2]
		} else if m := wikiNumber.FindStringSubmatch(line); m != nil {
			prefix, line = strings.Repeat("  ", len(m[1])-1)+"1. ", m[2]
		}
		out = append(out, prefix+inlineToMarkdown(line))
	}
	if inCode {
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

func inlineToMarkdown(line string) string {
	parts := wikiMono.Split(line, -1)
	codes := wikiMono.FindAllStringSubmatch(line, -1)
	var b strings.Builder
	for i, part := range parts {
		part = strings.ReplaceAll(part, `\[`, "[")
		part = wikiImage.ReplaceAllString(part, "![]($1)")
		part = wikiLink.ReplaceAllString(part, "[$1]($2)")
		for wikiBold.MatchString(part) {
			part = wikiBold.ReplaceAllString(part, "$1**$2**$3")
		}
		b.WriteString(part)
		if i < len(codes) {
			b.WriteString("`" + codes[i][1] + "`")
		}
	}
	return b.String()
}
