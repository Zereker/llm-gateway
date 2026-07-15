package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestControlPlaneAPIVersionBoundary(t *testing.T) {
	engine := NewEngine(NewStore(nil), []Token{{Value: testToken, Role: RoleAdmin}})

	old := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	old.Header.Set("Authorization", "Bearer "+testToken)
	oldResponse := httptest.NewRecorder()
	engine.ServeHTTP(oldResponse, old)
	if oldResponse.Code != http.StatusNotFound {
		t.Fatalf("unversioned route status = %d, want 404", oldResponse.Code)
	}

	current := httptest.NewRequest(http.MethodGet, apiPrefix+"/accounts", nil)
	currentResponse := httptest.NewRecorder()
	engine.ServeHTTP(currentResponse, current)
	assertAPIError(t, currentResponse, http.StatusUnauthorized, "unauthorized")
}

func TestControlPlaneStrictJSONContract(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{name: "unknown field", contentType: apiJSONMediaType, body: `{"pin":"a","unknown":true}`, status: 400, code: "invalid_json"},
		{name: "json charset accepted", contentType: apiJSONMediaType + "; charset=utf-8", body: `{"pin":"a","unknown":true}`, status: 400, code: "invalid_json"},
		{name: "trailing value", contentType: apiJSONMediaType, body: `{"pin":"a"}{"pin":"b"}`, status: 400, code: "invalid_json"},
		{name: "wrong media type", contentType: "text/plain", body: `{"pin":"a"}`, status: 415, code: "unsupported_media_type"},
		{name: "oversized", contentType: apiJSONMediaType, body: `{"pin":"` + strings.Repeat("a", int(maxJSONBodyBytes)) + `"}`, status: 400, code: "invalid_json"},
	}

	engine := NewEngine(NewStore(nil), []Token{{Value: testToken, Role: RoleAdmin}})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, apiPrefix+"/accounts", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", tt.contentType)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)
			assertAPIError(t, response, tt.status, tt.code)
		})
	}
}

func TestControlPlaneRejectsInvalidQueryValues(t *testing.T) {
	tests := []struct {
		path    string
		message string
	}{
		{path: apiPrefix + "/pricing?model_service_id=abc", message: "model_service_id"},
		{path: apiPrefix + "/pricing?model_service_id=0", message: "model_service_id"},
		{path: apiPrefix + "/pricing?active=yes", message: "active"},
		{path: apiPrefix + "/audit?limit=0", message: "limit"},
		{path: apiPrefix + "/audit?limit=1001", message: "limit"},
		{path: apiPrefix + "/audit?limit=lots", message: "limit"},
	}

	engine := NewEngine(NewStore(nil), []Token{{Value: testToken, Role: RoleAdmin}})
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)
			body := assertAPIError(t, response, http.StatusBadRequest, "invalid_argument")
			if !strings.Contains(body.Error.Message, tt.message) {
				t.Fatalf("error message = %q, want %q", body.Error.Message, tt.message)
			}
		})
	}
}

func TestControlPlaneErrorDetailsUseStableEnvelope(t *testing.T) {
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)

	abortErrorDetails(ctx, http.StatusBadRequest, "endpoint_invalid", "endpoint failed validation",
		map[string]any{"reasons": []string{"invalid_url"}})

	body := assertAPIError(t, response, http.StatusBadRequest, "endpoint_invalid")
	reasons, ok := body.Error.Details["reasons"].([]any)
	if !ok || len(reasons) != 1 || reasons[0] != "invalid_url" {
		t.Fatalf("error details = %#v", body.Error.Details)
	}
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) apiErrorResponse {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body.String())
	}

	var body apiErrorResponse
	decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, response.Body.String())
	}
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("error = %+v, want code %q and a message", body.Error, code)
	}

	return body
}
