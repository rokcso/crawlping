package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// endpointsInDocs returns every method/path pair the README's endpoint table
// lists, in the order it lists them.
//
// The table is the one place a caller can see the whole surface at once, so it is
// the thing most likely to be forgotten when an endpoint is added. Reading it
// here rather than restating it is what makes this a check instead of a copy.
func endpointsInDocs(t *testing.T) []string {
	t.Helper()

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	// | `POST` | `/submit` | auth | success | what it does |
	row := regexp.MustCompile("^\\|\\s*`(GET|POST)`\\s*\\|\\s*`(/[^`]*)`\\s*\\|")

	var found []string
	for _, line := range strings.Split(string(readme), "\n") {
		if match := row.FindStringSubmatch(line); match != nil {
			found = append(found, match[1]+" "+match[2])
		}
	}

	if len(found) == 0 {
		t.Fatal("the endpoint table in README.md matched nothing; the table or this regexp has moved")
	}

	return found
}

func TestEveryDocumentedEndpointIsServed(t *testing.T) {
	// The table claims these exist. Anything it lists has to route: a typo in the
	// path, or a document written ahead of the code, would otherwise be found by
	// the first caller rather than by CI.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	handler := srv.routes()

	for _, endpoint := range endpointsInDocs(t) {
		method, path, _ := strings.Cut(endpoint, " ")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))

		if recorder.Code == http.StatusNotFound {
			t.Fatalf("%s %s is documented but not routed (got 404)", method, path)
		}
	}
}

func TestEveryServedPathIsDocumented(t *testing.T) {
	// The other direction, which is the one that actually rots: a new endpoint
	// ships and the table is never touched.
	//
	// This list is a restatement, not an introspection, and that is its weakness:
	// Go's ServeMux does not expose what was registered, so adding a route
	// without adding it here keeps this test green. It catches the common case
	// (a route added to the table's sibling, its documentation forgotten) rather
	// than proving the table complete. Closing that properly means registering
	// routes from one slice both routes() and this test read, which is a change to
	// routes() worth making when there are more than three of them.
	served := []string{"/submit", "/health", "/metrics"}

	documented := make(map[string]bool)
	for _, endpoint := range endpointsInDocs(t) {
		_, path, _ := strings.Cut(endpoint, " ")
		documented[path] = true
	}

	for _, path := range served {
		if !documented[path] {
			t.Fatalf("%s is served but missing from the endpoint table in README.md; add a row for it", path)
		}
	}

	if len(documented) != len(served) {
		t.Fatalf("the table documents %d paths but %d are served; one of the two lists is stale",
			len(documented), len(served))
	}
}

func TestDocsAPIReferenceMatchesTheTable(t *testing.T) {
	// The README table is the summary; docs/API.md is the contract. A path that
	// appears in one and not the other means a reader following the link finds a
	// different API from the one the table promised.
	api, err := os.ReadFile("docs/API.md")
	if err != nil {
		t.Fatalf("read docs/API.md: %v", err)
	}

	for _, endpoint := range endpointsInDocs(t) {
		_, path, _ := strings.Cut(endpoint, " ")
		if !strings.Contains(string(api), "`"+path+"`") {
			t.Fatalf("docs/API.md never mentions %s, which the README table lists", path)
		}
	}
}
