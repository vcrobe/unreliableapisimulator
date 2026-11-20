package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProviderRoutes(t *testing.T) {
	handler, err := loadHandler("providers")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{"GET", "/payments/123", 500},
		{"GET", "/payments/123", 500},
		{"GET", "/payments/123", 200},
		{"GET", "/payments/123", 200},
		{"POST", "/refunds", 500},
		{"POST", "/refunds", 500},
		{"POST", "/refunds", 200},
		{"POST", "/refunds", 200},
		{"GET", "/refunds", 405},
		{"GET", "/missing", 404},
	} {
		req, err := http.NewRequest(test.method, server.URL+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != test.status {
			t.Fatalf("%s %s: got %d, want %d", test.method, test.path, res.StatusCode, test.status)
		}
		if test.status == 200 && (!json.Valid(body) || res.Header.Get("Content-Type") != "application/json") {
			t.Fatalf("expected JSON response, got headers=%v body=%s", res.Header, body)
		}
	}
}

func TestSequenceRepeatsAndSupportsConcurrentCalls(t *testing.T) {
	e, err := newEndpoint(endpointConfig{
		Method: "GET", Path: "/", Policy: "sequence", Repeat: true,
		Responses: []responseConfig{{Status: 500}, {Status: 200}},
	})
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(chan int, 100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- e.selectResponse().Status
		}()
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[200] != 50 || counts[500] != 50 {
		t.Fatalf("unexpected outcome counts: %v", counts)
	}
}

func TestSlowResponseCausesClientTimeoutAndStopsOnCancellation(t *testing.T) {
	e, err := newEndpoint(endpointConfig{
		Method: "GET", Path: "/",
		Responses: []responseConfig{{Status: 200, Delay: 10000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		e.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 100 * time.Millisecond
	res, err := client.Get(server.URL)
	if res != nil {
		res.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected client timeout, got %v", err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("handler kept waiting after client cancellation")
	}
}

func TestDelayAndCustomHeaders(t *testing.T) {
	e, err := newEndpoint(endpointConfig{
		Method: "GET", Path: "/",
		Responses: []responseConfig{{Status: 429, Delay: 20, Headers: map[string]string{"Retry-After": "2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if time.Since(started) < 20*time.Millisecond {
		t.Fatal("response returned before configured delay")
	}
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("unexpected response: %v", w.Result())
	}
}

func TestInvalidConfigurations(t *testing.T) {
	for _, test := range []struct {
		name  string
		files []string
	}{
		{"empty directory", nil},
		{"unknown field", []string{`{"name":"test","typo":true}`}},
		{"extra JSON", []string{`{"name":"test"} {}`}},
		{"missing name", []string{`{"endpoints":[]}`}},
		{"negative delay", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/","responses":[{"status":200,"delay":-1}]}]}`}},
		{"delay overflow", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/","responses":[{"status":200,"delay":9223372036855}]}]}`}},
		{"string delay", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/","responses":[{"status":200,"delay":"10s"}]}]}`}},
		{"invalid status", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/","responses":[{"status":0}]}]}`}},
		{"invalid policy", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/","policy":"oops","responses":[{"status":200}]}]}`}},
		{"missing responses", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/"}]}`}},
		{"negative weight", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/","responses":[{"status":200,"weight":-1}]}]}`}},
		{"invalid pattern", []string{`{"name":"test","endpoints":[{"method":"GET","path":"/{","responses":[{"status":200}]}]}`}},
		{"duplicate routes", []string{
			`{"name":"one","endpoints":[{"method":"GET","path":"/","responses":[{"status":200}]}]}`,
			`{"name":"two","endpoints":[{"method":"GET","path":"/","responses":[{"status":500}]}]}`,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for i, contents := range test.files {
				if err := os.WriteFile(filepath.Join(dir, strings.Repeat("x", i+1)+".json"), []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadHandler(dir); err == nil {
				t.Fatal("expected invalid configuration to be rejected")
			}
		})
	}
}
