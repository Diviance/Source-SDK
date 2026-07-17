package antibot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectRequestDoesNotUseSolver(t *testing.T) {
	var solverCalls atomic.Int32
	solver := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { solverCalls.Add(1) }))
	defer solver.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html>ordinary</html>") }))
	defer upstream.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}})
	response := mustDo(t, client, http.MethodGet, upstream.URL, "")
	response.Body.Close()
	if solverCalls.Load() != 0 {
		t.Fatalf("solver calls = %d", solverCalls.Load())
	}
}

func TestChallengeUsesSolverAndReusesIdentity(t *testing.T) {
	const browserUA = "Browser UA"
	var solverCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("cf_clearance")
		if err == nil && cookie.Value == "valid" && r.UserAgent() == browserUA {
			io.WriteString(w, "<html>direct after solve</html>")
			return
		}
		w.Header().Set("Server", "cloudflare")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "<html><title>Just a moment...</title></html>")
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		solverCalls.Add(1)
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["cmd"] != "request.get" || input["url"] != upstream.URL {
			t.Fatalf("solver input = %#v", input)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok","message":"Success","solution":{"url":"`+upstream.URL+`","status":200,"headers":{"content-type":"text/html"},"cookies":[{"name":"cf_clearance","value":"valid","domain":"`+upstreamURL.Hostname()+`","path":"/"}],"userAgent":"`+browserUA+`","response":"<html>rendered by solver</html>"}}`)
	}))
	defer solver.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}})
	first := mustDo(t, client, http.MethodGet, upstream.URL, "")
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if string(firstBody) != "<html>rendered by solver</html>" {
		t.Fatalf("first body = %q", firstBody)
	}
	secondRequest, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	second, err := client.DoDirect(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if string(secondBody) != "<html>direct after solve</html>" || solverCalls.Load() != 1 {
		t.Fatalf("second body = %q, solver calls = %d", secondBody, solverCalls.Load())
	}
}

func TestPostChallengeBootstrapsAndReplaysBody(t *testing.T) {
	const browserUA = "Browser UA"
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		cookie, err := r.Cookie("clearance")
		if err == nil && cookie.Value == "yes" && r.UserAgent() == browserUA {
			body, _ := io.ReadAll(r.Body)
			io.WriteString(w, "accepted:"+string(body))
			return
		}
		w.Header().Set("CF-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"status":"ok","solution":{"url":"`+upstream.URL+`/","status":200,"cookies":[{"name":"clearance","value":"yes","path":"/"}],"userAgent":"`+browserUA+`","response":"<html>ok</html>"}}`)
	}))
	defer solver.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}, BootstrapURL: upstream.URL + "/"})
	response := mustDo(t, client, http.MethodPost, upstream.URL+"/ajax", "a=b")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "accepted:a=b" || posts.Load() != 2 {
		t.Fatalf("body = %q, posts = %d", body, posts.Load())
	}
}

func TestConcurrentChallengesShareOneSolve(t *testing.T) {
	const browserUA = "Browser UA"
	var solverCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("clearance")
		if err == nil && cookie.Value == "yes" && r.UserAgent() == browserUA {
			io.WriteString(w, "<html>direct</html>")
			return
		}
		w.Header().Set("CF-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		solverCalls.Add(1)
		io.WriteString(w, `{"status":"ok","solution":{"url":"`+upstream.URL+`","status":200,"cookies":[{"name":"clearance","value":"yes","path":"/"}],"userAgent":"`+browserUA+`","response":"<html>solved</html>"}}`)
	}))
	defer solver.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}})
	start := make(chan struct{})
	errors := make(chan error, 8)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			request, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
			response, err := client.Do(request)
			if err == nil {
				response.Body.Close()
			}
			errors <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if solverCalls.Load() != 1 {
		t.Fatalf("solver calls = %d", solverCalls.Load())
	}
}

func TestDisallowedHostAndOrdinaryForbiddenDoNotUseSolver(t *testing.T) {
	var solverCalls atomic.Int32
	solver := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { solverCalls.Add(1) }))
	defer solver.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer upstream.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{"example.invalid"}})
	response := mustDo(t, client, http.MethodGet, upstream.URL, "")
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || solverCalls.Load() != 0 {
		t.Fatalf("status = %d, solver calls = %d", response.StatusCode, solverCalls.Load())
	}
}

func TestContextCancellationStopsSolver(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("CF-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	solverStarted := make(chan struct{})
	releaseSolver := make(chan struct{})
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(solverStarted)
		<-releaseSolver
	}))
	defer solver.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}, SolveTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	result := make(chan error, 1)
	go func() {
		_, err := client.Do(request)
		result <- err
	}()
	<-solverStarted
	cancel()
	if err := <-result; err == nil {
		t.Fatal("expected cancellation error")
	}
	close(releaseSolver)
}

func mustDo(t *testing.T, client *Client, method, target, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
