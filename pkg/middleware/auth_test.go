package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// stubProvider returns the configured user / err regardless of creds.
type stubProvider struct {
	user *domain.UserIdentity
	err  error
}

func (p stubProvider) Resolve(_ context.Context, _ *domain.Credentials) (*domain.UserIdentity, error) {
	return p.user, p.err
}

func TestAuth_RejectsMissingCreds(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), Auth(WithIdentityProvider(stubProvider{})))
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

// 凭证无效（wrap domain.ErrInvalidCredentials）→ 401，固定文案，**不**泄漏内部细节。
func TestAuth_RejectsInvalidCreds(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), Auth(
		WithIdentityProvider(stubProvider{
			err: fmt.Errorf("apikey: revoked at 2026-01-01: %w", domain.ErrInvalidCredentials),
		}),
	))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer bad")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid credentials") {
		t.Errorf("body = %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "revoked") {
		t.Errorf("内部错误细节不该出现在响应 body：%s", w.Body.String())
	}
}

// 依赖故障（非 sentinel 的裸错误，如 SQL 连不上）→ fail-closed 503 + Retry-After，
// 不得伪装成 401（docs/01 §7），错误细节不进 body。
func TestAuth_DependencyFailureFailsClosed503(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), Auth(
		WithIdentityProvider(stubProvider{err: errors.New("apikey: lookup: dial tcp 10.0.0.5:3306: i/o timeout")}),
	))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503 (DB 故障不能伪装成 401)", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 应带 Retry-After")
	}
	if strings.Contains(w.Body.String(), "dial tcp") {
		t.Errorf("SQL 错误细节不该出现在响应 body：%s", w.Body.String())
	}
}

func TestAuth_AcceptsValidBearer(t *testing.T) {
	want := domain.UserIdentity{SubAccountID: "alice", Group: "default"}
	r := newGinTest(TraceContext(), Recover(), Auth(
		WithIdentityProvider(stubProvider{user: &want}),
	))
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
	if got.SubAccountID != "alice" {
		t.Errorf("Identity.SubAccountID = %q, want alice", got.SubAccountID)
	}
}

func TestAuth_LoggerGetsSubAccountID(t *testing.T) {
	want := domain.UserIdentity{SubAccountID: "carol"}
	r := newGinTest(TraceContext(), Recover(), Auth(
		WithIdentityProvider(stubProvider{user: &want}),
	))
	r.GET("/x", func(c *gin.Context) {
		_ = GetRequestContext(c) // Logger 字段已删；改 ctx-aware 后这个 test 不再断言 logger
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
		name       string
		headers    map[string]string
		wantNil    bool
		wantAPIKey string
		wantBearer string
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
