package httpapi

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Forge installer compatibility; SHA-256 is also supplied.
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

type installerArchiveStorage struct {
	testArtifactStorage
	mu      sync.RWMutex
	objects map[string][]byte
}

func (s *installerArchiveStorage) Exists(_ context.Context, key string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.objects[key]
	return ok, nil
}
func (s *installerArchiveStorage) Open(_ context.Context, key string) (storage.ObjectReader, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	body, ok := s.objects[key]
	if !ok {
		return storage.ObjectReader{}, storage.ErrObjectNotFound
	}
	return storage.ObjectReader{Body: io.NopCloser(bytes.NewReader(body)), Size: int64(len(body)), ContentType: "application/gzip"}, nil
}
func (s *installerArchiveStorage) Stat(_ context.Context, key string) (storage.ObjectAttrs, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	body, ok := s.objects[key]
	if !ok {
		return storage.ObjectAttrs{}, storage.ErrObjectNotFound
	}
	return storage.ObjectAttrs{Size: int64(len(body)), ContentType: "application/gzip"}, nil
}

func (s *installerArchiveStorage) UploadIfAbsent(_ context.Context, key, _ string, body []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; ok {
		return false, nil
	}
	s.objects[key] = bytes.Clone(body)
	return true, nil
}

func (s *installerArchiveStorage) UploadReaderIfAbsent(ctx context.Context, key, contentType string, body io.Reader) (bool, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return false, err
	}
	return s.UploadIfAbsent(ctx, key, contentType, data)
}

func TestReleasePreflightIncludesOpenVox(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(source), "\n") {
		if strings.HasPrefix(line, "release-preflight:") {
			if slices.Contains(strings.Fields(strings.SplitN(line, "##", 2)[0]), "test-openvox") {
				return
			}
			t.Fatal("release-preflight must include the standard OpenVox installer test")
		}
	}
	t.Fatal("release-preflight target is missing")
}

func TestCIIncludesOpenVox(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("../../.github/workflows/checks.yml")
	if err != nil {
		t.Fatal(err)
	}
	_, job, ok := strings.Cut(string(source), "\n  openvox:\n")
	if !ok {
		t.Fatal("reusable CI checks must include an OpenVox job")
	}
	var lines []string
	for line := range strings.SplitSeq(job, "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":") {
			break
		}
		lines = append(lines, line)
	}
	job = strings.Join(lines, "\n")
	if !strings.Contains(job, "run: make test-openvox") || strings.Contains(job, "continue-on-error:") || strings.Contains(job, "    if:") {
		t.Fatal("OpenVox CI job must unconditionally require the real installer test")
	}
}

