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
	return logging(mux)
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	connected, mgr := s.connected, s.managerVersion
	session := ""
	if s.session != nil {
		session = s.session.GetSessionId()
	}
	verified := s.sessionToken != ""
	s.mu.RUnlock()

	body := map[string]any{
		"connected":         connected,
		"manager":           mgr,
		"session":           session,
		"sessionCredential": verified,
		"header":            s.cfg.headerName,
		"allowedNamespaces": s.cfg.allowedNamespaces,
		"tunnels":           s.agents.status(),
	}
	if !connected {
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
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

	s.mu.RLock()
	connected := s.connected
	s.mu.RUnlock()
	if !connected {
		writeErr(w, http.StatusServiceUnavailable, "no manager session yet")
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
			p.Disposition, p.Message, p.AgentPod = prev.Disposition, prev.Message, prev.AgentPod
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
