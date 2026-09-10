// agentic-preview raises header-routed previews by talking to the telepresence
// traffic-manager's gRPC API directly. No telepresence CLI, no connector
// daemon, no TUN device, no root, no laptop: it runs as an ordinary Deployment
// and a pipeline drives it over HTTP.
//
// One call does the whole job. Given an image reference it BUILDS the preview -
// by copying the live Deployment, image swapped, so that everything the pod
// needs to run comes across rather than being invented - creates a Service in
// front of it, and then routes the header to it. See workload.go.
//
// It is long-lived by necessity rather than by choice. InterceptSpec.target_host
// reads like an address the traffic-agent connects to, but the agent never
// dials it: for each intercepted request it opens a tunnel back to the CLIENT
// session and sends the address down it, and the dial happens at the client's
// end. So the destination of a preview is not a value handed to the manager -
// it is a process holding a session and answering dial requests. Put that
// process in the cluster and a plain net.Dial reaches any ClusterIP.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func logf(format string, a ...any) {
	fmt.Printf("%s  %s\n", time.Now().UTC().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, a...))
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		logf("FATAL %v", err)
		os.Exit(1)
	}

	logf("agentic-preview starting: manager=%s header=%s namespaces=%v",
		cfg.managerAddr, cfg.headerName, cfg.allowedNamespaces)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The session loop gets its OWN context, cancelled only after cleanup has
	// finished. If it shared the signal context, runSession would return the
	// instant SIGTERM arrived and its deferred conn.Close() would race the
	// teardown below - which is exactly how a graceful stop ends up leaving
	// some intercepts behind and the header they answer hanging.
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	r := NewServer(cfg)

	// Tidy up after a previous incarnation before arriving ourselves.
	r.SweepPreviousSession(sigCtx)

	go r.Run(runCtx)

	// The expiry sweep. It is a safety net against forgotten previews and not
	// a teardown policy: previews go when somebody asks, or when nothing has
	// touched them for PREVIEW_LIFETIME.
	go r.ReapExpired(runCtx)

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-sigCtx.Done()
		logf("signal received, tearing down")

		// Remove every intercept and depart the session while the session is
		// still live. This is not housekeeping: an intercept left in manager
		// state with nobody holding its tunnel makes its own header HANG, not
		// fall back to the app - the agent waits for a dial watcher on an
		// unbounded 20ms retry. Removing them is what makes a stop safe.
		tctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		r.Shutdown(tctx)

		// Only now let the session loop go.
		cancelRun()

		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()

	logf("API listening on %s", cfg.listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logf("FATAL http server: %v", err)
		os.Exit(1)
	}
	logf("stopped")
}