func TestOpenVox8Installer(t *testing.T) {
	image := os.Getenv("PUPPET_FORGE_TEST_OPENVOX_IMAGE")
	if image == "" {
		t.Skip("set PUPPET_FORGE_TEST_OPENVOX_IMAGE to run the standard OpenVox 8 installer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	st := newHTTPAPITestStore(t)
	artifacts := &installerArchiveStorage{objects: make(map[string][]byte)}
	upstreamResponses := make(map[string]map[string]any)
	upstreamArchives := make(map[string][]byte)
	var catalogRequests, archiveRequests atomic.Int32
	for _, spec := range []struct {
		owner, name, version string
		upstream             bool
	}{
		{"puppetlabs", "stdlib", "10.1.0", true}, {"puppetlabs", "apt", "11.4.0", true}, {"teamname", "common", "1.0.0", false},
	} {
		metadata := map[string]any{"name": spec.owner + "-" + spec.name, "version": spec.version, "author": "Forge installer test", "license": "Apache-2.0", "summary": "Installer compatibility fixture", "source": "https://example.com/fixtures", "dependencies": []any{}, "requirements": []any{map[string]any{"name": "puppet", "version_requirement": ">= 8.0.0 < 9.0.0"}}}
		if spec.name == "apt" {
			metadata["dependencies"] = []any{map[string]any{"name": "puppetlabs/stdlib", "version_requirement": ">= 10.1.0 < 11.0.0"}}
		}
		if spec.name == "common" {
			metadata["dependencies"] = []any{map[string]any{"name": "puppetlabs/apt", "version_requirement": "11.4.0"}}
		}
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		slug := spec.owner + "-" + spec.name + "-" + spec.version
		archive, err := testutil.BuildTarGz(map[string]string{slug + "/metadata.json": string(raw), slug + "/manifests/init.pp": "class " + spec.name + " {}\n"})
		if err != nil {
			t.Fatal(err)
		}
		key := "modules/" + slug + ".tar.gz"
		md5sum := md5.Sum(archive) // #nosec G401 -- Required by the Forge V3 installer protocol.
		sha := sha256.Sum256(archive)
		if spec.upstream {
			upstreamResponses["/v3/modules/"+spec.owner+"-"+spec.name] = map[string]any{"slug": spec.owner + "-" + spec.name, "owner": spec.owner, "name": spec.name, "releases": []any{map[string]any{"slug": slug, "version": spec.version}}, "current_release": map[string]any{"slug": slug, "version": spec.version}}
			upstreamResponses["/v3/releases/"+slug] = map[string]any{"slug": slug, "version": spec.version, "metadata": metadata, "file_uri": "/v3/files/" + slug + ".tar.gz", "file_md5": fmt.Sprintf("%x", md5sum), "file_sha256": fmt.Sprintf("%x", sha), "file_size": len(archive)}
			upstreamArchives["/v3/files/"+slug+".tar.gz"] = archive
			continue
		}
		artifacts.objects[key] = archive
		module, err := st.UpsertModule(ctx, spec.owner, spec.name)
		if err != nil {
			t.Fatal(err)
		}
		release := store.NewRelease(module.ID, spec.owner, spec.name, spec.version, "", "", slug+".tar.gz", "application/gzip", fmt.Sprintf("%x", md5sum), fmt.Sprintf("%x", sha), key, int64(len(archive)), metadata)
		if _, err := st.CreateRelease(ctx, release); err != nil {
			t.Fatal(err)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Error("client token leaked upstream")
		}
		if body, ok := upstreamArchives[req.URL.Path]; ok {
			archiveRequests.Add(1)
			w.Header().Set("Content-Type", "application/gzip")
			if _, err := w.Write(body); err != nil {
				t.Error(err)
			}
			return
		}
		if strings.HasPrefix(req.URL.Path, "/v3/modules/") {
			catalogRequests.Add(1)
		}
		payload, ok := upstreamResponses[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	forge, err := proxy.NewForgeProxy(upstream.URL, time.Minute, 1<<20, artifacts, "upstream-cache", proxy.WithHTTPClient(upstream.Client()), proxy.WithPrivateNetworks())
	if err != nil {
		t.Fatal(err)
	}
	token := rand.Text()
	if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{{Team: "consumer", ReadTokens: []string{token}}}); err != nil {
		t.Fatal(err)
	}
	configs, err := st.LoadTeamConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := auth.NewAuthorizerWithTokenHasher(configs, httpAPITestTokenHasher())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(newTestRouter(service.NewModuleService(st, artifacts, "modules", forge), forge.Handler(), "", authorizer, nil, "", false, defaultActiveReleaseTTL))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Listener.Close(); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/openvox-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	docker := os.Getenv("DOCKER")
	if docker == "" {
		docker = "docker"
	}
	name := "forge-openvox-" + strings.ToLower(rand.Text())
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(cleanupCtx, docker, "rm", "--force", name).CombinedOutput(); err != nil {
			t.Logf("installer container cleanup: %v: %s", err, output)
		}
	})
	command := exec.CommandContext(ctx, docker, "run", "--rm", "--init", "--name", name, "--add-host=host.docker.internal:host-gateway", "--mount", "type=bind,src="+script+",dst=/openvox-install.sh,readonly", "--env", "OPENVOX_READ_TOKEN", "--env", "OPENVOX_FORGE_URL=http://host.docker.internal:"+port, "--env", "OPENVOX_COMMON_MODULE=teamname-common", "--entrypoint", "/bin/bash", image, "/openvox-install.sh")
	command.Env = append(os.Environ(), "OPENVOX_READ_TOKEN="+token)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("standard OpenVox installer: %v\n%s", err, strings.ReplaceAll(string(output), token, "[redacted]"))
	}
	if catalogRequests.Load() != 2 || archiveRequests.Load() != 2 {
		t.Fatalf("cold/warm cache requests: catalogs=%d archives=%d", catalogRequests.Load(), archiveRequests.Load())
	}
	t.Log(string(output))
}
