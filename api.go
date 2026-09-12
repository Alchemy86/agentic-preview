package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Handler is the whole API. It is deliberately small: a pipeline calls this,
// not a person.
//
//	POST   /previews                                  add one service to a work id,
//	                                                  building the preview too when asked
//	GET    /previews                                  list every work id and its services
//	GET    /previews/{workId}                         one work id's service set
//	DELETE /previews/{workId}                         remove a whole work id
//	DELETE /previews/{workId}/{namespace}/{workload}  remove one service of it
//	GET    /schedules                                 declared schedules and their state
//	POST   /schedules/{name}/override                 force one open or closed now, or back to auto
//	GET    /healthz                                   liveness
//	GET    /readyz                                    ready once a manager session exists
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/previews", s.handlePreviews)
	mux.HandleFunc("/previews/", s.handlePreviewPath)
	mux.HandleFunc("/schedules", s.handleSchedules)
	mux.HandleFunc("/schedules/", s.handleSchedulePath)
	return logging(mux)
}

// handleReady reports one session per allowed namespace, because that is what
// there is to report: this process holds a session in each, and a namespace
// whose session is missing can be intercepted in and never tunnelled to.
//
// Readiness is "at least one namespace has a session", not "all of them do".
// This endpoint is the readiness probe, and failing it takes the pod out of its
// Service - so an all-or-nothing reading would let one namespace's manager
// trouble stop callers reaching the namespaces that are working, which is
// precisely the fault this session-per-namespace design exists to end. The
// namespaces that are NOT connected are named in the body instead, so a partial
// state is loud without being fatal.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	mgr := s.managerVersion
	s.mu.RUnlock()

	live, missing := s.connectedNamespaces()
	sessions := map[string]any{}
	for _, sess := range s.allSessions() {
		sessions[sess.ns] = map[string]any{
			"session":           sess.si.GetSessionId(),
			"sessionCredential": sess.token != "",
		}
	}

	body := map[string]any{
		"connected":              len(live) > 0,
		"manager":                mgr,
		"sessions":               sessions,
		"header":                 s.cfg.headerName,
		"allowedNamespaces":      s.cfg.allowedNamespaces,
		"disconnectedNamespaces": missing,
		"tunnels":                s.agents.status(),
	}
	if len(live) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// handleSchedules reports every declared schedule and what the controller last
// made of it.
//
// The SET of schedules is read-only here on purpose: a schedule is declared in
// config so that it is reviewable and survives a restart, and a window that
// could be CREATED by an HTTP call would be a global intercept anybody could
// raise on a workload nobody asked about. What an already-declared schedule is
// doing right now can be forced - see handleScheduleOverride, which reaches the
// same surface a preview is raised on and no other. The field to watch is
// "problem" - empty is the only good value while "open" is true.
func (s *Server) handleSchedules(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed,
			"use GET: schedules are declared in SCHEDULE_FILE, not raised over the API")
		return
	}
	body := map[string]any{
		"checkInterval": s.cfg.scheduleCheckInterval.String(),
		"schedules":     s.ScheduleStatuses(),
	}
	if s.cfg.schedulePath == "" {
		body["note"] = "no SCHEDULE_FILE is set, so nothing is scheduled and no intercept is raised without a POST /previews"
	} else {
		body["file"] = s.cfg.schedulePath
	}
	writeJSON(w, http.StatusOK, body)
}

// OverrideRequest is the body of POST /schedules/{name}/override.
type OverrideRequest struct {
	// State is "open", "closed", or "auto" to clear the override and let the
	// declared windows decide again.
	State string `json:"state"`
	// Duration is how long the override lasts, as a Go duration. Empty means
	// until it is cleared - which is honest, and reported on every GET
	// /schedules for as long as it lasts.
	Duration string `json:"duration,omitempty"`
	// Reason is a free-text note, never parsed, reported back alongside the
	// override so the next person can see why it is there.
	Reason string `json:"reason,omitempty"`
}

// handleSchedulePath is POST /schedules/{name}/override: force one declared
// schedule open or closed right now, or hand it back to its windows.
//
// It is the same lever a preview has - raise it on demand rather than waiting
// for whatever normally raises it - and it deliberately arrives at the same
// door. This API is reachable through the API server's service proxy, which is
// what `kubectl agentic-preview` uses and what the cluster's own RBAC guards;
// putting the override anywhere else would give a global intercept a second,
// laxer way to be flipped, and the property that it cannot be is the reason the
// schedules page says what it says.
//
// It overrides only the ANSWER to "should this be open now". The reconcile loop
// is untouched: it still opens, health-checks, drift-checks, re-raises and
// alarms exactly as it does for a window edge.
func (s *Server) handleSchedulePath(w http.ResponseWriter, req *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(req.URL.Path, "/schedules/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "override" {
		writeErr(w, http.StatusBadRequest,
			"use POST /schedules/{name}/override, or GET /schedules to read them all")
		return
	}
	name := parts[0]

	switch req.Method {
	case http.MethodDelete:
		s.handleScheduleOverride(w, name, OverrideRequest{State: overrideAuto})
	case http.MethodPost:
		var body OverrideRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		s.handleScheduleOverride(w, name, body)
	default:
		writeErr(w, http.StatusMethodNotAllowed,
			`use POST /schedules/{name}/override with {"state":"open"|"closed"|"auto"}, or DELETE to clear it`)
	}
}

