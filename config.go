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

	// lifetime is how long a preview lives without being touched. It is a
	// safety net against previews accumulating forever, and NOT a policy about
	// pull requests: nothing here removes a preview because a PR merged,
	// closed or changed state. Anything that touches a work id - raising a
	// service under it again, adding another service to it - extends every
	// preview in that id.
	//
	// It defaults to 24 hours because that is the safe answer for somebody
	// installing this with nobody minding their cluster for them. It can be
	// switched off - "off", "never" or "0" - and an adopter who has something
	// else responsible for cleaning up should switch it off, because a timer
	// that removes a preview while somebody is still testing against it is
	// worse than a forgotten pod. Off is deliberately an explicit choice:
	// someone who never reads this gets the timer.
	lifetime time.Duration

	// reapInterval is how often expired previews are swept.
	reapInterval time.Duration

	// readyTimeout is how long a create waits for the preview's pods to come
	// up before giving up and reporting why. Zero means do not wait, which
	// trades the two loudest failure modes - a tag that does not exist, and no
	// credentials to pull it - for a faster POST.
	readyTimeout time.Duration

	// agentReconcile is how often the known agent-pod set is re-reconciled
	// even without a new snapshot, so a dial loop that ended on its own is
	// re-established. The manager's own node-agent resync is 30s.
	agentReconcile time.Duration

	// schedulePath is the file declaring scheduled, headerless intercepts.
	// Unset - which is the normal case - means there are none and nothing in
	// schedule.go is reached.
	schedulePath string

	// schedules are the compiled contents of schedulePath. Compiled at startup
	// and never reloaded: a window that opens at 18:32 must have been proved
	// parseable at 09:00, not discovered to be unparseable at 18:32.
	schedules []*schedule

	// scheduleCheckInterval is how often the schedule controller reconciles,
	// which is BOTH how promptly a window opens or closes and how quickly a
	// scheduled intercept that has died is noticed.
	//
	// It is 30s rather than minutes because of what a dead GLOBAL intercept
	// costs: not one header hanging, but every request to the workload, with
	// nobody awake to notice. The existing safety nets are hours (PREVIEW_
	// LIFETIME) or demand-driven (the conflict orphan sweep), so this interval
	// is the real bound on that exposure. Raising it trades traffic held for
	// gRPC calls saved; the calls are one GetIntercept per schedule.
	scheduleCheckInterval time.Duration
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
		readyTimeout:     envDuration("PREVIEW_READY_TIMEOUT", 120*time.Second),
		lifetime:         envLifetime("PREVIEW_LIFETIME", 24*time.Hour),
		reapInterval:     envDuration("PREVIEW_REAP_INTERVAL", time.Minute),

		schedulePath:          strings.TrimSpace(os.Getenv("SCHEDULE_FILE")),
		scheduleCheckInterval: envDuration("SCHEDULE_CHECK_INTERVAL", 30*time.Second),
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

	// Schedules are compiled last because compiling one checks its namespaces
	// against the allow-list above, and FATALLY because the alternative is a
	// window that silently never opens. Nobody is watching a 03:00 window; a
	// pod that will not start is noticed, and a schedule that quietly did not
	// load is not.
	if c.schedulePath != "" {
		scheds, err := loadSchedules(c.schedulePath, c)
		if err != nil {
			return nil, err
		}
		c.schedules = scheds
	}

	if c.scheduleCheckInterval <= 0 {
		return nil, fmt.Errorf("SCHEDULE_CHECK_INTERVAL must be positive")
	}
	return c, nil
}

// scheduledNames is the set of work ids reserved by schedules. A POST /previews
// may not use one: it would land on the same registry key as the schedule's own
// entry and the two would take turns removing each other.
func (c *config) scheduledNames() map[string]bool {
	out := make(map[string]bool, len(c.schedules))
	for _, s := range c.schedules {
		out[s.spec.Name] = true
	}
	return out
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

// envLifetime reads a preview lifetime, accepting "off" and "never" alongside a
// duration and a bare 0. It is spelled out rather than left as "set it to zero"
// because turning expiry off is a decision somebody should be able to read back
// off the manifest and understand.
func envLifetime(key string, def time.Duration) time.Duration {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v {
	case "":
		return def
	case "off", "never", "none", "0":
		return 0
	default:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		return def
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
