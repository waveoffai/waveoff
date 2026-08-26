// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"github.com/gowebpki/jcs"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// Prefix is how many hex characters of behaviorDigest appear in a manifest
// name. 48 bits, not the 28 that a seven-character prefix gives: Git went
// through this expansion under collision pressure and there is no reason to
// start where it started rather than where it ended up.
const Prefix = 12

// ContentPrefix is how many hex characters of contentDigest are appended to
// disambiguate two manifests that share a behaviorDigest — the registry
// migration case, where both objects must exist under different names.
const ContentPrefix = 8

// CanonicalJSON returns the exact bytes that are hashed for the given scope.
//
// Exported because a digest mismatch is otherwise undebuggable: `waveoff verify
// --explain` prints this so an operator can diff the hash inputs rather than
// stare at two unequal hex strings.
func CanonicalJSON(spec *v1alpha1.AgentManifestSpec, scope Scope) ([]byte, error) {
	if err := checkFinite(spec); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(Project(spec, scope))
	if err != nil {
		return nil, fmt.Errorf("marshal %s projection: %w", scope, err)
	}
	// RFC 8785. This is what makes key order, whitespace, unicode escaping and
	// float formatting stop mattering: 1.0 and 1 canonicalise identically, so
	// a round trip through the API server — which decodes integral JSON numbers
	// as int64 and everything else as float64 — cannot move a digest.
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalise %s projection: %w", scope, err)
	}
	return canonical, nil
}

// Compute returns the digest for one scope, as "sha256:<64 hex>".
func Compute(spec *v1alpha1.AgentManifestSpec, scope Scope) (string, error) {
	canonical, err := CanonicalJSON(spec, scope)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Both returns the behaviour and content digests together.
func Both(spec *v1alpha1.AgentManifestSpec) (behavior, content string, err error) {
	if behavior, err = Compute(spec, ScopeBehavior); err != nil {
		return "", "", err
	}
	if content, err = Compute(spec, ScopeContent); err != nil {
		return "", "", err
	}
	return behavior, content, nil
}

// Short returns the first n hex characters of a "sha256:..." digest, for
// display and for name derivation.
func Short(d string, n int) string {
	hexPart := d
	if len(d) > 7 && d[:7] == "sha256:" {
		hexPart = d[7:]
	}
	if len(hexPart) < n {
		return hexPart
	}
	return hexPart[:n]
}

// Name derives the canonical object name: "<agent>-<behaviorDigest[:12]>".
func Name(agent, behaviorDigest string) string {
	return agent + "-" + Short(behaviorDigest, Prefix)
}

// DisambiguatedName is the name to use when the canonical one is already taken
// by a manifest with the same behaviorDigest but different content — a registry
// migration, where the two are the same agent and must both exist.
func DisambiguatedName(agent, behaviorDigest, contentDigest string) string {
	return Name(agent, behaviorDigest) + "." + Short(contentDigest, ContentPrefix)
}

// checkFinite rejects NaN and infinity before they reach the canonicaliser.
// JSON has no representation for them, so json.Marshal would fail with a less
// useful message and RFC 8785 leaves them undefined.
func checkFinite(spec *v1alpha1.AgentManifestSpec) error {
	check := func(path string, v *float64) error {
		if v == nil {
			return nil
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			return fmt.Errorf("%s: %v is not a finite number; JSON cannot represent it", path, *v)
		}
		return nil
	}
	if err := check("spec.model.params.temperature", spec.Model.Params.Temperature); err != nil {
		return err
	}
	if err := check("spec.model.params.topP", spec.Model.Params.TopP); err != nil {
		return err
	}
	for i := range spec.Judges {
		if c := spec.Judges[i].Calibration; c != nil {
			if err := check(fmt.Sprintf("spec.judges[%d].calibration.kappa", i), c.Kappa); err != nil {
				return err
			}
		}
	}
	return nil
}
