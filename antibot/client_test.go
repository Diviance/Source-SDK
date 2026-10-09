package antibot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewFromEnvironmentAppliesSolveTimeout(t *testing.T) {
	for _, test := range []struct {
		name     string
		env      string
		explicit time.Duration
		want     time.Duration
		invalid  bool
	}{
		{name: "default", want: time.Minute},
		{name: "configured", env: "90", want: 90 * time.Second},
		{name: "trimmed", env: " 120 ", want: 2 * time.Minute},
		{name: "maximum", env: "300", want: 5 * time.Minute},
		{name: "explicit wins", env: "90", explicit: 2 * time.Second, want: 2 * time.Second},
		{name: "explicit ignores invalid environment", env: "invalid", explicit: 2 * time.Second, want: 2 * time.Second},
		{name: "invalid", env: "invalid", invalid: true},
		{name: "zero", env: "0", invalid: true},
		{name: "negative", env: "-1", invalid: true},
		{name: "too large", env: "301", invalid: true},
		{name: "overflow", env: "999999999999999999999", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(SolverURLEnvironment, "http://solver.invalid/v1")
			t.Setenv(SolverTimeoutEnvironment, test.env)
			client := NewFromEnvironment(Options{SolveTimeout: test.explicit, AllowedHosts: []string{"upstream.invalid"}})
			if test.invalid {
				request, _ := http.NewRequest(http.MethodGet, "http://upstream.invalid", nil)
				_, err := client.Do(request)
				var configErr *Error
				if !errors.As(err, &configErr) || configErr.Kind != ErrorInvalidConfig {
					t.Fatalf("configuration error = %v", err)
				}
				return
			}
			if !client.Enabled() || client.solveTimeout != test.want || client.solver.Timeout != test.want+solverResponseGrace {
				t.Fatalf("enabled = %v, solve timeout = %s, HTTP timeout = %s; want %s plus response allowance", client.Enabled(), client.solveTimeout, client.solver.Timeout, test.want)
			}
		})
	}
}

func TestSolverReceivesConfiguredTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("CF-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	maxTimeout := make(chan int64, 1)
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input solverRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		maxTimeout <- input.MaxTimeout
		io.WriteString(w, `{"status":"ok","solution":{"url":"`+upstream.URL+`","status":200,"response":"solved"}}`)
	}))
	defer solver.Close()
	t.Setenv(SolverURLEnvironment, solver.URL)
	t.Setenv(SolverTimeoutEnvironment, "120")
	client := NewFromEnvironment(Options{AllowedHosts: []string{upstream.URL}})
	response := mustDo(t, client, http.MethodGet, upstream.URL, "")
	response.Body.Close()
	select {
	case got := <-maxTimeout:
		if got != 120000 {
			t.Fatalf("solver maxTimeout = %d, want 120000 milliseconds", got)
		}
	default:
		t.Fatal("solver did not receive a request")
	}
}

func TestSlowSolverResponseUsesGraceAndIgnoresShortHTTPTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("CF-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(150 * time.Millisecond):
			io.WriteString(w, `{"status":"ok","solution":{"url":"`+upstream.URL+`","status":200,"response":"slow solution"}}`)
		case <-r.Context().Done():
		}
	}))
	defer solver.Close()
	shared := &http.Client{Timeout: 25 * time.Millisecond}
	client := New(Options{
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}, SolverHTTPClient: shared,
		SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}, SolveTimeout: 75 * time.Millisecond,
	})
	response := mustDo(t, client, http.MethodGet, upstream.URL, "")
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "slow solution" {
		t.Fatalf("slow response = %q, %v", body, err)
	}
	if shared.Timeout != 25*time.Millisecond {
		t.Fatal("shared solver HTTP client was mutated")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestUnresponsiveSolverStopsAtDeadline(t *testing.T) {
	solveTimeout := 10 * time.Millisecond
	client := New(Options{
		SolverURL: "http://solver.invalid/v1", SolveTimeout: solveTimeout, AllowedHosts: []string{"upstream.invalid"},
		SolverHTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})},
	})
	target, _ := url.Parse("http://upstream.invalid")
	started := time.Now()
	_, err := client.solve(context.Background(), target)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unresponsive solver error = %v, want deadline exceeded", err)
	}
	if elapsed < solveTimeout+solverResponseGrace || elapsed > solveTimeout+solverResponseGrace+time.Second {
		t.Fatalf("solver waited %s, want solve timeout plus response allowance", elapsed)
	}
}

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

