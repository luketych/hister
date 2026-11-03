// SPDX-License-Identifier: AGPL-3.0-or-later

package extractor

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
)

func TestYTDownloaderAliasesInitializeOneCanonicalExtractor(t *testing.T) {
	for _, names := range [][]string{{config.LegacyYTDownloaderName}, {config.YTDownloaderName}, {config.LegacyYTDownloaderName, config.YTDownloaderName}} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			cfgs := make(map[string]*config.Extractor)
			for _, name := range names {
				cfgs[name] = &config.Extractor{Enable: true, Options: map[string]any{"output_dir": "/unchanged/media", "timeout": 45}}
			}
			before := make(map[string]*config.Extractor)
			for name, entry := range cfgs {
				before[name] = entry.Clone()
			}
			r := NewDefaultRegistry()
			if err := r.Init(cfgs); err != nil {
				t.Fatal(err)
			}
			if fields := ValidateConfiguration(cfgs); len(fields) != 0 {
				t.Fatalf("alias was rejected by draft validation: %#v", fields)
			}
			if !reflect.DeepEqual(before, cfgs) {
				t.Fatal("initialization mutated caller settings")
			}
			count := 0
			for _, entry := range r.List() {
				if entry.Name == config.LegacyYTDownloaderName {
					t.Fatal("legacy alias registered a second extractor")
				}
				if entry.Name == config.YTDownloaderName {
					count++
					if !entry.Enabled || entry.Options["output_dir"] != "/unchanged/media" || entry.Options["timeout"] != 45 {
						t.Fatalf("effective settings changed: %#v", entry)
					}
				}
			}
			if count != 1 {
				t.Fatalf("canonical registrations = %d, want 1", count)
			}
			for _, name := range []string{config.YTDownloaderName, config.LegacyYTDownloaderName, "YT_VIDEO_DOWNLOADER"} {
				preview, err := r.Preview(&document.Document{URL: "https://www.youtube.com/watch?v=abcdefghijk"}, name)
				if err != nil || strings.Contains(preview.Content, "<a ") {
					t.Fatalf("alias preview failed or invented playback: %#v, %v", preview, err)
				}
			}
			for _, field := range r.ConfigurationFields(nil) {
				if strings.Contains(field.Path, config.LegacyYTDownloaderName) {
					t.Fatalf("catalog emitted legacy name: %#v", field)
				}
				if field.Path == "extractors.yt_downloader.enable" && field.Default != false {
					t.Fatalf("alias changed opt-in default: %#v", field)
				}
			}
		})
	}
}

func TestYTDownloaderAliasConflictIsAtomicAndRedacted(t *testing.T) {
	r := NewDefaultRegistry()
	cfgs := map[string]*config.Extractor{
		config.LegacyYTDownloaderName: {Enable: true, Options: map[string]any{"binary": "/private/legacy-executable"}},
		config.YTDownloaderName:       {Enable: true, Options: map[string]any{"binary": "/private/canonical-executable"}},
	}
	if err := r.Init(cfgs); !errors.Is(err, config.ErrExtractorAliasConflict) || strings.Contains(err.Error(), "/private/") {
		t.Fatalf("conflict was ignored or leaked values: %v", err)
	}
	for _, entry := range r.List() {
		if entry.Name == config.YTDownloaderName && entry.Enabled {
			t.Fatal("failed alias validation partially configured the registry")
		}
	}
	fields := ValidateConfiguration(cfgs)
	if len(fields) != 1 || fields[0].Code != "conflicting_alias" || strings.Contains(fields[0].Message, "/private/") {
		t.Fatalf("draft conflict = %#v", fields)
	}
}

func TestYTDownloaderAliasStillRequiresEnabledMatchingPreview(t *testing.T) {
	r := NewDefaultRegistry()
	if _, err := r.Preview(&document.Document{URL: "https://www.youtube.com/watch?v=abcdefghijk"}, config.LegacyYTDownloaderName); !errors.Is(err, ErrNoExtractor) {
		t.Fatalf("alias bypassed disabled extractor: %v", err)
	}
	if err := r.Init(map[string]*config.Extractor{config.LegacyYTDownloaderName: {Enable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Preview(&document.Document{URL: "https://example.com/"}, config.LegacyYTDownloaderName); !errors.Is(err, ErrNoExtractor) {
		t.Fatalf("alias bypassed source matching: %v", err)
	}
	if _, err := NewRegistry(&yt.YTDownloader{}, &yt.YTDownloader{}); !errors.Is(err, ErrDuplicateExtractor) {
		t.Fatalf("duplicate registration = %v", err)
	}
}
