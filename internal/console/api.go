package console

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/policy"
)

// statusKey is the JSON field every mutation/health handler below reports its
// outcome under.
const statusKey = "status"

// statusDeleted is the outcome value shared by every hard-delete handler.
const statusDeleted = "deleted"

// apiPrefix is versioned before the first release so future incompatible
// control-plane changes can coexist with existing clients.
const apiPrefix = "/api/v1"

const maxJSONBodyBytes int64 = 1 << 20 // Control-plane mutations never need bodies larger than 1 MiB.

const apiJSONMediaType = "application/json"

// NewEngine assembles the control-plane gin.Engine: the ops route (/healthz) is
// public, everything under /api/v1/* goes through adminAuth (authenticate +
// resolve role). Write routes (POST/DELETE) additionally attach
// requireAdmin — the viewer role can only read. All business handlers depend
// only on *Store.
func NewEngine(store *Store, tokens []Token) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())

	engine.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{statusKey: "ok"}) })

	// Web UI: single-file admin console. The page itself carries no
	// secrets; auth happens on the /api/v1/* API calls it issues (the browser
	// attaches the admin token).
	engine.GET("/", func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML) })

	api := &api{store: store}
	// adminAuth authenticates + resolves role/actor first; auditWrites then
	// records the write-operation audit (this order must not be reversed).
	admin := engine.Group(apiPrefix, adminAuth(tokens), requireAdminForWrites, auditWrites(store))
	{
		// Reads: both admin and viewer are allowed
		admin.GET("/accounts", api.listAccounts)
		admin.GET("/model-services", api.listModelServices)
		admin.GET("/endpoints", api.listEndpoints)
		admin.GET("/endpoints/:id", api.getEndpoint)
		admin.GET("/accounts/:pin/api-keys", api.listAPIKeys)
		admin.GET("/quota-policies", api.listQuotaPolicies)
		admin.GET("/pricing", api.listPricing)
		admin.GET("/model-aliases", api.listModelAliases)
		admin.GET("/routing-policies", api.listRoutingPolicies)
		admin.GET("/routing-costs", api.listRoutingCosts)
		admin.GET("/policies", api.listEnforcementPolicies)
		admin.GET("/policy-bindings", api.listPolicyBindings)
		admin.GET("/audit", requireAdmin, api.listAudit) // audit is admin-only

		// Writes: admin only. Enforcement is method-driven at the group level
		// (requireAdminForWrites, attached below) rather than per-route, so a
		// newly added write route cannot silently ship reachable by a viewer.
		admin.POST("/accounts", api.createAccount)
		admin.POST("/model-services", api.createModelService)
		admin.POST("/subscriptions", api.subscribe)
		admin.POST("/endpoints", api.createEndpoint)
		admin.DELETE("/endpoints/:id", api.deleteEndpoint)
		admin.POST("/api-keys", api.createAPIKey)
		admin.DELETE("/accounts/:pin/api-keys/:keyID", api.revokeAPIKey)
		admin.POST("/quota-policies", api.createQuotaPolicy)
		admin.DELETE("/quota-policies/:id", api.deleteQuotaPolicy)
		admin.POST("/pricing", api.publishPrice)
		admin.POST("/model-aliases", api.createModelAlias)
		admin.DELETE("/model-aliases/:alias", api.deleteModelAlias)
		admin.POST("/routing-policies", api.publishRoutingPolicy)
		admin.POST("/routing-costs", api.publishRoutingCost)
		admin.POST("/routing-policies/dry-run", api.dryRunRoutingPolicy)
		admin.DELETE("/routing-policies/:policyID", api.disableRoutingPolicy)
		admin.POST("/policies", api.publishEnforcementPolicy)
		admin.POST("/policies/simulate", api.simulateEnforcementPolicy)
		admin.DELETE("/policies/:policyID", api.disableEnforcementPolicy)
		admin.POST("/policy-bindings", api.bindEnforcementPolicy)
		admin.DELETE("/policy-bindings/:scopeKind", api.deletePolicyBinding)
	}

	return engine
}

func (a *api) publishEnforcementPolicy(c *gin.Context) {
	var in EnforcementPolicyInput
	if !bind(c, &in) {
		return
	}

	view, err := a.store.PublishEnforcementPolicy(c.Request.Context(), in, c.GetString(ctxActorKey))
	if err != nil {
		writePolicyStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{"policy": view})
}

func (a *api) listEnforcementPolicies(c *gin.Context) {
	rows, err := a.store.ListEnforcementPolicies(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"policies": rows})
}