func (s *Server) handleScheduleOverride(w http.ResponseWriter, name string, body OverrideRequest) {
	var d time.Duration
	if v := strings.TrimSpace(body.Duration); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("duration %q: %v", body.Duration, err))
			return
		}
		d = parsed
	}

	ov, err := s.SetOverride(name, body.State, body.Reason, d, time.Now())
	if err != nil {
		code := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "no schedule is named") {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}

	out := map[string]any{
		"schedule": name,
		// The override changes what the NEXT tick reconciles to, so the change
		// is visible within one check interval rather than instantly. Saying so
		// is the difference between a caller that waits and one that retries.
		"appliesWithin": s.cfg.scheduleCheckInterval.String(),
	}
	if ov == nil {
		out["override"] = nil
		out["note"] = "override cleared; the schedule's declared windows decide again"
	} else {
		out["override"] = ov
		out["note"] = "the reconcile loop still health-checks, drift-checks and re-applies this schedule as it always does"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePreviews(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"header": s.cfg.headerName,
			"work":   s.reg.works(),
		})
	case http.MethodPost:
		s.handleAdd(w, req)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// handleAdd raises one service under one work id. Calling it again with the
// same work id adds to that id's set; calling it again with the same work id
// AND service refreshes that one entry. It never replaces the set.
func (s *Server) handleAdd(w http.ResponseWriter, req *http.Request) {
	var pr PreviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<16)).Decode(&pr); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("bad JSON: %v", err))
		return
	}
	p, err := pr.validate(s.cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// A schedule's intercept is registered under the schedule's name as its
	// work id, so a POST using that name would land on the same registry key
	// and the two would take turns evicting each other.
	if s.cfg.scheduledNames()[p.WorkID] {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"workId %q is the name of a declared scheduled intercept and is reserved; use a different work id", p.WorkID))
		return
	}

	// The other half of the refusal in registry.conflictingMode. A workload
	// carrying a live SCHEDULED (headerless) intercept has its traffic-agent
	// port on the raw TCP listener; a header-keyed preview raised alongside it
	// would be created, reported ACTIVE, and match nothing, because the global
	// intercept is the one that listener serves. Saying so beats a preview that
	// appears to exist and never receives a request.
	if other := s.reg.conflictingMode(p); other != nil {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"%s.%s is currently held by the scheduled global intercept %q (schedule %s), which diverts ALL traffic to that port. "+
				"A workload runs in ONE intercept mode at a time, so a header-keyed preview raised now would match nothing. "+
				"Wait for that schedule's window to close, or take the workload off the schedule",
			p.Workload, p.Namespace, other.Name, other.Schedule))
		return
	}

	// The session for THIS preview's namespace. Another namespace being
	// connected says nothing about whether this one can be intercepted in.
	if s.sessionFor(p.Namespace) == nil {
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Sprintf("no manager session for namespace %s yet", p.Namespace))
		return
	}

	// Build the preview before anything is decided about the intercept.
	//
	// It runs BEFORE the idempotency check below rather than after, because it
	// is what fills in the port the intercept forwards to - read off the
	// Service it just built - and because it is itself idempotent: a pipeline
	// retry reconciles the same two objects, and a work id re-raised with a
	// newer tag rolls the Deployment forward in place.
	if p.Create != nil {
		if err := s.createWorkload(req.Context(), p); err != nil {
			var re requestError
			if errors.As(err, &re) {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}

	// Idempotent by design: a pipeline retries, and re-POSTing a service that
	// is already previewed under this work id must not disturb it. Raising it
	// again would collide with our own live intercept on identical header
	// filters, which is a pointless way to fail a retry.
	if prev, existed := s.reg.get(p.key()); existed {
		sameTarget := prev.TargetService == p.TargetService &&
			prev.TargetPort == p.TargetPort && prev.PortID == p.PortID

		if sameTarget && prev.Image == p.Image {
			s.extend(p.WorkID)
			work, _ := s.reg.work(p.WorkID)
			writeJSON(w, http.StatusOK, map[string]any{
				"unchanged": prev,
				"work":      work,
			})
			return
		}

		// The image moved but the intercept did not: a new commit under the
		// same work id. createWorkload has already rolled the Deployment
		// forward in place, and the intercept still points at the same Service
		// on the same port - so tearing it down and raising it again would be
		// a gap in routing for no gain. Carry the live intercept's state onto
		// the new record and say plainly that it rolled rather than that
		// nothing happened.
		if sameTarget {
			p.Disposition, p.Message = prev.Disposition, prev.Message
			p.AgentPods, p.AgentPodsReported = prev.AgentPods, prev.AgentPodsReported
			if err := s.reg.add(p); err != nil {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
			s.extend(p.WorkID)
			work, _ := s.reg.work(p.WorkID)
			logf("%s rolled forward to %s", p.Name, p.Image)
			writeJSON(w, http.StatusOK, map[string]any{
				"rolled": p,
				"work":   work,
			})
			return
		}
		// Same service under the same work id, but pointed somewhere new: the
		// old intercept has to go before the new one can be raised.
		logf("%s now targets %s:%d (was %s:%d); replacing its intercept",
			prev.Name, p.TargetService, p.TargetPort, prev.TargetService, prev.TargetPort)
		s.reg.removeService(prev.key())
		if err := s.remove(req.Context(), prev); err != nil {
			logf("replacing %s: %v", prev.Name, err)
		}
	}

	// Registered before it is raised, so that a session lost mid-raise still
	// leaves the reconciler something to re-raise.
	if err := s.reg.add(p); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.raise(req.Context(), p); err != nil {
		s.reg.removeService(p.key())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	s.extend(p.WorkID)
	work, _ := s.reg.work(p.WorkID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"raised": p,
		"work":   work,
	})
}

// extend pushes every preview under a work id out to a fresh lifetime. Any
// contact with an id counts - re-raising a service, or adding another one -
// because a work id is one change and its services are used together.
func (s *Server) extend(workID string) {
	if s.cfg.lifetime <= 0 {
		return
	}
	s.reg.touch(workID, time.Now().Add(s.cfg.lifetime))
}

func (s *Server) handlePreviewPath(w http.ResponseWriter, req *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(req.URL.Path, "/previews/"), "/")
	if rest == "" {
		writeErr(w, http.StatusBadRequest, "a work id is required")
		return
	}
	parts := strings.Split(rest, "/")

	switch {
	case len(parts) == 1 && req.Method == http.MethodGet:
		work, ok := s.reg.work(parts[0])
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("work id %q has nothing live", parts[0]))
			return
		}
		writeJSON(w, http.StatusOK, work)

	case len(parts) == 1 && req.Method == http.MethodDelete:
		s.handleRemoveWork(w, req, parts[0])

	case len(parts) == 3 && req.Method == http.MethodDelete:
		s.handleRemoveService(w, req, serviceKey{WorkID: parts[0], Namespace: parts[1], Workload: parts[2]})

	default:
		writeErr(w, http.StatusBadRequest,
			"use GET|DELETE /previews/{workId} or DELETE /previews/{workId}/{namespace}/{workload}")
	}
}

