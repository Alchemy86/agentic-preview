package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const defaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // path, not a credential

// config is the service's whole configuration. Everything comes from the
// environment so that the Deployment manifest is the single place it is set.
type config struct {
	// managerAddr is the traffic-manager's gRPC address. Plain h2c: the
	// manager terminates no TLS on this port. Required: the namespace the
	// traffic-manager was installed into varies by installation, and a
	// default pointing at the wrong one fails as an unhelpful dial timeout.
	managerAddr string

	// listenAddr is where the HTTP API listens.
	listenAddr string

	// tokenPath holds the ServiceAccount token presented to the manager as
	// "authorization: bearer <token>". Read fresh on every call because a
	// projected token is rotated in place by the kubelet.
	tokenPath string

	// clientName is the ClientInfo.Name the manager records for our session.
	clientName string

	// headerName is the single header every preview is routed on; its VALUE
	// is the work id. It is service-level, not per-request, on purpose: two
	// services of one work id reached by different header names would defeat
	// the point of a work id, which is that one header reaches all of them.
	headerName string

	// allowedNamespaces bounds which namespaces a caller may intercept in.
	// The RBAC on attachments.telepresence.io is the real boundary; this is
	// the same boundary stated locally so a request is refused with a clear
	// message instead of a gRPC permission error. Required: no default,
	// because an empty list read as "everything" is the wrong failure.
	allowedNamespaces []string

	// statePath records the current session id so that a restarted process can
	// depart its predecessor's session. See sweepPreviousSession.
	statePath string

	// remainInterval is how often Remain is called to hold the session open.
	remainInterval time.Duration

	// reconnectBackoff is the pause before rebuilding a dead session.
	reconnectBackoff time.Duration

	// agentReconcile is how often the known agent-pod set is re-reconciled
	// even without a new snapshot, so a dial loop that ended on its own is
	// re-established. The manager's own node-agent resync is 30s.
	agentReconcile time.Duration
}

func loadConfig() (*config, error) {
	c := &config{
		managerAddr:      strings.TrimSpace(os.Getenv("MANAGER_ADDR")),
		listenAddr:       env("LISTEN_ADDR", ":8080"),
		tokenPath:        env("TOKEN_FILE", defaultTokenPath),
		clientName:       env("CLIENT_NAME", "agentic-preview"),
		headerName:       strings.ToLower(env("HEADER_NAME", "x-preview")),
		statePath:        env("STATE_FILE", "/var/lib/agentic-preview/session"),
		remainInterval:   envDuration("REMAIN_INTERVAL", 20*time.Second),
		reconnectBackoff: envDuration("RECONNECT_BACKOFF", 5*time.Second),
		agentReconcile:   envDuration("AGENT_RECONCILE_INTERVAL", 10*time.Second),
	}

	if c.managerAddr == "" {
		return nil, fmt.Errorf("MANAGER_ADDR is required: the traffic-manager's gRPC address, " +
			"traffic-manager.<its namespace>.svc.cluster.local:8081")
	}

	for _, ns := range strings.Split(os.Getenv("ALLOWED_NAMESPACES"), ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			c.allowedNamespaces = append(c.allowedNamespaces, ns)
		}
	}
	if len(c.allowedNamespaces) == 0 {
		return nil, fmt.Errorf("ALLOWED_NAMESPACES is required and must list at least one namespace")
	}
	return c, nil
}

// namespaceAllowed reports whether ns is one agentic-preview may intercept in.
func (c *config) namespaceAllowed(ns string) bool {
	for _, a := range c.allowedNamespaces {
		if a == ns {
			return true
		}
	}
	return false
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