func (a *api) disableEnforcementPolicy(c *gin.Context) {
	if err := a.store.DisableEnforcementPolicy(c.Request.Context(), c.Param("policyID")); err != nil {
		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{statusKey: "disabled"})
}

func (a *api) bindEnforcementPolicy(c *gin.Context) {
	var in PolicyBindingInput
	if !bind(c, &in) {
		return
	}

	if err := a.store.BindEnforcementPolicy(c.Request.Context(), in, c.GetString(ctxActorKey)); err != nil {
		writePolicyStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{statusKey: "bound"})
}

func (a *api) listPolicyBindings(c *gin.Context) {
	rows, err := a.store.ListPolicyBindings(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"policy_bindings": rows})
}

func (a *api) deletePolicyBinding(c *gin.Context) {
	scope := policy.Scope{Kind: policy.ScopeKind(c.Param("scopeKind")), ID: c.Query("scope_id")}
	if err := a.store.DeletePolicyBinding(c.Request.Context(), scope); err != nil {
		writePolicyStoreErr(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{statusKey: "deleted"})
}

func (a *api) simulateEnforcementPolicy(c *gin.Context) {
	var in PolicySimulationInput
	if !bind(c, &in) {
		return
	}

	result, err := a.store.SimulateEnforcementPolicy(c.Request.Context(), in)
	if err != nil {
		writePolicyStoreErr(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"result": result})
}

func writePolicyStoreErr(c *gin.Context, err error) {
	var invalid *InvalidEnforcementPolicyError
	if errors.As(err, &invalid) {
		abortError(c, http.StatusBadRequest, "policy_invalid", invalid.Reason)

		return
	}

	writeStoreErr(c, err)
}

func (a *api) publishRoutingCost(c *gin.Context) {
	var in RoutingCostInput
	if !bind(c, &in) {
		return
	}

	view, err := a.store.PublishRoutingCost(c.Request.Context(), in, c.GetString(ctxActorKey))
	if err != nil {
		var invalid *InvalidRoutingCostError
		if errors.As(err, &invalid) {
			abortError(c, http.StatusBadRequest, "routing_cost_invalid", invalid.Reason)

			return
		}

		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{"routing_cost": view})
}

func (a *api) listRoutingCosts(c *gin.Context) {
	rows, err := a.store.ListRoutingCosts(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"routing_costs": rows})
}

// =============================================================================
// Virtual-model routing policies
// =============================================================================

func (a *api) publishRoutingPolicy(c *gin.Context) {
	var in RoutingPolicyInput
	if !bind(c, &in) {
		return
	}

	view, err := a.store.PublishRoutingPolicy(c.Request.Context(), in, c.GetString(ctxActorKey))
	if err != nil {
		var invalid *InvalidRoutingPolicyError
		if errors.As(err, &invalid) {
			abortError(c, http.StatusBadRequest, "routing_policy_invalid", invalid.Reason)
			return
		}

		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{"routing_policy": view})
}

func (a *api) listRoutingPolicies(c *gin.Context) {
	rows, err := a.store.ListRoutingPolicies(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"routing_policies": rows})
}

func (a *api) disableRoutingPolicy(c *gin.Context) {
	if err := a.store.DisableRoutingPolicy(c.Request.Context(), c.Param("policyID")); err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{statusKey: "disabled"})
}

func (a *api) dryRunRoutingPolicy(c *gin.Context) {
	var in RoutingDryRunInput
	if !bind(c, &in) {
		return
	}

	if in.AccountID == "" || in.RequestedModel == "" || in.Modality == nil {
		abortError(c, http.StatusBadRequest, "invalid_argument", "account_id, requested_model, and modality are required")
		return
	}

	resolution, err := a.store.DryRunRoutingPolicy(c.Request.Context(), in)
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"decision": resolution.Decision, "model_chain": resolution.Decision.EligibleModels()})
}

// Note: usage/metering aggregation is deliberately kept out of the control
// plane — the gateway is only responsible for emitting usage events via the
// outbox (file source-of-truth + Kafka broadcast), which downstream
// metering/billing systems consume. Turning the control plane into a usage
// aggregator would pull the independently complex "billing" domain in and
// break the boundary.

type api struct {
	store *Store
}

// =============================================================================
// Accounts
// =============================================================================

func (a *api) createAccount(c *gin.Context) {
	var in AccountInput
	if !bind(c, &in) {
		return
	}

	if in.Pin == "" || in.Name == "" {
		abortError(c, 400, "invalid_argument", "pin and name are required")
		return
	}

	if err := a.store.CreateAccount(c.Request.Context(), in); err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"pin": in.Pin})
}

