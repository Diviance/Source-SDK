// Package antibot provides an HTTP client that can bootstrap an ordinary
// net/http session through a FlareSolverr-compatible anti-bot service.
package antibot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SolverURLEnvironment is the environment variable used by extension
	// workers to discover the configured FlareSolverr-compatible endpoint.
	SolverURLEnvironment = "MANGAVAULT_ANTIBOT_URL"

	defaultSolveTimeout = 60 * time.Second
	defaultInspectLimit = 512 << 10
	maxSolverResponse   = 32 << 20
)

// ErrorKind identifies failures produced by the anti-bot integration.
type ErrorKind string

const (
	ErrorInvalidConfig     ErrorKind = "invalid_config"
	ErrorSolverUnavailable ErrorKind = "solver_unavailable"
	ErrorChallengeUnsolved ErrorKind = "challenge_unsolved"
	ErrorRequestReplay     ErrorKind = "request_replay"
)

// Error describes a solver or request replay failure.
type Error struct {
	Kind    ErrorKind
	Op      string
	URL     string
	Status  int
	Message string
	Err     error
}

func (e *Error) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" && e.Err != nil {
		message = e.Err.Error()
	}
	if message == "" {
		message = string(e.Kind)
	}
	if e.URL != "" {
		return fmt.Sprintf("anti-bot %s %s: %s", e.Op, e.URL, message)
	}
	return fmt.Sprintf("anti-bot %s: %s", e.Op, message)
}

func (e *Error) Unwrap() error { return e.Err }

// Options configures a Client.
type Options struct {
	HTTPClient       *http.Client
	SolverHTTPClient *http.Client
	SolverURL        string
	AllowedHosts     []string
	BootstrapURL     string
	StateFile        string
	SolveTimeout     time.Duration
	InspectLimit     int64
}

// Client performs ordinary requests first and invokes a solver only when the
// response is recognizable as an anti-bot challenge.
type Client struct {
	direct       *http.Client
	solver       *http.Client
	endpoint     *url.URL
	allowedHosts map[string]struct{}
	bootstrapURL *url.URL
	solveTimeout time.Duration
	inspectLimit int64
	configErr    error
	stateStore   *stateStore

	solveMu    sync.Mutex
	stateMu    sync.RWMutex
	userAgent  string
	generation uint64
}

// NewFromEnvironment constructs a client using MANGAVAULT_ANTIBOT_URL when
// Options.SolverURL is empty.
func NewFromEnvironment(options Options) *Client {
	if strings.TrimSpace(options.SolverURL) == "" {
		options.SolverURL = strings.TrimSpace(os.Getenv(SolverURLEnvironment))
	}
	return New(options)
}

// New constructs a solver-aware HTTP client. An empty SolverURL leaves the
// client in direct-only mode.
func New(options Options) *Client {
	if options.SolveTimeout <= 0 {
		options.SolveTimeout = defaultSolveTimeout
	}
	if options.InspectLimit <= 0 {
		options.InspectLimit = defaultInspectLimit
	}
	direct := options.HTTPClient
	if direct == nil {
		direct = &http.Client{Timeout: 30 * time.Second}
	}
	if direct.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err == nil {
			direct.Jar = jar
		}
	}
	store, err := loadStateStore(options.StateFile)
	if err != nil {
		return &Client{direct: direct, configErr: &Error{Kind: ErrorInvalidConfig, Op: "load state", URL: options.StateFile, Err: err}}
	}
	if store != nil && direct.Jar != nil {
		direct.Jar = newPersistentJar(direct.Jar, store)
	}
	solver := options.SolverHTTPClient
	if solver == nil {
		solver = &http.Client{Timeout: options.SolveTimeout + 5*time.Second}
	}
	client := &Client{
		direct:       direct,
		solver:       solver,
		allowedHosts: make(map[string]struct{}, len(options.AllowedHosts)),
		solveTimeout: options.SolveTimeout,
		inspectLimit: options.InspectLimit,
		stateStore:   store,
	}
	if store != nil {
		client.userAgent = store.userAgent()
		if client.userAgent != "" {
			client.generation = 1
		}
	}
	for _, raw := range options.AllowedHosts {
		if host := normalizeHost(raw); host != "" {
			client.allowedHosts[host] = struct{}{}
		}
	}
	if raw := strings.TrimSpace(options.BootstrapURL); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			client.configErr = &Error{Kind: ErrorInvalidConfig, Op: "configure", URL: raw, Message: "invalid bootstrap URL", Err: err}
		} else {
			client.bootstrapURL = parsed
		}
	}
	if raw := strings.TrimSpace(options.SolverURL); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			client.configErr = &Error{Kind: ErrorInvalidConfig, Op: "configure", URL: raw, Message: "invalid solver URL", Err: err}
		} else if len(client.allowedHosts) == 0 {
			client.configErr = &Error{Kind: ErrorInvalidConfig, Op: "configure", URL: raw, Message: "at least one allowed upstream host is required"}
		} else {
			if parsed.Path == "" || parsed.Path == "/" {
				parsed.Path = "/v1"
			}
			client.endpoint = parsed
		}
	}
	return client
}

