// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"reflect"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// ValueSource describes a setting's origin without retaining its value.
// It is separate from the field catalog and contains no credentials.
type ValueSource struct {
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Locked      bool   `json:"locked"`
	Reason      string `json:"reason,omitempty"`
	DerivedFrom string `json:"derived_from,omitempty"`
	WebSelected bool   `json:"web_selected"`
}

// Override is an explicit setting assignment. Presence does not depend on
// whether Value differs from the default or the previously loaded value.
type Override struct {
	Path   string
	Value  any
	Source ValueSource
}

// Source returns the active origin of a setting or collection member.
func (c *Config) Source(path string) ValueSource {
	path = canonicalExtractorSettingPath(strings.ToLower(path))
	if path == "app.user_handling" && c.Server.ProxyAuthHeader != "" {
		source := c.Source("server.proxy_auth_header")
		source.Locked = true
		source.DerivedFrom = "server.proxy_auth_header"
		source.Reason = "Enabled by server.proxy_auth_header"
		source.WebSelected = c.sources["app.user_handling"].WebSelected
		if source.WebSelected {
			source.Reason += "; overrides stored web selection"
		}
		return source
	}
	if source, ok := c.sources[sourceSettingPath(strings.ToLower(path))]; ok {
		source.Locked = source.Kind != "web"
		source.Reason = "Set by " + source.Kind + ": " + source.Name
		if source.Locked && source.WebSelected {
			source.Reason += "; overrides stored web selection"
		}
		return source
	}
	return ValueSource{Kind: "default"}
}

// Read presence from YAML before decoding into defaults. Structural containers
// do not select settings; lists and dictionaries select their whole collection.
type configInputs struct {
	values        *viper.Viper
	sources       map[string]ValueSource
	managed       map[string]any
	defaults      *Config
	managedDigest [32]byte
}

func fileInputs(raw []byte, name string) (*configInputs, error) {
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, errors.New("invalid configuration YAML")
	}
	inputs := &configInputs{values: viper.New(), sources: make(map[string]ValueSource)}
	if err := inputs.apply(Override{Value: values, Source: ValueSource{Kind: "file", Name: name}}); err != nil {
		return nil, err
	}
	return inputs, nil
}

// apply is the only operation that introduces source-owned values. A root
// mapping supplies a sparse base layer; named paths override that layer.
func (inputs *configInputs) apply(overrides ...Override) error {
	for _, override := range overrides {
		value := override.Value
		if value != nil {
			value = cloneInputValue(reflect.ValueOf(value)).Interface()
		}
		path, value, err := normalizeExtractorAliasInput(override.Path, value)
		if err != nil {
			return err
		}
		if path == "" {
			values, ok := value.(map[string]any)
			if !ok {
				return errors.New("configuration layer must be a mapping")
			}
			if err := inputs.values.MergeConfigMap(values); err != nil {
				return errors.New("invalid configuration mapping")
			}
		} else {
			inputs.values.Set(path, value)
		}
		inputs.record(strings.ToLower(path), value, override.Source)
	}
	return nil
}

// Viper and the decoder can retain collection members by reference. Each
// candidate owns its inputs so mutations cannot affect later reloads.
func cloneInputValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return value
		}
		copy := reflect.New(value.Type()).Elem()
		if value.Kind() == reflect.Pointer {
			copy.Set(reflect.New(value.Type().Elem()))
			copy.Elem().Set(cloneInputValue(value.Elem()))
		} else {
			copy.Set(cloneInputValue(value.Elem()))
		}
		return copy
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		copy := reflect.MakeMapWithSize(value.Type(), value.Len())
		for iter := value.MapRange(); iter.Next(); {
			copy.SetMapIndex(iter.Key(), cloneInputValue(iter.Value()))
		}
		return copy
	case reflect.Slice, reflect.Array:
		var copy reflect.Value
		if value.Kind() == reflect.Slice {
			if value.IsNil() {
				return value
			}
			copy = reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		} else {
			copy = reflect.New(value.Type()).Elem()
		}
		for i := range value.Len() {
			copy.Index(i).Set(cloneInputValue(value.Index(i)))
		}
		return copy
	case reflect.Struct:
		copy := reflect.New(value.Type()).Elem()
		copy.Set(value)
		for i := range value.NumField() {
			if copy.Field(i).CanSet() {
				copy.Field(i).Set(cloneInputValue(value.Field(i)))
			}
		}
		return copy
	default:
		return value
	}
}

func (inputs *configInputs) record(path string, value any, source ValueSource) {
	var visit func(string, any)
	visit = func(path string, value any) {
		if setting := sourceSettingPath(path); setting != "" {
			inputs.sources[setting] = ValueSource{Kind: source.Kind, Name: source.Name}
			return
		}
		if fields, ok := value.(map[string]any); ok {
			for key, child := range fields {
				childPath := strings.ToLower(key)
				if path != "" {
					childPath = path + "." + childPath
				}
				visit(childPath, child)
			}
		}
	}
	visit(path, value)
}

// Use the configuration shape only to identify structural containers. This is
// not an editing catalog: it does not grant write access or infer option types.
func sourceSettingPath(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, ".")
	typ := reflect.TypeFor[Config]()
	for i, key := range parts {
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		switch typ.Kind() {
		case reflect.Struct:
			var fieldType reflect.Type
			for field := range typ.Fields() {
				if field.Tag.Get("mapstructure") == key {
					fieldType = field.Type
					break
				}
			}
			if fieldType == nil {
				return strings.Join(parts[:i+1], ".")
			}
			typ = fieldType
		case reflect.Map:
			typ = typ.Elem()
		default:
			return strings.Join(parts[:i], ".")
		}
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() == reflect.Struct {
			continue
		}
		if typ.Kind() == reflect.Map {
			element := typ.Elem()
			if element.Kind() == reflect.Pointer {
				element = element.Elem()
			}
			if element.Kind() == reflect.Struct || (i == 2 && parts[0] == "extractors" && key == "options") {
				continue
			}
		}
		return strings.Join(parts[:i+1], ".")
	}
	return ""
}