func (a *api) listAccounts(c *gin.Context) {
	rows, err := a.store.ListAccounts(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"accounts": rows})
}

// =============================================================================
// Model services + subscriptions
// =============================================================================

func (a *api) createModelService(c *gin.Context) {
	var in ModelServiceInput
	if !bind(c, &in) {
		return
	}

	if in.ServiceID == "" || in.Model == "" {
		abortError(c, 400, "invalid_argument", "service_id and model are required")
		return
	}

	id, err := a.store.CreateModelService(c.Request.Context(), in)
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (a *api) listModelServices(c *gin.Context) {
	rows, err := a.store.ListModelServices(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"model_services": rows})
}

func (a *api) subscribe(c *gin.Context) {
	var in SubscriptionInput
	if !bind(c, &in) {
		return
	}

	if in.AccountID == "" || in.ModelServiceID == 0 {
		abortError(c, 400, "invalid_argument", "account_id and model_service_id are required")
		return
	}

	if err := a.store.Subscribe(c.Request.Context(), in); err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{statusKey: "subscribed"})
}

// =============================================================================
// Endpoints
// =============================================================================

func (a *api) createEndpoint(c *gin.Context) {
	var in EndpointInput
	if !bind(c, &in) {
		return
	}

	id, err := a.store.CreateEndpoint(c.Request.Context(), in)
	if err != nil {
		var invalid *InvalidEndpointError
		if errors.As(err, &invalid) {
			abortErrorDetails(c, http.StatusBadRequest, "endpoint_invalid", "endpoint failed validation",
				map[string]any{"reasons": invalid.Reasons})

			return
		}

		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (a *api) listEndpoints(c *gin.Context) {
	rows, err := a.store.ListEndpoints(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"endpoints": rows})
}

func (a *api) getEndpoint(c *gin.Context) {
	id, ok := pathInt64(c, "id")
	if !ok {
		return
	}

	v, err := a.store.GetEndpoint(c.Request.Context(), id)
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, v)
}

func (a *api) deleteEndpoint(c *gin.Context) {
	id, ok := pathInt64(c, "id")
	if !ok {
		return
	}

	if err := a.store.DeleteEndpoint(c.Request.Context(), id); err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{statusKey: statusDeleted})
}

// =============================================================================
// API keys
// =============================================================================

func (a *api) createAPIKey(c *gin.Context) {
	var in APIKeyInput
	if !bind(c, &in) {
		return
	}

	if in.SubAccountID == "" {
		abortError(c, 400, "invalid_argument", "sub_account_id is required")
		return
	}

	created, err := a.store.CreateAPIKey(c.Request.Context(), in)
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	// The plaintext is returned only this once.
	c.JSON(http.StatusCreated, created)
}

func (a *api) listAPIKeys(c *gin.Context) {
	rows, err := a.store.ListAPIKeys(c.Request.Context(), c.Param("pin"))
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"api_keys": rows})
}

func (a *api) revokeAPIKey(c *gin.Context) {
	if err := a.store.RevokeAPIKey(c.Request.Context(), c.Param("pin"), c.Param("keyID")); err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{statusKey: "revoked"})
}

// =============================================================================
// Quota policies
// =============================================================================

func (a *api) createQuotaPolicy(c *gin.Context) {
	var in QuotaPolicyInput
	if !bind(c, &in) {
		return
	}

	id, err := a.store.CreateQuotaPolicy(c.Request.Context(), in)
	if err != nil {
		var invalid *InvalidPolicyError
		if errors.As(err, &invalid) {
			abortError(c, 400, "policy_invalid", invalid.Reason)
			return
		}

		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (a *api) listQuotaPolicies(c *gin.Context) {
	rows, err := a.store.ListQuotaPolicies(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"quota_policies": rows})
}

func (a *api) deleteQuotaPolicy(c *gin.Context) {
	id, ok := pathInt64(c, "id")
	if !ok {
		return
	}

	if err := a.store.DeleteQuotaPolicy(c.Request.Context(), id); err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{statusKey: statusDeleted})
}

// =============================================================================
// Pricing (append-only)
// =============================================================================

