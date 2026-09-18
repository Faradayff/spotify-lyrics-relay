package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	APIBase    = "https://api.spotify.com"
	AuthURL    = "https://accounts.spotify.com/authorize"
	TokenURL   = "https://accounts.spotify.com/api/token"
	Scope      = "user-read-playback-state user-read-currently-playing streaming"
)

type spotifyTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

type spotifyClient struct {
	mu        sync.Mutex
	clientID  string
	clientSec string
	redirect  string
	tokens    spotifyTokens
	stateDir  string
	http      *http.Client
}

func newSpotifyClient() *spotifyClient {
	sd := os.Getenv("STATE_DIR")
	if sd == "" {
		sd = "data"
	}
	c := &spotifyClient{
		clientID:  os.Getenv("SPOTIFY_CLIENT_ID"),
		clientSec: os.Getenv("SPOTIFY_CLIENT_SECRET"),
		redirect:  os.Getenv("SPOTIFY_REDIRECT_URI"),
		stateDir:  sd,
		http:      &http.Client{Timeout: 20 * time.Second},
	}
	c.load()
	return c
}

func (s *spotifyClient) stateFile() string {
	return filepath.Join(s.stateDir, "tokens.json")
}

func (s *spotifyClient) load() {
	b, err := os.ReadFile(s.stateFile())
	if err != nil {
		return
	}
	var t spotifyTokens
	if err := json.Unmarshal(b, &t); err == nil {
		s.tokens = t
	}
}

func (s *spotifyClient) save() {
	if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
		return
	}
	b, _ := json.Marshal(s.tokens)
	_ = os.WriteFile(s.stateFile(), b, 0o600)
}

func (s *spotifyClient) apply(tokenResp map[string]any) {
	ttl := int64(3600)
	if v, ok := tokenResp["expires_in"].(float64); ok {
		ttl = int64(v)
	}
	if at, _ := tokenResp["access_token"].(string); at != "" {
		s.tokens.AccessToken = at
	}
	if rt, _ := tokenResp["refresh_token"].(string); rt != "" {
		s.tokens.RefreshToken = rt
	}
	s.tokens.ExpiresAt = time.Now().UnixMilli() + ttl*1000
	s.save()
}

func (s *spotifyClient) refresh() error {
	if s.tokens.RefreshToken == "" {
		return errors.New("refresh token ausente; vuelve a iniciar sesion con GET /login")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.tokens.RefreshToken},
		"client_id":     {s.clientID},
		"client_secret": {s.clientSec},
	}
	body, status, _, err := s.doToken(form)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("refresh fallido: %d %s", status, string(body))
	}
	s.mu.Lock()
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		s.apply(m)
	}
	s.mu.Unlock()
	return nil
}

func (s *spotifyClient) accessToken() (string, error) {
	s.mu.Lock()
	t := s.tokens
	s.mu.Unlock()
	if t.AccessToken != "" && t.ExpiresAt > time.Now().UnixMilli()+30000 {
		return t.AccessToken, nil
	}
	if err := s.refresh(); err != nil {
		return "", err
	}
	s.mu.Lock()
	t = s.tokens
	s.mu.Unlock()
	if t.AccessToken == "" {
		return "", errors.New("sin access token")
	}
	return t.AccessToken, nil
}

func (s *spotifyClient) authorized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens.AccessToken != "" || s.tokens.RefreshToken != ""
}

func (s *spotifyClient) loginURL() (string, error) {
	if s.clientID == "" || s.redirect == "" {
		return "", errors.New("falta SPOTIFY_CLIENT_ID o SPOTIFY_REDIRECT_URI")
	}
	q := url.Values{
		"client_id":     {s.clientID},
		"response_type": {"code"},
		"redirect_uri":  {s.redirect},
		"scope":         {Scope},
		"show_dialog":   {"false"},
	}
	return AuthURL + "?" + q.Encode(), nil
}

func (s *spotifyClient) exchangeCode(code string) error {
	if s.clientID == "" || s.clientSec == "" {
		return errors.New("falta SPOTIFY_CLIENT_ID o SPOTIFY_CLIENT_SECRET")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {s.clientID},
		"client_secret": {s.clientSec},
		"redirect_uri":  {s.redirect},
	}
	body, status, _, err := s.doToken(form)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("exchange fallido: %d %s", status, string(body))
	}
	s.mu.Lock()
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		s.apply(m)
	}
	s.mu.Unlock()
	return nil
}

func (s *spotifyClient) logout() {
	s.mu.Lock()
	s.tokens = spotifyTokens{}
	s.save()
	s.mu.Unlock()
}

func (s *spotifyClient) call(method, path string) (io.ReadCloser, int, http.Header, error) {
	token, err := s.accessToken()
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequest(method, APIBase+path, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "lyrics-relay/0.1")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if err := s.refresh(); err != nil {
			resp.Body.Close()
			return nil, 0, nil, err
		}
		resp.Body.Close()
		token, err = s.accessToken()
		if err != nil {
			return nil, 0, nil, err
		}
		req2, _ := http.NewRequest(method, APIBase+path, nil)
		req2.Header.Set("Authorization", "Bearer "+token)
		req2.Header.Set("User-Agent", "lyrics-relay/0.1")
		resp, err = s.http.Do(req2)
		if err != nil {
			return nil, 0, nil, err
		}
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, resp.StatusCode, nil, fmt.Errorf("spotify %d: %s", resp.StatusCode, string(b))
	}
	return resp.Body, resp.StatusCode, resp.Header, nil
}

func (s *spotifyClient) mePlayer() (map[string]any, error) {
	body, _, _, err := s.call("GET", "/v1/me/player")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}, nil
	}
	return m, nil
}

func (s *spotifyClient) control(action string) error {
	method, path, ok := map[string][2]string{
		"next":   {"POST", "/v1/me/player/next"},
		"prev":   {"POST", "/v1/me/player/previous"},
		"pause":  {"PUT", "/v1/me/player/pause"},
		"resume": {"PUT", "/v1/me/player/play"},
	}[action]
	if !ok {
		return fmt.Errorf("accion desconocida: %q", action)
	}
	body, _, _, err := s.call(method, path)
	if err != nil {
		return err
	}
	if body != nil {
		body.Close()
	}
	return nil
}

func (s *spotifyClient) doToken(form url.Values) ([]byte, int, map[string]any, error) {
	req, err := http.NewRequest("POST", TokenURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "lyrics-relay/0.1")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, resp.StatusCode, nil, err
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		m = map[string]any{}
	}
	return b, resp.StatusCode, m, nil
}
