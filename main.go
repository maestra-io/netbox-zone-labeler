// Command netbox-zone-labeler sets topology.kubernetes.io/zone on every node
// from the NetBox rack of the device (or of the VM's host device).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/maestra-io/netbox-zone-labeler/internal/config"
	"github.com/maestra-io/netbox-zone-labeler/internal/labeler"
	"github.com/maestra-io/netbox-zone-labeler/internal/netbox"
)

func main() {
	if err := run(); err != nil {
		slog.Error("netbox-zone-labeler failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	level := new(slog.LevelVar)
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	cfg, err := config.FromEnv(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	level.Set(cfg.LogLevel)
	log.Info("starting",
		"netbox_url", cfg.NetBoxURL,
		"reconcile_period", cfg.ReconcilePeriod.String(),
		"exclude_roles", cfg.ExcludeRoles,
		"dry_run", cfg.DryRun,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	restCfg, err := kubeConfig(cfg.KubeContext)
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	restCfg.UserAgent = "netbox-zone-labeler"
	client, err := metadata.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	nb := netbox.NewClient(cfg.NetBoxURL, cfg.NetBoxToken, netbox.WithTimeout(cfg.NetBoxTimeout))
	waitForNetBox(ctx, nb, cfg.NetBoxWait, log)

	ctrl, err := labeler.New(labeler.Options{
		Client:       client,
		Lookup:       nb,
		ExcludeRoles: cfg.ExcludeRoles,
		Period:       cfg.ReconcilePeriod,
		DryRun:       cfg.DryRun,
		Registerer:   prometheus.DefaultRegisterer,
		Logger:       log,
	})
	if err != nil {
		return err
	}

	srv := newServer(cfg.ListenAddr, ctrl)
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("http server: %w", err)
		}
		close(serveErr)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	runErr := make(chan error, 1)
	go func() { runErr <- ctrl.Run(ctx) }()

	var result error
	select {
	case result = <-serveErr:
		if result != nil {
			stop()
			<-runErr
			return result
		}
		result = <-runErr
	case result = <-runErr:
	}
	// A signal during start-up (e.g. while waiting for NetBox) cancels ctx
	// before the informer has synced; that is a clean shutdown, not a failure.
	if ctx.Err() != nil {
		return nil
	}
	return result
}

// kubeConfig prefers the in-cluster config and falls back to KUBECONFIG /
// ~/.kube/config (optionally a named context) for local runs.
func kubeConfig(kubeContext string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
}

// waitForNetBox blocks for at most budget until NetBox answers. It gives up
// with a warning rather than an error: lookups retry with backoff anyway,
// and refusing to start would only hide the label state from the informer.
func waitForNetBox(ctx context.Context, nb *netbox.Client, budget time.Duration, log *slog.Logger) {
	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	err := wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := nb.Ping(ctx); err != nil {
			log.Info("waiting for netbox", "error", err)
			return false, nil
		}
		return true, nil
	})
	switch {
	case err == nil:
		log.Info("netbox reachable")
	case ctx.Err() != nil:
	default:
		log.Warn("netbox not reachable at start-up, continuing", "waited", budget.String())
	}
}

func newServer(addr string, ctrl *labeler.Controller) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", probe(ctrl.Healthy, "full pass overdue"))
	mux.HandleFunc("/readyz", probe(ctrl.Ready, "informer not synced"))
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func probe(ok func() bool, failure string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if ok() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		http.Error(w, failure, http.StatusServiceUnavailable)
	}
}
