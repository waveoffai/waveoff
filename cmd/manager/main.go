// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Command manager runs the Waveoff admission webhook.
//
// It hosts two webhooks and the rollout controller.
//
// The validating webhook refuses an AgentManifest that does not match its own
// digests. The mutating webhook injects the recorder sidecar into pods that ask
// for it. The controller runs AgentRollouts.
//
// There is deliberately no controller for AgentManifest: it is immutable, so
// there is nothing to reconcile — the object's whole job is to be an accurate,
// verified record of what an agent was.
package main

import (
	"crypto/tls"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	waveoffv1alpha1 "github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/rollout"
	"github.com/waveoffai/waveoff/internal/webhook/inject"
	webhookv1alpha1 "github.com/waveoffai/waveoff/internal/webhook/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(waveoffv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr   string
		probeAddr     string
		certDir       string
		webhookPort   int
		recorderImage string
		corpusRoot    string
		outputRoot    string
		blobDir       string
		modelUpstream string
		stageTimeout  time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "address the metrics endpoint binds to ('0' disables it)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	flag.StringVar(&certDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "directory holding the serving certificate")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "port the webhook server binds to")
	flag.StringVar(&recorderImage, "recorder-image", "ghcr.io/waveoffai/waveoff-recorder:latest",
		"image injected as the recorder sidecar")
	flag.StringVar(&corpusRoot, "corpus-root", "/var/lib/waveoff/corpus", "where recorded corpora live")
	flag.StringVar(&outputRoot, "output-root", "/var/lib/waveoff/replays", "where replay outputs are written")
	flag.StringVar(&blobDir, "blob-dir", "/var/lib/waveoff/blobs", "content-addressed blob store")
	flag.StringVar(&modelUpstream, "model-upstream", "",
		"model provider replays run against; required to reconcile rollouts")
	flag.DurationVar(&stageTimeout, "stage-timeout", 30*time.Minute, "how long one rollout stage may take")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: certDir,
			TLSOpts: []func(*tls.Config){func(c *tls.Config) { c.MinVersion = tls.VersionTLS12 }},
		}),
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := webhookv1alpha1.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register the AgentManifest webhook")
		os.Exit(1)
	}
	if err := inject.SetupWithManager(mgr, recorderImage); err != nil {
		setupLog.Error(err, "unable to register the sidecar injection webhook")
		os.Exit(1)
	}

	// Rollouts are only reconciled when the manager has somewhere to send the
	// model traffic a replay generates. Starting the controller without one
	// would accept AgentRollouts and hold every single of them, which looks
	// like a broken gate rather than an unconfigured one.
	if modelUpstream == "" {
		setupLog.Info("not reconciling AgentRollouts: -model-upstream is unset, " +
			"and an offline replay runs the model live against the candidate's prompts")
	} else {
		// Observations for live and shadow stages are not wired up yet: where
		// they come from is a deployment decision, and a controller that
		// invented a source would be measuring something nobody chose. Those
		// stages hold and say so; offline replay works.
		if err := (&rollout.Reconciler{
			Client:        mgr.GetClient(),
			CorpusRoot:    corpusRoot,
			OutputRoot:    outputRoot,
			BlobDir:       blobDir,
			ModelUpstream: modelUpstream,
			StageTimeout:  stageTimeout,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to register the rollout controller")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// Readiness gates on the serving certificate being present. The webhook
	// runs with failurePolicy: Fail, so a replica that reports ready before it
	// can serve TLS would reject every manifest in the cluster.
	if err := mgr.AddReadyzCheck("readyz", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to set up readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting webhook server")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
