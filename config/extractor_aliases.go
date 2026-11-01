// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
)

const (
	YTDownloaderName       = "yt_downloader"
	LegacyYTDownloaderName = "yt_video_downloader"
)

var ErrExtractorAliasConflict = errors.New("conflicting extractors.yt_video_downloader and extractors.yt_downloader settings; use extractors.yt_downloader only, or supply matching settings")

// CanonicalExtractorName also preserves the existing case-insensitive preview
// selector contract. Only the old YouTube integration name is an alias.
func CanonicalExtractorName(name string) string {
	name = strings.ToLower(name)
	if name == LegacyYTDownloaderName {
		return YTDownloaderName
	}
	return name
}

func canonicalExtractorSettingPath(path string) string {
	parts := strings.Split(path, ".")
	if len(parts) >= 2 && strings.EqualFold(parts[0], "extractors") && strings.EqualFold(parts[1], LegacyYTDownloaderName) {
		parts[0], parts[1] = "extractors", YTDownloaderName
		return strings.Join(parts, ".")
	}
	return path
}

// Normalize aliases within EACH input layer, before Viper merging, strict
// decoding and source tracking. Otherwise an old file key and a new managed
// key would evade normal precedence/locking as if they were different settings.
// inputs.apply has already detached value from the caller's map.
func normalizeExtractorAliasInput(path string, value any) (string, any, error) {
	path = canonicalExtractorSettingPath(path)
	fields, ok := value.(map[string]any)
	if !ok {
		return path, value, nil
	}
	switch strings.ToLower(path) {
	case "":
		for key, child := range fields {
			if strings.EqualFold(key, "extractors") {
				_, normalized, err := normalizeExtractorAliasInput("extractors", child)
				if err != nil {
					return path, nil, err
				}
				fields[key] = normalized
			}
		}
	case "extractors":
		var entry any
		found := false
		for key, child := range fields {
			lower := strings.ToLower(key)
			if lower != YTDownloaderName && lower != LegacyYTDownloaderName {
				continue
			}
			if found {
				var err error
				entry, err = mergeExtractorAliasValues(entry, child)
				if err != nil {
					return path, nil, err
				}
			} else {
				entry, found = child, true
			}
			delete(fields, key)
		}
		if found {
			fields[YTDownloaderName] = entry
		}
	}
	return path, fields, nil
}

// Alias blocks can supply disjoint settings; overlapping leaves must agree.
// No precedence is guessed between two spellings in the same layer. Values
// never appear in errors, including unknown or malformed option values.
func mergeExtractorAliasValues(a, b any) (any, error) {
	left, leftMap := a.(map[string]any)
	right, rightMap := b.(map[string]any)
	if leftMap && rightMap {
		merged := make(map[string]any, len(left)+len(right))
		for _, fields := range []map[string]any{left, right} {
			for key, value := range fields {
				key = strings.ToLower(key) // Viper also folds configuration keys.
				if previous, present := merged[key]; present {
					var err error
					value, err = mergeExtractorAliasValues(previous, value)
					if err != nil {
						return nil, err
					}
				}
				merged[key] = value
			}
		}
		return merged, nil
	}
	leftJSON, leftErr := json.Marshal(a)
	rightJSON, rightErr := json.Marshal(b)
	if leftErr != nil || rightErr != nil || !bytes.Equal(leftJSON, rightJSON) {
		return nil, ErrExtractorAliasConflict
	}
	return a, nil
}

// NormalizeExtractorConfigs covers programmatic callers too (registry Init,
// diagnostics, direct Indexer.New). It never mutates the caller's configuration.
func NormalizeExtractorConfigs(cfgs map[string]*Extractor) (map[string]*Extractor, error) {
	normalized := maps.Clone(cfgs)
	legacy, present := cfgs[LegacyYTDownloaderName]
	if !present {
		return normalized, nil
	}
	canonical, both := cfgs[YTDownloaderName]
	delete(normalized, LegacyYTDownloaderName)
	if !both {
		if legacy == nil {
			normalized[YTDownloaderName] = nil
		} else {
			normalized[YTDownloaderName] = legacy.Clone()
		}
		return normalized, nil
	}
	if legacy == nil || canonical == nil {
		return nil, ErrExtractorAliasConflict
	}
	if !legacy.enableOmitted && !canonical.enableOmitted && legacy.Enable != canonical.Enable {
		return nil, ErrExtractorAliasConflict
	}
	merged := canonical.Clone()
	if merged.enableOmitted && !legacy.enableOmitted {
		merged.Enable, merged.enableOmitted = legacy.Enable, false
	}
	options, err := mergeExtractorAliasValues(legacy.Options, canonical.Options)
	if err != nil {
		return nil, err
	}
	merged.Options = cloneInputValue(reflect.ValueOf(options)).Interface().(map[string]any)
	normalized[YTDownloaderName] = merged
	return normalized, nil
}
