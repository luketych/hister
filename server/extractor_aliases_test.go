// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/extractor"
	"github.com/asciimoo/hister/server/testutil"
)

func TestYTDownloaderAliasesSurviveRuntimeReloadWithoutDuplicates(t *testing.T) {
	previous, err := extractor.NewRegistry(extractor.DefaultRegistry().Extractors()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extractor.DefaultRegistry().Replace(previous) })
	_, handler, write := newReloadTestServer(t, false)
	mediaDir := filepath.Join(t.TempDir(), "tools--youtube_downloader")
	block := func(name string, enabled bool) string {
		return fmt.Sprintf("  %s:\n    enable: %t\n    options:\n      output_dir: %q\n      binary: %q\n", name, enabled, mediaDir, filepath.Join(t.TempDir(), "must-not-run"))
	}
	assertCanonical := func() {
		t.Helper()
		rec := testutil.ServeHTTP(t, handler, http.MethodGet, "/hister/api/extractors", nil, map[string]string{"X-Access-Token": "secret"})
		if rec.Code != http.StatusOK {
			t.Fatalf("extractor list = %d %s", rec.Code, rec.Body)
		}
		var entries []extractor.ExtractorInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			if entry.Name == config.LegacyYTDownloaderName {
				t.Fatal("legacy alias appeared in preview menu/list")
			}
			if entry.Name == config.YTDownloaderName {
				count++
				if !entry.Enabled {
					t.Fatal("reload lost enabled state")
				}
			}
		}
		if count != 1 {
			t.Fatalf("canonical registrations = %d", count)
		}
		runtime := handler.(*serverRuntime)
		runtime.mu.Lock()
		generation := runtime.current
		runtime.mu.Unlock()
		if generation.cfg.Extractors[config.LegacyYTDownloaderName] != nil || generation.cfg.Extractors[config.YTDownloaderName].Options["output_dir"] != mediaDir {
			t.Fatal("reload changed media location or retained an alias configuration")
		}
		if generation.idx == nil {
			t.Fatal("reload lost the source index")
		}
	}
	for _, name := range []string{config.LegacyYTDownloaderName, config.YTDownloaderName, config.LegacyYTDownloaderName} {
		write("  title: Alias reload\nextractors:\n" + block(name, true))
		if status := reloadRequest(t, handler, "secret"); status != http.StatusOK {
			t.Fatalf("%s reload = %d", name, status)
		}
		assertCanonical()
	}
	// Both identical settings are one registration, not two downloads.
	legacy := block(config.LegacyYTDownloaderName, true)
	canonical := strings.Replace(legacy, config.LegacyYTDownloaderName, config.YTDownloaderName, 1)
	write("  title: Alias reload\nextractors:\n" + legacy + canonical)
	if status := reloadRequest(t, handler, "secret"); status != http.StatusOK {
		t.Fatalf("matching duplicate aliases reload = %d", status)
	}
	assertCanonical()
	write("  title: Must not apply\nextractors:\n" + block(config.LegacyYTDownloaderName, true) + block(config.YTDownloaderName, false))
	if status := reloadRequest(t, handler, "secret"); status != http.StatusBadRequest {
		t.Fatalf("conflicting alias reload = %d, want 400", status)
	}
	assertCanonical()
}
