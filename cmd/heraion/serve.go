package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/hooks"
	"github.com/muskats/heraion/internal/playbook"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	level := logLevelFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, log, err := load(*cfgPath, *level)
	if err != nil {
		return err
	}

	pbs, err := playbook.LoadDir(cfg.PlaybooksDir)
	if err == nil {
		err = playbook.ValidateAll(pbs, cfg.Mode, cfg.Repos)
	}
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
	kc, ns, err := kubeClient(cfg.Gateway.Namespace)
	if err != nil {
		return err
	}
	d := &dispatch.Dispatcher{Client: kc, Namespace: ns, Template: tmpl, MaxActive: cfg.Gateway.MaxConcurrentJobs, Log: log}
	srv, err := hooks.New(cfg.Gateway, pbs, d, log)
	if err != nil {
		return err
	}

	servers := []*http.Server{newHTTPServer(cfg.Gateway.Listen, srv.Handler())}
	if addr := cfg.Gateway.MetricsListen; addr != "" {
		reg := prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		reg.MustRegister(srv.Collectors()...)
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		servers = append(servers, newHTTPServer(addr, mux))
	}
	errc := make(chan error, len(servers))
	for _, hs := range servers {
		go func() { errc <- hs.ListenAndServe() }()
	}
	log.Info("gateway listening", "addr", cfg.Gateway.Listen, "metrics", cfg.Gateway.MetricsListen, "namespace", ns, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var errs []error
	for _, hs := range servers {
		errs = append(errs, hs.Shutdown(sctx))
	}
	return errors.Join(errs...)
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// kubeClient uses KUBECONFIG or ~/.kube/config when present, else the pod's
// service account. The namespace is the configured one, else the kubeconfig
// context's, else POD_NAMESPACE or the service account's.
func kubeClient(namespace string) (kubernetes.Interface, string, error) {
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	rc, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes client config: %w", err)
	}
	if namespace == "" {
		if namespace, _, err = cc.Namespace(); err != nil {
			return nil, "", fmt.Errorf("kubernetes namespace: %w", err)
		}
	}
	rc.UserAgent = "heraion-gateway/" + version
	// A slow API server fails the webhook with a 500, which the sender
	// retries, instead of holding the request open.
	rc.Timeout = 15 * time.Second
	kc, err := kubernetes.NewForConfig(rc)
	return kc, namespace, err
}
