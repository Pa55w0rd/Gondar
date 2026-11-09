package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

func TestGondarLoginAndCookieAuthentication(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	previous, existed, err := m.pg.GetSetting(authPassKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = m.pg.SetSetting(authPassKey, previous)
		} else {
			_, _ = m.pg.Exec("DELETE FROM settings WHERE key=$1", authPassKey)
		}
	})
	const password = "Gondar-test-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(authPassKey, string(hash)); err != nil {
		t.Fatal(err)
	}
	s := testKey([]byte("test-signing-key-for-Gondar-login"))
	s.m = m
	for _, username := range []string{"Gondar", "gondar", "GONDAR"} {
		t.Run(username, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"username": username, "password": password})
			r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(string(body)))
			r.RemoteAddr = "192.0.2.22:1234"
			w := httptest.NewRecorder()
			s.authLogin(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
			}
			var response struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			claims := &jwt.RegisteredClaims{}
			if _, err := jwt.ParseWithClaims(response.Token, claims, func(*jwt.Token) (any, error) {
				return s.currentJWTKey(), nil
			}); err != nil || claims.Subject != "Gondar" {
				t.Fatalf("login claims subject=%q error=%v", claims.Subject, err)
			}
			protected := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			for _, tc := range []struct {
				cookie string
				status int
			}{{"gondar_token", http.StatusNoContent}, {"unrelated_token", http.StatusUnauthorized}} {
				r := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
				r.AddCookie(&http.Cookie{Name: tc.cookie, Value: response.Token})
				w := httptest.NewRecorder()
				protected.ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("cookie %q status=%d want=%d", tc.cookie, w.Code, tc.status)
				}
			}
		})
	}
}
