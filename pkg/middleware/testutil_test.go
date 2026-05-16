package middleware

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// 防 unused import 警告（time 在 stubBudgetGate 等地方间接被用到）
var _ = time.Now

// testutil_test.go 提供本包所有 _test.go 共享的 stub 与构造 helper。
//
// 风格约定（对齐 auth_test.go / tracing_test.go）：
//   - 手写 stub，零外部 mock 库依赖
//   - 每个 stub 内嵌可配置「返回值字段 + 调用计数」便于断言
//   - 并发场景用 sync.Mutex / atomic 保护
//
// **不要**在 testutil_test.go 里写业务断言；它只提供工具。

// =============================================================================
// 基础 helper
// =============================================================================

// newTestRC 构造一个带 Identity + Envelope shell 的 RequestContext，用于直接调
// middleware 内部函数的 lab-style 测试（不走 gin.Engine）。
//
// 默认 Identity.AccountID="acc1" / SubAccountID="sub1" / APIKeyID="ak1" / Group="default"；
// Envelope.SourceProtocol=ProtoOpenAI / Modality=ModalityChat / Model=model。
func newTestRC(model string) *domain.RequestContext {
	ctx := context.Background()
	rc := &domain.RequestContext{
		RequestID: "req_test",
		StartTime: time.Now(),
		Ctx:       ctx,
		Extras:    make(map[string]any),
		Identity: domain.UserIdentity{
			AccountID:    "acc1",
			SubAccountID: "sub1",
			APIKeyID:     "ak1",
			Group:        "default",
		},
		Envelope: &domain.RequestEnvelope{
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
			Model:          model,
			RawBytes:       []byte(`{"model":"` + model + `"}`),
		},
	}
	return rc
}

// withGinCtx 把 rc 注入一个新 *gin.Context（test mode），返回 ctx + recorder。
// 不走中间件链；适合需要 *gin.Context 但只想测 helper / middleware body 的场景。
func withGinCtx(rc *domain.RequestContext) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	AttachRequestContext(c, rc)
	return c, w
}

// =============================================================================
// Stub: BudgetGate
// =============================================================================

type stubBudgetGate struct {
	status   domain.BudgetStatus
	err      error
	calls    atomic.Int32
	lastUser string
	mu       sync.Mutex
}

func (g *stubBudgetGate) Check(_ context.Context, subAccountID string) (domain.BudgetStatus, error) {
	g.calls.Add(1)
	g.mu.Lock()
	g.lastUser = subAccountID
	g.mu.Unlock()
	return g.status, g.err
}

// =============================================================================
// Stub: ModelCatalog / SubscriptionChecker（M5 用，middleware-owned 接口）
// =============================================================================

type stubCatalog struct {
	ms  *domain.ModelService
	err error
}

func (s stubCatalog) GetByModel(_ context.Context, _ string) (*domain.ModelService, error) {
	return s.ms, s.err
}

type stubSubs struct {
	has bool
	err error
}

func (s stubSubs) HasModel(_ context.Context, _ string, _ int64) (bool, error) {
	return s.has, s.err
}

// =============================================================================
// Stub: ratelimit.Store
// =============================================================================

// stubStore 可配置 ReserveBatch / ChargeBatch / SnapshotBatch 返回值。
//
// 默认行为：ReserveBatch allowed=true；ChargeBatch 无错；SnapshotBatch 返回零值。
type stubStore struct {
	reserveAllowed bool
	reserveViol    *ratelimit.BucketViolation
	reserveErr     error

	chargeResults []ratelimit.BucketChargeResult
	chargeErr     error
	chargeCalls   atomic.Int32
	chargeCaptured [][]ratelimit.Bucket
	chargeMu      sync.Mutex

	snapshotResults []ratelimit.BucketState
	snapshotErr     error

	reserveCalls    atomic.Int32
	reserveCaptured [][]ratelimit.Bucket
	reserveMu       sync.Mutex
}

func newStubStoreAllowAll() *stubStore {
	return &stubStore{reserveAllowed: true}
}

func (s *stubStore) ReserveBatch(_ context.Context, buckets []ratelimit.Bucket) (bool, *ratelimit.BucketViolation, error) {
	s.reserveCalls.Add(1)
	s.reserveMu.Lock()
	cp := make([]ratelimit.Bucket, len(buckets))
	copy(cp, buckets)
	s.reserveCaptured = append(s.reserveCaptured, cp)
	s.reserveMu.Unlock()
	return s.reserveAllowed, s.reserveViol, s.reserveErr
}

func (s *stubStore) ChargeBatch(_ context.Context, buckets []ratelimit.Bucket) ([]ratelimit.BucketChargeResult, error) {
	s.chargeCalls.Add(1)
	s.chargeMu.Lock()
	cp := make([]ratelimit.Bucket, len(buckets))
	copy(cp, buckets)
	s.chargeCaptured = append(s.chargeCaptured, cp)
	s.chargeMu.Unlock()
	if s.chargeResults != nil {
		return s.chargeResults, s.chargeErr
	}
	// 默认每个 bucket 返一条 ok 结果（Overflow=false）
	out := make([]ratelimit.BucketChargeResult, len(buckets))
	for i, b := range buckets {
		out[i] = ratelimit.BucketChargeResult{Key: b.Key, Used: b.Cost, Limit: b.Limit}
	}
	return out, s.chargeErr
}

func (s *stubStore) SnapshotBatch(_ context.Context, buckets []ratelimit.Bucket) ([]ratelimit.BucketState, error) {
	if s.snapshotResults != nil {
		return s.snapshotResults, s.snapshotErr
	}
	out := make([]ratelimit.BucketState, len(buckets))
	for i, b := range buckets {
		out[i] = ratelimit.BucketState{Key: b.Key, Limit: b.Limit}
	}
	return out, s.snapshotErr
}

// =============================================================================
// Stub: QuotaPolicyProvider（PolicyCache 上游）
// =============================================================================

type stubQPProvider struct {
	policies map[int64]*repo.QuotaPolicy
	err      error
	calls    atomic.Int32
}

func (p *stubQPProvider) GetByID(_ context.Context, id int64) (*repo.QuotaPolicy, error) {
	p.calls.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	if pol, ok := p.policies[id]; ok {
		return pol, nil
	}
	return nil, nil
}

// =============================================================================
// Stub: Moderator
// =============================================================================

type stubModerator struct {
	checkInputErr  error
	checkOutputErr error
	inputCalls     atomic.Int32
	outputCalls    atomic.Int32
	lastInputModel string
	mu             sync.Mutex
}

func (m *stubModerator) CheckInput(_ context.Context, env *domain.RequestEnvelope) error {
	m.inputCalls.Add(1)
	m.mu.Lock()
	if env != nil {
		m.lastInputModel = env.Model
	}
	m.mu.Unlock()
	return m.checkInputErr
}

func (m *stubModerator) CheckOutput(_ context.Context, _ []byte) error {
	m.outputCalls.Add(1)
	return m.checkOutputErr
}
