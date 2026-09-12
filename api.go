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
//	GET    /schedules                                 declared scheduled intercepts and their state
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

// handleSchedules reports every declared scheduled intercept and what the
// controller last made of it.
//
// It is read-only on purpose: a schedule is declared in config so that it is
// reviewable and survives a restart, and a window that could be opened by an
// HTTP call would be a global intercept anybody could raise on a workload
// nobody asked about. The field to watch is "problem" - empty is the only good
// value while "open" is true.
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
