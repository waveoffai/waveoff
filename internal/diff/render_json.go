// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"encoding/json"
	"io"
)

// RenderJSON writes the machine-readable form.
//
// The shape is versioned by Result.SchemaVersion and is a supported interface:
// CI systems will branch on it, so fields may be added but never removed or
// repurposed without a version bump.
func RenderJSON(w io.Writer, r *Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if r.Changes == nil {
		r.Changes = []Change{}
	}
	return enc.Encode(r)
}