func TestOrdinaryCloudflareJSDPageDoesNotUseSolver(t *testing.T) {
	var solverCalls atomic.Int32
	solver := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { solverCalls.Add(1) }))
	defer solver.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><body>ordinary<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script></body></html>`)
	}))
	defer upstream.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}})
	response := mustDo(t, client, http.MethodGet, upstream.URL, "")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "ordinary") || solverCalls.Load() != 0 {
		t.Fatalf("body = %q, solver calls = %d", body, solverCalls.Load())
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

func TestSolvedIdentityPersistsAcrossClients(t *testing.T) {
	const browserUA = "Persistent Browser UA"
	var solverCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("cf_clearance")
		if err == nil && cookie.Value == "persisted" && r.UserAgent() == browserUA {
			io.WriteString(w, "<html>direct persisted session</html>")
			return
		}
		w.Header().Set("CF-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		solverCalls.Add(1)
		io.WriteString(w, `{"status":"ok","solution":{"url":"`+upstream.URL+`","status":200,"cookies":[{"name":"cf_clearance","value":"persisted","domain":"`+upstreamURL.Hostname()+`","path":"/"}],"userAgent":"`+browserUA+`","response":"<html>solved</html>"}}`)
	}))
	defer solver.Close()
	stateFile := filepath.Join(t.TempDir(), "antibot.json")
	firstClient := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}, StateFile: stateFile})
	first := mustDo(t, firstClient, http.MethodGet, upstream.URL, "")
	first.Body.Close()

	secondClient := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}, StateFile: stateFile})
	second := mustDo(t, secondClient, http.MethodGet, upstream.URL, "")
	body, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if string(body) != "<html>direct persisted session</html>" || solverCalls.Load() != 1 {
		t.Fatalf("body = %q, solver calls = %d", body, solverCalls.Load())
	}
}

func TestAssetChallengeSolvesBootstrapAndRetriesDirect(t *testing.T) {
	const browserUA = "Asset Browser UA"
	var solverCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("cf_clearance")
		if r.URL.Path == "/cover.jpg" && err == nil && cookie.Value == "asset" && r.UserAgent() == browserUA {
			w.Header().Set("Content-Type", "image/jpeg")
			io.WriteString(w, "jpeg bytes")
			return
		}
		w.Header().Set("CF-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		solverCalls.Add(1)
		var input solverRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input.URL != upstream.URL+"/" {
			t.Fatalf("solver target = %q", input.URL)
		}
		io.WriteString(w, `{"status":"ok","solution":{"url":"`+upstream.URL+`/","status":200,"cookies":[{"name":"cf_clearance","value":"asset","domain":"`+upstreamURL.Hostname()+`","path":"/"}],"userAgent":"`+browserUA+`","response":"<html>solved</html>"}}`)
	}))
	defer solver.Close()
	client := New(Options{SolverURL: solver.URL, AllowedHosts: []string{upstream.URL}, BootstrapURL: upstream.URL + "/"})
	request, _ := http.NewRequest(http.MethodGet, upstream.URL+"/cover.jpg", nil)
	response, err := client.DoAsset(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "jpeg bytes" || response.Header.Get("Content-Type") != "image/jpeg" || solverCalls.Load() != 1 {
		t.Fatalf("body = %q, content type = %q, solver calls = %d", body, response.Header.Get("Content-Type"), solverCalls.Load())
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
	defer close(releaseSolver)
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	result := make(chan error, 1)
	go func() {
		_, err := client.Do(request)
		result <- err
	}()
	<-solverStarted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation error, got %v", err)
	}
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