func (a *api) publishPrice(c *gin.Context) {
	var in PricingInput
	if !bind(c, &in) {
		return
	}

	id, err := a.store.PublishPrice(c.Request.Context(), in)
	if err != nil {
		var invalid *InvalidPricingError
		if errors.As(err, &invalid) {
			abortError(c, 400, "pricing_invalid", invalid.Reason)
			return
		}

		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (a *api) listPricing(c *gin.Context) {
	q := PricingQuery{AccountID: c.Query("account_id")}
	if value, present := c.GetQuery("active"); present {
		switch value {
		case "true":
			q.ActiveOnly = true
		case "false":
		default:
			abortError(c, http.StatusBadRequest, "invalid_argument", "active must be true or false")

			return
		}
	}

	if v := c.Query("model_service_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			abortError(c, http.StatusBadRequest, "invalid_argument", "model_service_id must be a positive integer")

			return
		}

		q.ModelServiceID = n
	}

	rows, err := a.store.ListPricing(c.Request.Context(), q)
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"pricing": rows})
}

// =============================================================================
// Model aliases
// =============================================================================

func (a *api) createModelAlias(c *gin.Context) {
	var in ModelAliasInput
	if !bind(c, &in) {
		return
	}

	if err := a.store.CreateModelAlias(c.Request.Context(), in); err != nil {
		var invalid *InvalidAliasError
		if errors.As(err, &invalid) {
			abortError(c, 400, "alias_invalid", invalid.Reason)
			return
		}

		writeStoreErr(c, err)

		return
	}

	c.JSON(http.StatusCreated, gin.H{"alias": in.Alias})
}

func (a *api) listModelAliases(c *gin.Context) {
	rows, err := a.store.ListModelAliases(c.Request.Context())
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"model_aliases": rows})
}

func (a *api) deleteModelAlias(c *gin.Context) {
	if err := a.store.DeleteModelAlias(c.Request.Context(), c.Param("alias")); err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{statusKey: statusDeleted})
}

// =============================================================================
// Audit
// =============================================================================

func (a *api) listAudit(c *gin.Context) {
	limit := 100
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			abortError(c, http.StatusBadRequest, "invalid_argument", "limit must be an integer between 1 and 1000")

			return
		}

		limit = n
	}

	rows, err := a.store.ListAudit(c.Request.Context(), limit)
	if err != nil {
		writeStoreErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"audit": rows})
}

// =============================================================================
// helpers
// =============================================================================

// bind parses one strict JSON value. Unknown fields, trailing JSON values,
// oversized bodies, and non-JSON media types are rejected rather than silently
// changing the meaning of a control-plane mutation.
func bind(c *gin.Context, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != apiJSONMediaType {
		abortError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be "+apiJSONMediaType)

		return false
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		abortError(c, 400, "invalid_json", err.Error())

		return false
	}

	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		abortError(c, 400, "invalid_json", "request body must contain exactly one JSON value")

		return false
	}

	return true
}

func pathInt64(c *gin.Context, name string) (int64, bool) {
	v, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil {
		abortError(c, 400, "invalid_argument", name+" must be an integer")
		return 0, false
	}

	return v, true
}

// writeStoreErr translates a store error to HTTP: NotFound -> 404, unique-key
// conflict -> 409, everything else -> 500. Internal error details only go to
// the log layer (gin.Recovery / slog), never into the client response body.
func writeStoreErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		abortError(c, 404, "not_found", "resource not found")
	case isDuplicateKey(err):
		abortError(c, 409, "conflict", "resource already exists (unique key violation)")
	case isForeignKeyViolation(err):
		// Referencing a resource that doesn't exist (e.g. subscribing to a
		// nonexistent model, issuing a key to a nonexistent account) is a
		// **client** input error, not a server-side fault.
		abortError(c, 400, "invalid_reference", "references a resource that does not exist")
	default:
		abortError(c, 500, "internal", "internal error")
	}
}

// isForeignKeyViolation recognizes MySQL error 1452 (foreign key constraint failure).
func isForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}

	s := err.Error()

	return containsAny(s, "foreign key constraint fails", "Error 1452")
}

// isDuplicateKey recognizes MySQL error 1062 (unique-key violation), by
// matching the error string rather than depending on a concrete driver type.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}

	s := err.Error()

	return containsAny(s, "Duplicate entry", "Error 1062")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}

	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}

	return -1
}

type apiErrorResponse struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// abortError writes the stable control-plane error envelope.
func abortError(c *gin.Context, status int, code, message string) {
	abortErrorDetails(c, status, code, message, nil)
}

func abortErrorDetails(c *gin.Context, status int, code, message string, details map[string]any) {
	c.AbortWithStatusJSON(status, apiErrorResponse{Error: apiErrorBody{Code: code, Message: message, Details: details}})
}
