// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package digest

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// elided are the two fields no hash can cover, because they are the hashes.
var elided = map[string]bool{"behaviorDigest": true, "contentDigest": true}

var timeType = reflect.TypeOf(metav1.Time{})

// walkSpec enumerates every leaf JSON path in a spec type, using "[]" to mark
// a list element, so that "tools[].effect" is one path regardless of how many
// tools a manifest has.
func walkSpec(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType || t.Kind() != reflect.Struct {
		*out = append(*out, strings.TrimSuffix(prefix, "."))
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if prefix == "" && elided[name] {
			continue
		}

		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct && ft.Elem() != timeType:
			walkSpec(ft.Elem(), prefix+name+"[].", out)
		default:
			walkSpec(ft, prefix+name+".", out)
		}
	}
}

func specPaths() []string {
	var got []string
	walkSpec(reflect.TypeOf(v1alpha1.AgentManifestSpec{}), "", &got)
	sort.Strings(got)
	return got
}

// TestRegistryIsExhaustive is the guard that keeps the classification map
// honest. A field can be added to the API without anyone deciding whether it
// determines behaviour — and if that happens, it silently lands outside
// behaviorDigest and a real behavioural change ships without a canary. This
// test makes that impossible to do by accident.
func TestRegistryIsExhaustive(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range Paths() {
		registered[p] = true
	}

	var missing []string
	for _, p := range specPaths() {
		if !registered[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Errorf("these spec fields are not in the classification map:\n  %s\n\n"+
			"Add each to digest.Registry with an Inclusion (InBoth or ContentOnly), a\n"+
			"NullSemantics, and a Why. When in doubt, use InBoth: including a field that\n"+
			"does not affect behaviour costs one unnecessary canary, while excluding one\n"+
			"that does ships an unvalidated change.", strings.Join(missing, "\n  "))
	}
}

// TestRegistryHasNoStalePaths catches the reverse: a classification left behind
// after the field it described was renamed or removed. A stale entry is not
// harmless — it makes the map look complete while a real field goes unhashed.
func TestRegistryHasNoStalePaths(t *testing.T) {
	actual := map[string]bool{}
	for _, p := range specPaths() {
		actual[p] = true
	}
	for _, p := range Paths() {
		if !actual[p] {
			t.Errorf("classification map has %q, which is not a field on AgentManifestSpec", p)
		}
	}
}

// TestEveryFieldHasRationale keeps docs/digest.md meaningful: it is generated
// from these strings, and a compliance reader is the audience.
func TestEveryFieldHasRationale(t *testing.T) {
	for _, f := range Registry {
		if strings.TrimSpace(f.Why) == "" {
			t.Errorf("%s: no Why recorded; the classification map is the documentation", f.Path)
		}
	}
}
