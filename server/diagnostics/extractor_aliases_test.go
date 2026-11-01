// SPDX-License-Identifier: AGPL-3.0-or-later

package diagnostics

import (
	"errors"
	"testing"

	"github.com/asciimoo/hister/config"
)

func TestYTDownloaderAliasDiagnostics(t *testing.T) {
	for _, name := range []string{config.LegacyYTDownloaderName, config.YTDownloaderName} {
		cfg := config.CreateDefaultConfig()
		cfg.Extractors = map[string]*config.Extractor{name: {Enable: true, Options: map[string]any{"timeout": 30}}}
		registry, err := Extractors(cfg)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range registry.List() {
			if entry.Name == config.YTDownloaderName {
				count++
				if !entry.Enabled {
					t.Fatal("alias did not enable the canonical extractor")
				}
			}
			if entry.Name == config.LegacyYTDownloaderName {
				t.Fatal("diagnostics retained a duplicate alias")
			}
		}
		if count != 1 || cfg.Extractors[name] == nil {
			t.Fatal("diagnostics mutated the caller or omitted the canonical extractor")
		}
	}
	cfg := config.CreateDefaultConfig()
	cfg.Extractors = map[string]*config.Extractor{config.LegacyYTDownloaderName: {Enable: true}, config.YTDownloaderName: {Enable: false}}
	if _, err := Extractors(cfg); !errors.Is(err, config.ErrExtractorAliasConflict) {
		t.Fatalf("diagnostics ignored alias conflict: %v", err)
	}
	cfg.Extractors = map[string]*config.Extractor{config.LegacyYTDownloaderName: {Enable: true, Options: map[string]any{"typo": true}}}
	if _, err := Extractors(cfg); err == nil {
		t.Fatal("legacy alias bypassed strict option validation")
	}
}
