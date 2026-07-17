package antibot

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const stateVersion = 1

type persistedState struct {
	Version   int                  `json:"version"`
	UserAgent string               `json:"userAgent,omitempty"`
	Cookies   []persistedCookieSet `json:"cookies,omitempty"`
}

type persistedCookieSet struct {
	URL     string         `json:"url"`
	Cookies []*http.Cookie `json:"cookies"`
}

type stateStore struct {
	mu    sync.Mutex
	path  string
	state persistedState
}

func loadStateStore(path string) (*stateStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	store := &stateStore{path: path, state: persistedState{Version: stateVersion}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decode anti-bot state: %w", err)
	}
	if store.state.Version != stateVersion {
		return nil, fmt.Errorf("unsupported anti-bot state version %d", store.state.Version)
	}
	return store, nil
}

func (s *stateStore) userAgent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.UserAgent
}

func (s *stateStore) cookieSets() []persistedCookieSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]persistedCookieSet, len(s.state.Cookies))
	for i, set := range s.state.Cookies {
		result[i] = persistedCookieSet{URL: set.URL, Cookies: cloneCookies(set.Cookies)}
	}
	return result
}

func (s *stateStore) setUserAgent(userAgent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.UserAgent = strings.TrimSpace(userAgent)
	return s.saveLocked()
}

func (s *stateStore) setCookies(rawURL string, cookies []*http.Cookie) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Cookies {
		if s.state.Cookies[i].URL == rawURL {
			s.state.Cookies[i].Cookies = mergeCookies(s.state.Cookies[i].Cookies, cookies)
			return s.saveLocked()
		}
	}
	s.state.Cookies = append(s.state.Cookies, persistedCookieSet{URL: rawURL, Cookies: cloneCookies(cookies)})
	return s.saveLocked()
}

func (s *stateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".antibot-*.json")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(&s.state); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, s.path)
}

type persistentJar struct {
	jar   http.CookieJar
	store *stateStore
}

func newPersistentJar(jar http.CookieJar, store *stateStore) *persistentJar {
	persistent := &persistentJar{jar: jar, store: store}
	for _, set := range store.cookieSets() {
		if parsed, err := url.Parse(set.URL); err == nil {
			jar.SetCookies(parsed, cloneCookies(set.Cookies))
		}
	}
	return persistent
}

func (p *persistentJar) Cookies(target *url.URL) []*http.Cookie {
	return p.jar.Cookies(target)
}

func (p *persistentJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	p.jar.SetCookies(target, cookies)
	_ = p.store.setCookies(target.String(), cookies)
}

func mergeCookies(existing, updates []*http.Cookie) []*http.Cookie {
	result := cloneCookies(existing)
	for _, update := range updates {
		if update == nil || update.Name == "" {
			continue
		}
		replaced := false
		for i, current := range result {
			if current.Name == update.Name && current.Domain == update.Domain && current.Path == update.Path {
				copy := *update
				result[i] = &copy
				replaced = true
				break
			}
		}
		if !replaced {
			copy := *update
			result = append(result, &copy)
		}
	}
	return result
}

func cloneCookies(cookies []*http.Cookie) []*http.Cookie {
	result := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie != nil {
			copy := *cookie
			result = append(result, &copy)
		}
	}
	return result
}
