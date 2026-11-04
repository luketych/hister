// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestYTDownloaderAliasLoadingAndSources(t *testing.T) {
	for _, name := range []string{"yt_video_downloader", "yt_downloader"} {
		t.Run(name, func(t *testing.T) {
			raw := []byte("extractors:\n  " + name + ":\n    enable: true\n    options:\n      output_dir: /unchanged/media\n      timeout: 45\n")
			cfg, err := decodeConfig(raw, "example.yml", true)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Extractors) != 1 || cfg.Extractors["yt_video_downloader"] != nil {
				t.Fatalf("alias was not canonicalized: %#v", cfg.Extractors)
			}
			entry := cfg.Extractors["yt_downloader"]
			if entry == nil || !entry.Enabled(false) || entry.Options["output_dir"] != "/unchanged/media" {
				t.Fatalf("configuration changed: %#v", entry)
			}
			for _, path := range []string{"extractors.yt_downloader.enable", "extractors.yt_video_downloader.enable"} {
				if source := cfg.Source(path); source.Kind != "file" || source.Name != "example.yml" {
					t.Fatalf("source for %s = %#v", path, source)
				}
			}
		})
	}
}

func TestYTDownloaderAliasPreservesOmittedEnableAndDetachedOptions(t *testing.T) {
	for _, name := range []string{LegacyYTDownloaderName, YTDownloaderName} {
		cfg, err := decodeConfig([]byte("extractors:\n  "+name+":\n    options: {timeout: 30}\n"), "example.yml", true)
		if err != nil {
			t.Fatal(err)
		}
		entry := cfg.Extractors[YTDownloaderName]
		if entry.Enabled(false) || !entry.Enabled(true) {
			t.Fatalf("%s alias turned omitted enable into an explicit value", name)
		}
	}
	cfgs := map[string]*Extractor{
		LegacyYTDownloaderName: {Enable: true, Options: map[string]any{"timeout": 30}},
		YTDownloaderName:       {Enable: true, Options: map[string]any{"timeout": float64(30), "transcribe": true}},
	}
	normalized, err := NormalizeExtractorConfigs(cfgs)
	if err != nil || len(normalized) != 1 || !normalized[YTDownloaderName].Options["transcribe"].(bool) {
		t.Fatalf("compatible programmatic aliases rejected: %#v, %v", normalized, err)
	}
	normalized[YTDownloaderName].Options["timeout"] = 60
	if cfgs[LegacyYTDownloaderName].Options["timeout"] != 30 || cfgs[YTDownloaderName].Options["timeout"] != float64(30) {
		t.Fatal("normalization changed the caller's option maps")
	}
}

func TestYTDownloaderAliasConflictsAndCompatibleBlocks(t *testing.T) {
	for _, test := range []struct {
		name, legacy, canonical string
		conflict                bool
	}{
		{"enabled conflict", "enable: true", "enable: false", true},
		{"option conflict", "options: {timeout: 30}", "options: {timeout: 60}", true},
		{"invalid shape conflict", "options: {timeout: 30}", "options: invalid", true},
		{"same settings", "enable: true\n    options: {timeout: 30}", "enable: true\n    options: {timeout: 30}", false},
		{"disjoint settings", "enable: true\n    options: {timeout: 30}", "options: {transcribe: true}", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte("extractors:\n  yt_video_downloader:\n    " + test.legacy + "\n  yt_downloader:\n    " + test.canonical + "\n")
			cfg, err := decodeConfig(raw, "example.yml", true)
			if test.conflict {
				if err == nil || !strings.Contains(err.Error(), "extractors.yt_video_downloader") || !strings.Contains(err.Error(), "extractors.yt_downloader") {
					t.Fatalf("expected actionable conflict, got %v", err)
				}
				return
			}
			if err != nil || len(cfg.Extractors) != 1 || !cfg.Extractors["yt_downloader"].Enabled(false) {
				t.Fatalf("compatible blocks did not become one entry: %#v, %v", cfg, err)
			}
		})
	}
	_, err := decodeConfig([]byte("extractors:\n  yt_video_downloader:\n    unknown_field: true\n"), "example.yml", true)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("alias bypassed strict key validation: %v", err)
	}
}

func TestYTDownloaderAliasNamedOverrideUsesCanonicalSetting(t *testing.T) {
	inputs, err := fileInputs([]byte("extractors:\n  yt_downloader:\n    enable: true\n"), "example.yml")
	if err != nil {
		t.Fatal(err)
	}
	if err := inputs.apply(Override{Path: "extractors.yt_video_downloader.enable", Value: false, Source: ValueSource{Kind: "flag", Name: "legacy-override"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := inputs.decode(true)
	if err != nil || len(cfg.Extractors) != 1 || cfg.Extractors[YTDownloaderName].Enabled(true) {
		t.Fatalf("legacy override did not select the canonical setting: %#v, %v", cfg, err)
	}
	if source := cfg.Source("extractors.yt_downloader.enable"); source.Kind != "flag" || source.Name != "legacy-override" {
		t.Fatalf("legacy override lost its canonical provenance: %#v", source)
	}
}

func TestYTDownloaderAliasCompositionReloadAndDraftDoNotRewriteFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yml")
	raw := []byte("app:\n  directory: " + dir + "\nextractors:\n  yt_video_downloader:\n    enable: true\n    options: {timeout: 30}\n")
	managed := []byte("extractors:\n  yt_downloader:\n    enable: false\napp:\n  subtitle: Managed\n")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.web.yml"), managed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadForInspection(file, true, Override{Path: "extractors.yt_downloader.options.timeout", Value: 50, Source: ValueSource{Kind: "flag", Name: "--timeout"}})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Extractors["yt_downloader"].Enabled(false) || cfg.Extractors["yt_downloader"].Options["timeout"] != 50 {
		t.Fatalf("aliases bypassed precedence: %#v", cfg.Extractors)
	}
	if source := cfg.Source("extractors.yt_downloader.enable"); source.Kind != "file" || !source.WebSelected || !source.Locked {
		t.Fatalf("canonical source lost authoritative legacy setting: %#v", source)
	}
	candidate, fields := cfg.PrepareDraft(nil)
	if len(fields) != 0 || candidate.Extractors["yt_video_downloader"] != nil || !candidate.Extractors["yt_downloader"].Enabled(false) {
		t.Fatalf("draft lost canonical config: %#v, %#v", candidate, fields)
	}
	reloaded, err := cfg.Reload()
	if err != nil || len(reloaded.Extractors) != 1 || !reloaded.Extractors["yt_downloader"].Enabled(false) {
		t.Fatalf("reload lost compatibility: %#v, %v", reloaded, err)
	}
	for name, original := range map[string][]byte{file: raw, filepath.Join(dir, "config.web.yml"): managed} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != string(original) {
			t.Fatalf("normalization rewrote %s: %v", name, err)
		}
	}
}
