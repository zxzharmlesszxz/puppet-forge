package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestManageClearCacheRequiresCSRFAndUpstreamOrigin(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"upstream", "local"} {
		t.Run(source, func(t *testing.T) {
			st := newHTTPAPITestStore(t)
			module, err := st.UpsertModule(t.Context(), "cacheteam", "demo")
			if err != nil {
				t.Fatal(err)
			}
			release := store.NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "upstream-cache/v3/files/demo.tar.gz", 7, nil)
			release.Source = source
			if _, err := st.CreateRelease(t.Context(), release); err != nil {
				t.Fatal(err)
			}
			svc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
			server := newAdminServer(t, svc, http.NotFoundHandler())
			client := server.Client()
			client.Jar = newCookieJar(t)
			postManageToken(t, client, server.URL, "admin-token")
			base := "/manage/modules/cacheteam/demo"
			card := getBody(t, client, server.URL+base+"/card")
			if strings.Contains(card, `aria-label="Clear cached archives"`) != (source == "upstream") {
				t.Fatalf("wrong cache action visibility for %s", source)
			}
			if source == "upstream" {
				for _, want := range []string{`form="clear-cache-cacheteam-demo"`, `data-refresh-target="item-cacheteam-demo"`, `data-async-mutation`} {
					if !strings.Contains(card, want) {
						t.Errorf("cache action misses %s", want)
					}
				}
			}
			for _, csrf := range []bool{false, true} {
				form := url.Values{}
				if csrf {
					form = manageFormValues(t, client, server.URL, form)
				}
				req, err := http.NewRequest(http.MethodPost, server.URL+base+"/clear-cache", strings.NewReader(form.Encode()))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("X-Puppet-Forge-Async-Mutation", "true")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				want := http.StatusForbidden
				if csrf && source == "upstream" {
					want = http.StatusNoContent
					if resp.Header.Get("X-Puppet-Forge-Refresh") != "item-cacheteam-demo" || resp.Header.Get("X-Puppet-Forge-Remove") != "" {
						t.Fatal("cache cleanup must refresh, not remove, the module card")
					}
				}
				if resp.StatusCode != want {
					t.Fatalf("csrf=%v source=%s status=%d, want %d", csrf, source, resp.StatusCode, want)
				}
			}
			events, total, err := st.ListAuditEvents(t.Context(), nil, "clear_upstream_cache", 10, 0)
			if err != nil || total != 1 || len(events) != 1 {
				t.Fatalf("cache action audit: %#v total=%d error=%v", events, total, err)
			}
			wantResult, wantPending := "success", 1
			if source == "local" {
				wantResult, wantPending = "failure", 0
			}
			if events[0].Result != wantResult || events[0].Attributes["module"] != "cacheteam/demo" {
				t.Fatalf("wrong audit result: %#v", events[0])
			}
			if pending, err := st.CountArtifactDeletions(t.Context()); err != nil || pending != wantPending {
				t.Fatalf("pending=%d want=%d error=%v", pending, wantPending, err)
			}
		})
	}
}
