// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/waveoffai/waveoff/internal/manifest"
	webhookv1alpha1 "github.com/waveoffai/waveoff/internal/webhook/v1alpha1"
)

// startWebhookServer runs the real webhook against the test API server, using
// the ValidatingWebhookConfiguration from config/webhook.
//
// The unit tests prove the validator's logic. This proves it is wired up: a
// correct validator behind a misconfigured path, rule or failurePolicy admits
// everything, and no unit test would notice.
func startWebhookServer() (stop func(), err error) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  manifest.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    webhookOpts.LocalServingHost,
			Port:    webhookOpts.LocalServingPort,
			CertDir: webhookOpts.LocalServingCertDir,
		}),
	})
	if err != nil {
		return nil, err
	}
	if err := webhookv1alpha1.SetupWithManager(mgr); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(ctx)
	}()

	addr := net.JoinHostPort(webhookOpts.LocalServingHost, fmt.Sprint(webhookOpts.LocalServingPort))
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if dialErr == nil {
			conn.Close()
			return func() { cancel(); <-done }, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done
	return nil, fmt.Errorf("webhook server did not become ready on %s", addr)
}
