package httpapi

import (
	"strings"
	"testing"
)

func TestRenderMarkdownTables(t *testing.T) {
	t.Parallel()
	source := "## Parameters\n\n" +
		"| APL key | Type | Default |\n" +
		"| --- | --- | --- |\n" +
		"| `docker::repo::settings` | [Settings](settings.md) | `{}` |\n" +
		"| `docker::compose::enabled` | Boolean | `false` |\n" +
		"| unsafe | [link](javascript:alert(1)) | <script>alert(2)</script> |\n"
	body := string(renderMarkdown(source, "/files/"))
	for _, want := range []string{
		"<table>", "<thead>", "<tbody>", "<th>APL key</th>",
		"<td><code>docker::repo::settings</code></td>",
		`href="/files/settings.md"`, "<td><code>false</code></td>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered table misses %q: %s", want, body)
		}
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "javascript:") {
		t.Fatalf("table bypassed Markdown sanitization: %s", body)
	}
}

func TestReadmeImagesUpgradeHTTPWithoutChangingLinks(t *testing.T) {
	body := string(renderMarkdown("[Documentation](http://www.puppetmodule.info/m/puppet-systemd)\n\n"+
		"![Docs](http://www.puppetmodule.info/images/badge.png)\n\n"+
		"![Other](HTTP://images.example.test/badge.svg?branch=main)\n\n"+
		"![Local](images/local.png)\n\n"+
		"![Secure](https://images.example.test/secure.svg)", "/files/"))
	for _, want := range []string{
		`href="http://www.puppetmodule.info/m/puppet-systemd"`,
		`src="https://www.puppetmodule.info/images/badge.png"`,
		`src="https://images.example.test/badge.svg?branch=main"`,
		`src="/files/images/local.png"`,
		`src="https://images.example.test/secure.svg"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered README does not contain %s: %s", want, body)
		}
	}
}
