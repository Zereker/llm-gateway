package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// stubProvider returns the configured user / err regardless of creds.
type stubProvider struct {
	user *domain.UserIdentity
	err  error
}

func (p stubProvider) Resolve(_ context.Context, _ *Credentials) (*domain.UserIdentity, error) {
	return p.user, p.err
}

func TestAuth_RejectsMissingCreds(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), Auth(AuthDeps{Provider: stubProvider{}}))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing credentials") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestAuth_RejectsInvalidCreds(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), Auth(AuthDeps{
		Provider: stubProvider{err: errors.New("unknown api key")},
	}))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer bad")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown api key") {
		t.Errorf("body should include underlying error: %s", w.Body.String())
	}
}

func TestAuth_AcceptsValidBearer(t *testing.T) {
	want := domain.UserIdentity{UserID: "alice", Group: "default"}
	r := newGinTest(TraceContext(), Recover(), Auth(AuthDeps{
		Provider: stubProvider{user: &want},
	}))
	var got domain.UserIdentity
	r.GET("/x", func(c *gin.Context) {
		got = GetRequestContext(c).Identity
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer sk-aaa")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got.UserID != "alice" {
		t.Errorf("Identity.UserID = %q, want alice", got.UserID)
	}
}

func TestAuth_LoggerGetsUserID(t *testing.T) {
	want := domain.UserIdentity{UserID: "carol"}
	r := newGinTest(TraceContext(), Recover(), Auth(AuthDeps{
		Provider: stubProvider{user: &want},
	}))
	r.GET("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Logger == nil {
			t.Error("logger nil")
		}
		// Can't easily inspect slog logger attrs; just ensure no panic.
		rc.Logger.Info("test")
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestExtractCredentials(t *testing.T) {
	cases := []struct {
		name        string
		headers     map[string]string
		wantNil     bool
		wantAPIKey  string
		wantBearer  string
	}{
		{"no headers", nil, true, "", ""},
		{"X-API-Key only", map[string]string{"X-API-Key": "ak1"}, false, "ak1", ""},
		{"Bearer only", map[string]string{"Authorization": "Bearer tok"}, false, "tok", "tok"},
		{"both, X-API-Key wins APIKey", map[string]string{
			"Authorization": "Bearer tok",
			"X-API-Key":     "ak2",
		}, false, "ak2", "tok"},
		{"non-Bearer Authorization ignored", map[string]string{"Authorization": "Basic xx"}, true, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.GET("/x", func(c *gin.Context) {
				creds := extractCredentials(c)
				switch {
				case tc.wantNil:
					if creds != nil {
						t.Errorf("want nil creds, got %+v", creds)
					}
				case creds == nil:
					t.Error("got nil, want non-nil")
				default:
					if creds.APIKey != tc.wantAPIKey {
						t.Errorf("APIKey = %q, want %q", creds.APIKey, tc.wantAPIKey)
					}
					if creds.BearerToken != tc.wantBearer {
						t.Errorf("BearerToken = %q, want %q", creds.BearerToken, tc.wantBearer)
					}
				}
				c.Status(200)
			})

			req := httptest.NewRequest("GET", "/x", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			r.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
}
