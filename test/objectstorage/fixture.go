// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// Package objectstorage shares real-controller scenarios between the Helm and Kustomize E2E suites.
package objectstorage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type material map[string]string

func (material) String() string   { return "<redacted material>" }
func (material) GoString() string { return "<redacted material>" }

type rotation struct {
	Initial material `json:"initial"`
	Rotated material `json:"rotated"`
}

type fixture struct {
	AWS   rotation `json:"aws"`
	Azure rotation `json:"azure"`
}

const fixtureReadLimit = 1 << 20

func (fixture) String() string   { return "<redacted fixture>" }
func (fixture) GoString() string { return "<redacted fixture>" }

// Missing fixtures skip only the live application scenarios. Malformed or incomplete supplied
// fixtures fail closed. Parser errors can contain credential material and must never be returned.
func loadFixture(path string) (*fixture, error) {
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("E2E_OBJECT_STORAGE_FIXTURE must be an absolute path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open object storage fixture")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, fixtureReadLimit+1))
	if err != nil || len(data) > fixtureReadLimit {
		return nil, errors.New("cannot read object storage fixture or fixture exceeds 1 MiB")
	}
	return decodeFixture(data)
}

func decodeFixture(data []byte) (*fixture, error) {
	invalid := errors.New("invalid object storage fixture JSON (details suppressed)")
	if len(data) > fixtureReadLimit {
		return nil, invalid
	}
	// encoding/json struct decoding merges duplicate objects, accepts case aliases and
	// converts null strings to empty values. All three can silently change a credential
	// fixture, so require exact, unique objects and string values at every level.
	root, err := uniqueJSONObject(data)
	if err != nil || len(root) != 2 || root["aws"] == nil || root["azure"] == nil {
		return nil, invalid
	}
	f := &fixture{}
	for _, provider := range []struct {
		name string
		pair *rotation
	}{{"aws", &f.AWS}, {"azure", &f.Azure}} {
		pair, err := uniqueJSONObject(root[provider.name])
		if err != nil || len(pair) != 2 || pair["initial"] == nil || pair["rotated"] == nil {
			return nil, invalid
		}
		for _, revision := range []struct {
			name   string
			values *material
		}{{"initial", &provider.pair.Initial}, {"rotated", &provider.pair.Rotated}} {
			fields, err := uniqueJSONObject(pair[revision.name])
			if err != nil {
				return nil, invalid
			}
			*revision.values = material{}
			for key, raw := range fields {
				var value string
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
					return nil, invalid
				}
				(*revision.values)[key] = value
			}
		}
	}
	for _, provider := range []struct {
		name string
		pair rotation
		keys []string
	}{
		{"aws", f.AWS, []string{"accessKey", "secretKey"}},
		{"azure", f.Azure, []string{"storageAccount", "storageKey"}},
	} {
		for _, values := range []material{provider.pair.Initial, provider.pair.Rotated} {
			for _, key := range provider.keys {
				if strings.TrimSpace(values[key]) == "" {
					return nil, errors.New("fixture requires initial and rotated material for both providers")
				}
			}
			for key := range values {
				allowed := key == provider.keys[0] || key == provider.keys[1] || (provider.name == "aws" && key == "sessionToken")
				if !allowed {
					return nil, errors.New("fixture contains an unsupported credential key (name suppressed)")
				}
			}
		}
		if reflect.DeepEqual(normalized(provider.pair.Initial), normalized(provider.pair.Rotated)) {
			return nil, errors.New("fixture must change normalized credential material for each rotation")
		}
	}
	return f, nil
}

func normalized(values material) material {
	out := material{}
	for key, value := range values {
		value = strings.TrimSpace(value)
		if key != "sessionToken" || value != "" {
			out[key] = value
		}
	}
	return out
}

func secretData(values ...material) map[string][]byte {
	out := map[string][]byte{}
	for _, entries := range values {
		for key, value := range entries {
			out[key] = []byte(value)
		}
	}
	return out
}

func containsMaterial(data []byte, values []string) bool {
	for _, value := range values {
		// A whitespace-only optional token normalizes to absence. Treating its spaces
		// as credential material would flag ordinary formatted status/log output.
		if strings.TrimSpace(value) != "" {
			encoded, _ := json.Marshal(value)
			// Status and Events are inspected as JSON; Secret.data is base64 in accidental dumps.
			for _, pattern := range [][]byte{[]byte(value), encoded[1 : len(encoded)-1], []byte(base64.StdEncoding.EncodeToString([]byte(value)))} {
				if bytes.Contains(data, pattern) {
					return true
				}
			}
		}
	}
	return false
}