// Enabled reports whether a valid solver endpoint is configured.
func (c *Client) Enabled() bool { return c != nil && c.endpoint != nil && c.configErr == nil }

// Do implements the request portion of http.Client.Do with challenge fallback.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	if c == nil {
		return nil, errors.New("nil anti-bot client")
	}
	if request == nil || request.URL == nil {
		return nil, errors.New("nil HTTP request")
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	generation := c.currentGeneration()
	response, err := c.direct.Do(c.withUserAgent(request))
	if err != nil {
		return nil, err
	}
	if !c.canSolve(request.URL) {
		return response, nil
	}
	challenge, err := c.isChallenge(response)
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	if !challenge {
		return response, nil
	}
	response.Body.Close()
	return c.solveOrRetry(request, generation)
}

// DoDirect performs a request with the current cookie jar and browser identity
// but never invokes the solver. It is intended for binary assets such as page
// images, which should not be returned through a solver's JSON document API.
func (c *Client) DoDirect(request *http.Request) (*http.Response, error) {
	if c == nil {
		return nil, errors.New("nil anti-bot client")
	}
	if request == nil || request.URL == nil {
		return nil, errors.New("nil HTTP request")
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	return c.direct.Do(c.withUserAgent(request))
}

// DoAsset performs a binary request directly. If the asset itself is
// challenged, it solves the configured bootstrap document, applies the
// resulting cookies and browser identity, and retries the binary request
// directly. Solver response bodies are never returned as asset bytes.
func (c *Client) DoAsset(request *http.Request) (*http.Response, error) {
	if c == nil {
		return nil, errors.New("nil anti-bot client")
	}
	if request == nil || request.URL == nil {
		return nil, errors.New("nil HTTP request")
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	generation := c.currentGeneration()
	response, err := c.direct.Do(c.withUserAgent(request))
	if err != nil || !c.canSolve(request.URL) {
		return response, err
	}
	challenge, err := c.isChallenge(response)
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	if !challenge {
		return response, nil
	}
	response.Body.Close()
	return c.solveAssetOrRetry(request, generation)
}

func (c *Client) solveOrRetry(request *http.Request, generation uint64) (*http.Response, error) {
	c.solveMu.Lock()
	defer c.solveMu.Unlock()
	if c.currentGeneration() != generation {
		return c.retry(request)
	}
	target := request.URL
	if request.Method != http.MethodGet {
		if c.bootstrapURL != nil {
			target = c.bootstrapURL
		} else {
			target = &url.URL{Scheme: request.URL.Scheme, Host: request.URL.Host, Path: "/"}
		}
	}
	solution, err := c.solve(request.Context(), target)
	if err != nil {
		return nil, err
	}
	if err := c.applySolution(request.URL, solution); err != nil {
		return nil, err
	}
	if request.Method == http.MethodGet {
		return c.responseFromSolution(request, solution)
	}
	return c.retry(request)
}

func (c *Client) solveAssetOrRetry(request *http.Request, generation uint64) (*http.Response, error) {
	c.solveMu.Lock()
	defer c.solveMu.Unlock()
	if c.currentGeneration() == generation {
		target := c.bootstrapURL
		if target == nil || !strings.EqualFold(target.Hostname(), request.URL.Hostname()) {
			target = &url.URL{Scheme: request.URL.Scheme, Host: request.URL.Host, Path: "/"}
		}
		solution, err := c.solve(request.Context(), target)
		if err != nil {
			return nil, err
		}
		if err := c.applySolution(request.URL, solution); err != nil {
			return nil, err
		}
	}
	response, err := c.retry(request)
	if err != nil {
		return nil, err
	}
	challenge, inspectErr := c.isChallenge(response)
	if inspectErr != nil {
		response.Body.Close()
		return nil, inspectErr
	}
	if challenge {
		response.Body.Close()
		return nil, &Error{Kind: ErrorChallengeUnsolved, Op: "retry asset", URL: request.URL.String(), Status: response.StatusCode, Message: "asset remained challenged after refreshing the browser identity"}
	}
	return response, nil
}

func (c *Client) retry(request *http.Request) (*http.Response, error) {
	retry := request.Clone(request.Context())
	if request.Body != nil && request.Body != http.NoBody {
		if request.GetBody == nil {
			return nil, &Error{Kind: ErrorRequestReplay, Op: "retry", URL: request.URL.String(), Message: "request body cannot be replayed"}
		}
		body, err := request.GetBody()
		if err != nil {
			return nil, &Error{Kind: ErrorRequestReplay, Op: "retry", URL: request.URL.String(), Err: err}
		}
		retry.Body = body
	}
	return c.direct.Do(c.withUserAgent(retry))
}

func (c *Client) solve(ctx context.Context, target *url.URL) (solverSolution, error) {
	ctx, cancel := context.WithTimeout(ctx, c.solveTimeout)
	defer cancel()
	input := solverRequest{Command: "request.get", URL: target.String(), MaxTimeout: c.solveTimeout.Milliseconds()}
	if c.direct.Jar != nil {
		for _, cookie := range c.direct.Jar.Cookies(target) {
			input.Cookies = append(input.Cookies, solverRequestCookie{Name: cookie.Name, Value: cookie.Value})
		}
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return solverSolution{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return solverSolution{}, &Error{Kind: ErrorInvalidConfig, Op: "request", URL: c.endpoint.String(), Err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.solver.Do(request)
	if err != nil {
		return solverSolution{}, &Error{Kind: ErrorSolverUnavailable, Op: "request", URL: c.endpoint.String(), Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return solverSolution{}, &Error{Kind: ErrorSolverUnavailable, Op: "request", URL: c.endpoint.String(), Status: response.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	var result solverResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxSolverResponse))
	if err := decoder.Decode(&result); err != nil {
		return solverSolution{}, &Error{Kind: ErrorSolverUnavailable, Op: "decode", URL: c.endpoint.String(), Err: err}
	}
	if !strings.EqualFold(result.Status, "ok") || result.Solution.Status < 200 || result.Solution.Status >= 300 {
		return solverSolution{}, &Error{Kind: ErrorChallengeUnsolved, Op: "solve", URL: target.String(), Status: result.Solution.Status, Message: firstNonEmpty(result.Message, "solver did not return a successful solution")}
	}
	if strings.TrimSpace(result.Solution.URL) == "" {
		result.Solution.URL = target.String()
	}
	resolved, err := url.Parse(result.Solution.URL)
	if err != nil || !c.hostAllowed(resolved) {
		return solverSolution{}, &Error{Kind: ErrorChallengeUnsolved, Op: "solve", URL: target.String(), Message: "solver redirected outside the allowed upstream hosts", Err: err}
	}
	return result.Solution, nil
}

func (c *Client) applySolution(target *url.URL, solution solverSolution) error {
	if c.direct.Jar != nil {
		cookies := make([]*http.Cookie, 0, len(solution.Cookies))
		for _, value := range solution.Cookies {
			domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value.Domain)), ".")
			if domain != "" && !domainMatches(strings.ToLower(target.Hostname()), domain) {
				continue
			}
			cookie := &http.Cookie{Name: value.Name, Value: value.Value, Domain: value.Domain, Path: value.Path, Secure: value.Secure, HttpOnly: value.HTTPOnly, SameSite: parseSameSite(value.SameSite)}
			if cookie.Path == "" {
				cookie.Path = "/"
			}
			if value.Expires > 0 {
				seconds, fraction := mathModf(value.Expires)
				cookie.Expires = time.Unix(seconds, int64(fraction*float64(time.Second)))
			}
			if cookie.Name != "" {
				cookies = append(cookies, cookie)
			}
		}
		c.direct.Jar.SetCookies(target, cookies)
	}
	c.stateMu.Lock()
	c.userAgent = strings.TrimSpace(solution.UserAgent)
	c.generation++
	userAgent := c.userAgent
	c.stateMu.Unlock()
	if c.stateStore != nil {
		if err := c.stateStore.setUserAgent(userAgent); err != nil {
			return &Error{Kind: ErrorInvalidConfig, Op: "save state", URL: c.stateStore.path, Err: err}
		}
	}
	return nil
}

func (c *Client) responseFromSolution(request *http.Request, solution solverSolution) (*http.Response, error) {
	if solution.Response == "" {
		return nil, &Error{Kind: ErrorChallengeUnsolved, Op: "solve", URL: request.URL.String(), Message: "solver returned an empty document"}
	}
	header := make(http.Header, len(solution.Headers))
	for key, raw := range solution.Headers {
		switch value := raw.(type) {
		case string:
			header.Add(key, value)
		case []any:
			for _, item := range value {
				header.Add(key, fmt.Sprint(item))
			}
		case nil:
		default:
			header.Add(key, fmt.Sprint(value))
		}
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", firstNonEmpty(solution.ContentType, "text/html; charset=utf-8"))
	}
	// Solver response bodies contain decoded document text, so transport-level
	// metadata copied from the browser response no longer applies.
	for _, key := range []string{"Content-Encoding", "Content-Length", "Transfer-Encoding"} {
		header.Del(key)
	}
	finalRequest := request.Clone(request.Context())
	if parsed, err := url.Parse(solution.URL); err == nil {
		finalRequest.URL = parsed
	}
	body := []byte(solution.Response)
	return &http.Response{StatusCode: solution.Status, Status: strconv.Itoa(solution.Status) + " " + http.StatusText(solution.Status), Header: header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: finalRequest}, nil
}

func (c *Client) isChallenge(response *http.Response) (bool, error) {
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("CF-Mitigated")), "challenge") {
		return true, nil
	}
	server := strings.ToLower(response.Header.Get("Server"))
	if strings.Contains(server, "cloudflare") && (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusServiceUnavailable) {
		return true, nil
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if response.Body == nil || (contentType != "" && !strings.Contains(contentType, "html")) {
		return false, nil
	}
	prefix, err := io.ReadAll(io.LimitReader(response.Body, c.inspectLimit))
	if err != nil {
		return false, err
	}
	response.Body = &prefixedReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
	body := strings.ToLower(string(prefix))
	for _, marker := range []string{"cf-chl-", "<title>just a moment", "cloudflare ray id", "attention required! | cloudflare", "ddos-guard"} {
		if strings.Contains(body, marker) {
			return true, nil
		}
	}
	// Cloudflare injects challenge-platform/scripts/jsd into ordinary 200
	// pages for JavaScript detections. It is only a challenge signal when the
	// response is unsuccessful or contains the managed-challenge bootstrap.
	if strings.Contains(body, "cdn-cgi/challenge-platform") && (response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(body, "window._cf_chl_opt")) {
		return true, nil
	}
	return false, nil
}

func (c *Client) withUserAgent(request *http.Request) *http.Request {
	c.stateMu.RLock()
	userAgent := c.userAgent
	c.stateMu.RUnlock()
	if userAgent == "" || !c.hostAllowed(request.URL) {
		return request
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("User-Agent", userAgent)
	return clone
}

func (c *Client) canSolve(target *url.URL) bool { return c.endpoint != nil && c.hostAllowed(target) }

func (c *Client) hostAllowed(target *url.URL) bool {
	if target == nil {
		return false
	}
	_, ok := c.allowedHosts[strings.ToLower(target.Hostname())]
	return ok
}

func (c *Client) currentGeneration() uint64 {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.generation
}

type prefixedReadCloser struct {
	io.Reader
	io.Closer
}

type solverRequest struct {
	Command    string                `json:"cmd"`
	URL        string                `json:"url"`
	MaxTimeout int64                 `json:"maxTimeout"`
	Cookies    []solverRequestCookie `json:"cookies,omitempty"`
}

type solverRequestCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type solverResponse struct {
	Status   string         `json:"status"`
	Message  string         `json:"message"`
	Solution solverSolution `json:"solution"`
}

type solverSolution struct {
	URL         string         `json:"url"`
	Status      int            `json:"status"`
	Headers     map[string]any `json:"headers"`
	Cookies     []solverCookie `json:"cookies"`
	UserAgent   string         `json:"userAgent"`
	Response    string         `json:"response"`
	ContentType string         `json:"contentType"`
}

type solverCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
		return strings.ToLower(parsed.Hostname())
	}
	if parsed, err := url.Parse("//" + raw); err == nil {
		return strings.ToLower(parsed.Hostname())
	}
	return ""
}

func domainMatches(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func parseSameSite(value string) http.SameSite {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "strict":
		return http.SameSiteStrictMode
	case "lax":
		return http.SameSiteLaxMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteDefaultMode
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func mathModf(value float64) (int64, float64) {
	seconds := int64(value)
	return seconds, value - float64(seconds)
}
