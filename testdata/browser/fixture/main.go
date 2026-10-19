package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httpapi"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type fixtureStorage struct{}

func (fixtureStorage) Upload(context.Context, string, string, []byte) error { return nil }
func (fixtureStorage) UploadIfAbsent(context.Context, string, string, []byte) (bool, error) {
	return true, nil
}
func (fixtureStorage) UploadReaderIfAbsent(context.Context, string, string, io.Reader) (bool, error) {
	return true, nil
}
func (fixtureStorage) Delete(context.Context, string) error         { return nil }
func (fixtureStorage) Exists(context.Context, string) (bool, error) { return false, nil }
func (fixtureStorage) Open(context.Context, string) (storage.ObjectReader, error) {
	return storage.ObjectReader{}, storage.ErrObjectNotFound
}
func (fixtureStorage) PublicURL(string) string { return "" }
func (fixtureStorage) Stat(context.Context, string) (storage.ObjectAttrs, error) {
	return storage.ObjectAttrs{}, storage.ErrObjectNotFound
}

func main() {
	ctx := context.Background()
	metadataStore, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer metadataStore.Close()

	for index := range 35 {
		owner := "teamname"
		if index >= 25 {
			owner = "upstream"
		}
		name := fmt.Sprintf("module-%02d", index)
		readme := ""
		if index == 34 {
			readme = "![Allowed badge](https://images.example.test/badge.svg)\n\n" +
				"![Redirected badge](https://images.example.test/redirect.svg)\n\n" +
				"![Blocked redirect](https://images.example.test/blocked-redirect.svg)\n\n" +
				"![Missing badge](https://images.example.test/missing.svg)\n\n" +
				"![](https://images.example.test/no-alt.svg)\n\n" +
				"![Blocked badge](https://blocked.example.test/badge.svg)"
		}
		module, err := metadataStore.UpsertModule(ctx, owner, name)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := metadataStore.CreateRelease(ctx, domain.Release{
			ID: fmt.Sprintf("release-%02d", index), ModuleID: module.ID, Owner: owner, Name: name,
			Version: "1.0.0", Source: "local", FileName: owner + "-" + name + "-1.0.0.tar.gz",
			ContentType: "application/gzip", MD5: "fixture-md5", SHA256: "fixture-sha256",
			StoragePath: "modules/fixture.tar.gz", SizeBytes: 1,
			Readme: readme,
		}); err != nil {
			log.Fatal(err)
		}
	}

	moduleService := service.NewModuleService(metadataStore, fixtureStorage{}, "modules", nil)
	if err := moduleService.ReplaceTeamConfigs(ctx, []auth.TeamConfig{{Team: "teamname"}, {Team: "platform"}}); err != nil {
		log.Fatal(err)
	}
	authorizer, err := auth.NewAuthorizer(nil)
	if err != nil {
		log.Fatal(err)
	}
	tokenHasher, err := auth.NewTokenHasher("browser-fixture-token-pepper-32-bytes")
	if err != nil {
		log.Fatal(err)
	}
	handler, err := httpapi.NewRouter(httpapi.RouterConfig{
		Modules: moduleService, Authorizer: authorizer, TokenHasher: tokenHasher,
		AdminToken: "admin-token", ManageSessionSecret: "browser-fixture-session-secret-32-bytes",
		RefreshAccessConfig: true, PublicModuleAccess: true, ActiveReleaseTTL: 30 * 24 * time.Hour,
		ReadmeImageAllowedHosts: []string{"images.example.test"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fixtureMux := http.NewServeMux()
	fixtureMux.HandleFunc("GET /browser-fixture/badge.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" width="40" height="20"><text x="0" y="15">Badge</text></svg>`)
	})
	fixtureMux.Handle("/", handler)
	address := os.Getenv("BROWSER_FIXTURE_ADDR")
	if address == "" {
		address = "127.0.0.1:18085"
	}
	log.Print("browser fixture starting")
	server := &http.Server{
		Addr:              address,
		Handler:           fixtureMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