// handleRemoveWork removes every service previewed under a work id, and every
// object agentic-preview built for it.
//
// Teardown is EXPLICIT and this is the only thing that asks for it, apart from
// the expiry sweep behind it. Nothing removes a preview because a pull request
// merged, closed or changed state: the change may still be being tested against
// the preview after it merges, and this service has no opinion about pull
// requests in any case.
func (s *Server) handleRemoveWork(w http.ResponseWriter, req *http.Request, workID string) {
	if s.cfg.scheduledNames()[workID] {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"%q is a scheduled intercept, not a work id: it goes when its window closes and the controller would raise it again. "+
				"Remove its window from SCHEDULE_FILE to stop it", workID))
		return
	}
	removed, deleted, problems, found := s.tearDownWork(req.Context(), workID)
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("work id %q has nothing live", workID))
		return
	}
	body := map[string]any{"workId": workID, "removed": removed}
	if len(deleted) > 0 {
		body["deleted"] = deleted
	}
	if problems != nil {
		body["problems"] = problems
	}
	writeJSON(w, http.StatusOK, body)
}

// handleRemoveService removes one service from a work id while the rest of the
// change stays up - one repository's part of it is finished with, the others
// are not.
func (s *Server) handleRemoveService(w http.ResponseWriter, req *http.Request, k serviceKey) {
	if s.cfg.scheduledNames()[k.WorkID] {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"%q is a scheduled intercept, not a work id: it goes when its window closes and the controller would raise it again. "+
				"Remove its window from SCHEDULE_FILE to stop it", k.WorkID))
		return
	}
	p, ok := s.reg.removeService(k)
	deleted, problems := s.dropWorkload(req.Context(), k.WorkID, k.Workload)
	if !ok && len(deleted) == 0 {
		writeErr(w, http.StatusNotFound,
			fmt.Sprintf("work id %q is not previewing %s.%s", k.WorkID, k.Workload, k.Namespace))
		return
	}
	body := map[string]any{"workId": k.WorkID, "removed": k.Workload + "." + k.Namespace}
	if ok {
		if err := s.remove(req.Context(), p); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(deleted) > 0 {
		body["deleted"] = deleted
	}
	if problems != nil {
		body["problems"] = problems
	}
	if work, stillLive := s.reg.work(k.WorkID); stillLive {
		body["work"] = work
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/healthz" && req.URL.Path != "/readyz" {
			logf("%s %s from %s", req.Method, req.URL.Path, req.RemoteAddr)
		}
		next.ServeHTTP(w, req)
	})
}
