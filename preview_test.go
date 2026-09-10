package main

import (
	"strings"
	"testing"
)

// cfgFor is a config with a deliberately short allow-list, so a test can say
// "this namespace is on it" and "this one is not" without ambiguity.
func cfgFor(namespaces ...string) *config {
	return &config{
		headerName:        "x-preview",
		allowedNamespaces: namespaces,
	}
}

// TestAllowListBoundsBothDirections is the executable form of the safety
// boundary: ALLOWED_NAMESPACES has to bound where traffic is forwarded TO as
// well as where it is intercepted.
//
// It regressed once in the other direction -- `namespace` was checked and the
// preview service's namespace was not, so a caller could intercept in an
// allowed namespace and have the tunnel forwarded to a ClusterIP in any
// namespace in the cluster, by setting previewNamespace or by writing
// "name.other-ns" in previewService.
func TestAllowListBoundsBothDirections(t *testing.T) {
	cfg := cfgFor("shop", "previews")

	cases := []struct {
		name    string
		req     PreviewRequest
		wantErr string // substring; empty means the request must be accepted
		wantNS  string // expected forward-target namespace when accepted
	}{{
		name: "forward target defaults to the intercepted namespace",
		req: PreviewRequest{WorkID: "4821", Workload: "checkout-api",
			Namespace: "shop", PreviewService: "checkout-api-preview"},
		wantNS: "shop",
	}, {
		name: "forward target named explicitly and on the list",
		req: PreviewRequest{WorkID: "4821", Workload: "checkout-api",
			Namespace: "shop", PreviewService: "some-preview",
			PreviewNamespace: "previews"},
		wantNS: "previews",
	}, {
		name: "forward target embedded as name.namespace and on the list",
		req: PreviewRequest{WorkID: "4821", Workload: "checkout-api",
			Namespace: "shop", PreviewService: "some-preview.previews"},
		wantNS: "previews",
	}, {
		// The finding. Interception is in an allowed namespace, so the request
		// used to pass; forwarding went wherever the caller asked.
		name: "REFUSED: forward target off the list via previewService",
		req: PreviewRequest{WorkID: "x", Workload: "checkout-api",
			Namespace: "shop", PreviewService: "whatever.some-other-ns"},
		wantErr: "forward-target namespace \"some-other-ns\" is not in",
	}, {
		name: "REFUSED: forward target off the list via previewNamespace",
		req: PreviewRequest{WorkID: "x", Workload: "checkout-api",
			Namespace: "shop", PreviewService: "whatever",
			PreviewNamespace: "kube-system"},
		wantErr: "forward-target namespace \"kube-system\" is not in",
	}, {
		name: "REFUSED: intercept namespace off the list (the original check)",
		req: PreviewRequest{WorkID: "x", Workload: "coredns",
			Namespace: "kube-system", PreviewService: "whatever"},
		wantErr: "namespace \"kube-system\" is not in",
	}, {
		// An FQDN smuggled in as the namespace would otherwise survive
		// resolveTarget's "already qualified" check and skip qualification.
		name: "REFUSED: an FQDN passed as the forward namespace",
		req: PreviewRequest{WorkID: "x", Workload: "checkout-api",
			Namespace: "shop", PreviewService: "whatever",
			PreviewNamespace: "elsewhere.svc.cluster.local"},
		wantErr: "must be a DNS label",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := c.req.validate(cfg)

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("accepted a request that must be refused; got target %q", p.TargetService)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("wrong refusal\n  want substring: %s\n  got:            %v", c.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("refused a legitimate request: %v", err)
			}
			if want := "." + c.wantNS; !strings.HasSuffix(p.TargetService, want) {
				t.Fatalf("forward target %q does not sit in namespace %q", p.TargetService, c.wantNS)
			}
		})
	}
}

// TestRefusalNamesTheFieldItMeans guards the wording, because the two
// namespaces are different request fields and a caller reading a vague message
// will go and change the wrong one.
func TestRefusalNamesTheFieldItMeans(t *testing.T) {
	cfg := cfgFor("shop")
	req := PreviewRequest{WorkID: "x", Workload: "checkout-api",
		Namespace: "shop", PreviewService: "whatever.some-other-ns"}
	_, err := req.validate(cfg)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"forward-target", "previewService", "some-other-ns", "shop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q, so a caller cannot tell which field to fix:\n  %v", want, err)
		}
	}
}

// TestHeaderValueIsTheWorkID pins the invariant a work id exists for: one
// header value reaching every service raised under it.
func TestHeaderValueIsTheWorkID(t *testing.T) {
	cfg := cfgFor("shop")
	for _, workload := range []string{"checkout-api", "pricing-api"} {
		req := PreviewRequest{WorkID: "4821", Workload: workload,
			Namespace: "shop", PreviewService: workload + "-preview"}
		p, err := req.validate(cfg)
		if err != nil {
			t.Fatalf("%s: %v", workload, err)
		}
		if p.HeaderName != "x-preview" || p.HeaderValue != "4821" {
			t.Fatalf("%s: header is %s: %s, want x-preview: 4821", workload, p.HeaderName, p.HeaderValue)
		}
	}
}

// TestMissingManagerAddrIsRefused pins the one thing loadConfig must not
// guess at: where the traffic-manager lives. Its namespace depends on how
// telepresence was installed, so there is no honest default.
func TestMissingManagerAddrIsRefused(t *testing.T) {
	t.Setenv("MANAGER_ADDR", "")
	t.Setenv("ALLOWED_NAMESPACES", "shop")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "MANAGER_ADDR") {
		t.Fatalf("want a refusal naming MANAGER_ADDR, got %v", err)
	}

	t.Setenv("MANAGER_ADDR", "traffic-manager.telepresence.svc.cluster.local:8081")
	t.Setenv("ALLOWED_NAMESPACES", "")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "ALLOWED_NAMESPACES") {
		t.Fatalf("want a refusal naming ALLOWED_NAMESPACES, got %v", err)
	}
}
