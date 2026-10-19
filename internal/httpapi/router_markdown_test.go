package httpapi

import (
	"strings"
	"testing"
)

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
