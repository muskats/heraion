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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	d := &dispatch.Dispatcher{Client: kc, Namespace: ns, Template: tmpl, MaxActive: cfg.Gateway.MaxConcurrentJobs, Log: log}
	srv, err := hooks.New(cfg.Gateway, pbs, d, log)
	if err != nil {
		return err
	}

	servers := []*http.Server{newHTTPServer(cfg.Gateway.Listen, srv.Handler())}
	if addr := cfg.Gateway.MetricsListen; addr != "" {
		reg := prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), d.Metrics())
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
