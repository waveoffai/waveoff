// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package manifest resolves the things a user can name on the command line
// into AgentManifest objects.
package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// Scheme is the scheme every cluster client in the CLI is built from.
//
// It carries the built-in kinds as well as the waveoff.ai ones, because
// `waveoff pin` reads Deployments, Pods and ConfigMaps. A scheme holding only
// the waveoff types produces a client that fails at the first Get with "no kind
// is registered", which is a runtime failure in a code path no unit test using
// a hand-built fake scheme will ever reach.
var Scheme = runtime.NewScheme()

func init() {
	for _, add := range []func(*runtime.Scheme) error{
		v1alpha1.AddToScheme,
		corev1.AddToScheme,
		appsv1.AddToScheme,
	} {
		if err := add(Scheme); err != nil {
			panic(err)
		}
	}
}

// ErrNoCluster reports that a reference needed a cluster and there was not one.
var ErrNoCluster = errors.New("no reachable cluster")

// Ref is a parsed command-line reference to a manifest.
type Ref struct {
	// Raw is what the user typed, for error messages.
	Raw string
	// Path is set when the reference names a file, or "-" for stdin.
	Path string
	// Name selects one document from a multi-document file, or names an object
	// in the cluster.
	Name string
	// Namespace overrides the ambient namespace for a cluster lookup.
	Namespace string
}

// IsFile reports whether resolving this reference needs the filesystem rather
// than a cluster.
func (r Ref) IsFile() bool { return r.Path != "" }

// ParseRef interprets one command-line argument.
//
//	manifest.yaml          a file
//	manifest.yaml#name     one document out of a multi-document file
//	-                      stdin
//	support-agent-7f3a9c2  an object in the cluster
//
// A path is recognised by existing on disk, or by looking unambiguously like a
// path. Guessing wrong in the cluster direction produces a confusing "not
// found" for what was a typo in a filename, so existence is checked first.
func ParseRef(arg, namespace string) Ref {
	r := Ref{Raw: arg, Namespace: namespace}
	if arg == "-" {
		r.Path = "-"
		return r
	}
	path, sel, hasSel := strings.Cut(arg, "#")
	if hasSel {
		r.Name = sel
	}
	if _, err := os.Stat(path); err == nil {
		r.Path = path
		return r
	}
	if hasSel || strings.ContainsAny(path, "/\\") || strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		// Looks like a path, but is not on disk. Say so rather than searching
		// the cluster for an object named "./prompts/manifest.yaml".
		r.Path = path
		return r
	}
	r.Name = arg
	return r
}

// Load resolves a reference. The cluster client is created lazily, so a diff
// between two local files works with no kubeconfig at all.
func Load(ctx context.Context, r Ref) (*v1alpha1.AgentManifest, error) {
	if r.IsFile() {
		return loadFile(r)
	}
	return loadCluster(ctx, r)
}

func loadFile(r Ref) (*v1alpha1.AgentManifest, error) {
	var (
		in  io.Reader
		err error
	)
	if r.Path == "-" {
		in = os.Stdin
	} else {
		f, ferr := os.Open(r.Path)
		if ferr != nil {
			return nil, fmt.Errorf("read %s: %w", r.Path, ferr)
		}
		defer f.Close()
		in = f
	}

	docs, err := ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.Raw, err)
	}
	switch {
	case len(docs) == 0:
		return nil, fmt.Errorf("%s: contains no AgentManifest", r.Raw)
	case r.Name != "":
		for _, m := range docs {
			if m.Name == r.Name {
				return m, nil
			}
		}
		names := make([]string, 0, len(docs))
		for _, m := range docs {
			names = append(names, m.Name)
		}
		return nil, fmt.Errorf("%s: no manifest named %q in %s (found: %s)",
			r.Raw, r.Name, r.Path, strings.Join(names, ", "))
	case len(docs) > 1:
		names := make([]string, 0, len(docs))
		for _, m := range docs {
			names = append(names, m.Name)
		}
		return nil, fmt.Errorf("%s holds %d manifests; select one with %s#<name> (found: %s)",
			r.Path, len(docs), r.Path, strings.Join(names, ", "))
	}
	return docs[0], nil
}

// ReadAll decodes every AgentManifest in a YAML stream, ignoring other kinds so
// a manifest can live in a file alongside the rest of an application's config.
func ReadAll(in io.Reader) ([]*v1alpha1.AgentManifest, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(in, 4096)
	var out []*v1alpha1.AgentManifest
	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		if kind, _ := raw["kind"].(string); kind != "AgentManifest" {
			continue
		}
		m := &v1alpha1.AgentManifest{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, m); err != nil {
			return nil, fmt.Errorf("decode AgentManifest: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

func loadCluster(ctx context.Context, r Ref) (*v1alpha1.AgentManifest, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("%w: %v (name %q was read as a cluster object; "+
			"pass a path if you meant a file)", ErrNoCluster, err, r.Name)
	}
	c, err := client.New(cfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoCluster, err)
	}
	ns := r.Namespace
	if ns == "" {
		ns = "default"
	}
	m := &v1alpha1.AgentManifest{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: r.Name}, m); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("no AgentManifest %q in namespace %q", r.Name, ns)
		}
		return nil, fmt.Errorf("get AgentManifest %q: %w", r.Name, err)
	}
	return m, nil
}
