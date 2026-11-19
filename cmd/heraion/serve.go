package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/hooks"
	"github.com/muskats/heraion/internal/playbook"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", envOr("HERAION_CONFIG", "/etc/heraion/config.yaml"), "config file (env HERAION_CONFIG)")
	level := fs.String("log-level", envOr("HERAION_LOG_LEVEL", "info"), "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	red := guardrails.NewRedactor(cfg.Secrets()...)
	log := newLogger(*level, red)

	pbs, err := playbook.LoadDir(cfg.PlaybooksDir)
	if err != nil {
		return err
	}
	if cfg.Gateway.JobTemplate == "" {
		return errors.New("gateway.jobTemplate is required")
	}
	tmpl, err := dispatch.LoadTemplate(cfg.Gateway.JobTemplate)
	if err != nil {
		return err
	}
	kc, err := kubeClient()
	if err != nil {
		return err
	}
	ns, err := namespace(cfg.Gateway.Namespace)
	if err != nil {
		return err
	}
	d := &dispatch.Dispatcher{Client: kc, Namespace: ns, Template: tmpl, MaxActive: cfg.Gateway.MaxConcurrentJobs}
	srv, err := hooks.New(cfg.Gateway, pbs, d, log)
	if err != nil {
		return err
	}

	hs := &http.Server{
		Addr:              cfg.Gateway.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("gateway listening", "addr", cfg.Gateway.Listen, "namespace", ns, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

// kubeClient uses the pod's service account, or KUBECONFIG when run locally.
func kubeClient() (kubernetes.Interface, error) {
	rc, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes client config: %w", err)
	}
	rc.UserAgent = "heraion-gateway/" + version
	return kubernetes.NewForConfig(rc)
}

func namespace(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns, nil
	}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", errors.New("cannot determine namespace: set gateway.namespace or POD_NAMESPACE")
	}
	return strings.TrimSpace(string(b)), nil
}
