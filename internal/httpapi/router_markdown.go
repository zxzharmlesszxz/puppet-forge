package httpapi

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
)

var relativeLinkPattern = regexp.MustCompile(`(?i)(href|src)="([^"]+)"`)
var readmeCodeBlockPattern = regexp.MustCompile(`(?s)<pre><code(?: class="language-([^"]+)")?>(.*?)</code></pre>`)
var buttonTypePattern = regexp.MustCompile(`^button$`)
var markdownRenderer = goldmark.New(
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
	),
)
var markdownSanitizer = newMarkdownSanitizer()

func renderMarkdown(source, basePath string) template.HTML {
	if strings.TrimSpace(source) == "" {
		return ""
	}

	var buf bytes.Buffer
	if err := markdownRenderer.Convert([]byte(source), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(source))
	}

	html := buf.String()
	if basePath != "" {
		html = rewriteRelativeReadmeLinks(html, basePath)
	}
	html = wrapReadmeCodeBlocks(html)
	html = markdownSanitizer.Sanitize(html)

	return template.HTML(html)
}

func newMarkdownSanitizer() *bluemonday.Policy {
	policy := bluemonday.UGCPolicy()
	policy.AllowRelativeURLs(true)
	policy.AllowElements("div", "span", "button")
	policy.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("div", "span", "code", "button")
	policy.AllowAttrs("type").Matching(buttonTypePattern).OnElements("button")
	return policy
}

func wrapReadmeCodeBlocks(source string) string {
	return readmeCodeBlockPattern.ReplaceAllStringFunc(source, func(match string) string {
		parts := readmeCodeBlockPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}

		title := readmeCodeBlockTitle(parts[1])

		return fmt.Sprintf(`<div class="code-window">
<div class="code-titlebar">
<span class="code-title">%s</span>
<button class="copy-button" type="button">Copy</button>
</div>
<pre><code>%s</code></pre>
</div>`, template.HTMLEscapeString(title), parts[2])
	})
}

func readmeCodeBlockTitle(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "":
		return "Code"
	case "pp", "puppet":
		return "Puppet"
	default:
		return language
	}
}

func releasePath(suffix string, owner, name, version string) string {
	if version == "" {
		return ""
	}
	return fmt.Sprintf("/api/v1/modules/%s/%s/versions/%s%s", owner, name, version, suffix)
}

func downloadPath(owner, name, version string) string {
	return releasePath("/download", owner, name, version)
}

func releaseAPIPath(owner, name, version string) string {
	return releasePath("", owner, name, version)
}

func readmeBaseHref(owner, name, version string) string {
	if version == "" {
		return ""
	}
	return fmt.Sprintf("/modules/%s/%s/versions/%s/files/", owner, name, version)
}

func rewriteRelativeReadmeLinks(source, basePath string) string {
	return relativeLinkPattern.ReplaceAllStringFunc(source, func(match string) string {
		parts := relativeLinkPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}

		attr := parts[1]
		target := parts[2]
		switch {
		case target == "":
			return match
		case strings.HasPrefix(target, "#"):
			return match
		case strings.HasPrefix(target, "/"):
			return match
		case strings.Contains(target, "://"):
			return match
		case strings.HasPrefix(target, "mailto:"):
			return match
		case strings.HasPrefix(target, "javascript:"):
			return match
		default:
			return fmt.Sprintf(`%s="%s%s"`, attr, basePath, target)
		}
	})
}
