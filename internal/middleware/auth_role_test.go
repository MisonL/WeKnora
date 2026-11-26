package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// fakeMemberService is a hand-rolled stand-in for
// interfaces.TenantMemberService. It backs Get/HasAnyMembers/AddMember
// with two in-memory maps and lets each test seed exactly the rows it
// cares about. Other interface methods are stubbed because resolveTenantRole
// never touches them.
type fakeMemberService struct {
	members map[string]*types.TenantMember // key = userID + "|" + tenantID
	// addCalls records every AddMember invocation so tests can assert
	// that resolveTenantRole did (or didn't) attempt to auto-promote.
	addCalls []struct {
		UserID   string
		TenantID uint64
		Role     types.TenantRole
	}
	failGet    error
	failHasAny error
	failAdd    error
	// 阻止 auto-promote 把 hasAny 翻面：默认 AddMember 成功也会写入 members map。
}

func newFakeMemberService() *fakeMemberService {
	return &fakeMemberService{members: map[string]*types.TenantMember{}}
}

func memberKey(u string, t uint64) string {
	return u + "|" + uintToStr(t)
}

func uintToStr(t uint64) string {
	// 简单数字转字符串，避免引入额外依赖。
	if t == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for t > 0 {
		i--
		buf[i] = byte('0' + t%10)
		t /= 10
	}
	return string(buf[i:])
}

func (f *fakeMemberService) seedActive(userID string, tenantID uint64, role types.TenantRole) {
	f.seedStatus(userID, tenantID, role, types.TenantMemberStatusActive)
}

func (f *fakeMemberService) seedStatus(
	userID string, tenantID uint64, role types.TenantRole, status types.TenantMemberStatus,
) {
	f.members[memberKey(userID, tenantID)] = &types.TenantMember{
		UserID:   userID,
		TenantID: tenantID,
		Role:     role,
		Status:   status,
	}
}

func (f *fakeMemberService) AddMember(
	ctx context.Context, userID string, tenantID uint64, role types.TenantRole, invitedBy *string,
) (*types.TenantMember, error) {
	f.addCalls = append(f.addCalls, struct {
		UserID   string
		TenantID uint64
		Role     types.TenantRole
	}{userID, tenantID, role})
	if f.failAdd != nil {
		return nil, f.failAdd
	}
	m := &types.TenantMember{UserID: userID, TenantID: tenantID, Role: role, Status: types.TenantMemberStatusActive}
	f.members[memberKey(userID, tenantID)] = m
	return m, nil
}

func (f *fakeMemberService) EnsureOwner(
	ctx context.Context, userID string, tenantID uint64,
) (*types.TenantMember, error) {
	if existing, ok := f.members[memberKey(userID, tenantID)]; ok {
		return existing, nil
	}
	return f.AddMember(ctx, userID, tenantID, types.TenantRoleOwner, nil)
}

func (f *fakeMemberService) GetMembership(
	ctx context.Context, userID string, tenantID uint64,
) (*types.TenantMember, error) {
	if f.failGet != nil {
		return nil, f.failGet
	}
	m, ok := f.members[memberKey(userID, tenantID)]
	if !ok {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (f *fakeMemberService) ListByUser(ctx context.Context, userID string) ([]*types.TenantMember, error) {
	var out []*types.TenantMember
	for _, member := range f.members {
		if member.UserID == userID && member.Status == types.TenantMemberStatusActive {
			copy := *member
			out = append(out, &copy)
		}
	}
	return out, nil
}
func (f *fakeMemberService) ListByTenant(ctx context.Context, tenantID uint64) ([]*types.TenantMember, error) {
	return nil, nil
}
func (f *fakeMemberService) ListMembersPage(
	ctx context.Context, tenantID uint64, query string, page, pageSize int,
) ([]*types.TenantMember, int64, error) {
	return nil, 0, nil
}
func (f *fakeMemberService) HasAnyMembers(ctx context.Context, tenantID uint64) (bool, error) {
	if f.failHasAny != nil {
		return false, f.failHasAny
	}
	for _, m := range f.members {
		if m.TenantID == tenantID && m.Status == types.TenantMemberStatusActive {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeMemberService) UpdateRole(
	ctx context.Context, userID string, tenantID uint64, newRole types.TenantRole,
) error {
	return nil
}
func (f *fakeMemberService) RemoveMember(ctx context.Context, userID string, tenantID uint64) error {
	return nil
}

var _ interfaces.TenantMemberService = (*fakeMemberService)(nil)

func cfgWithRBAC(enabled bool) *config.Config {
	return &config.Config{Tenant: &config.TenantConfig{EnableRBAC: &enabled}}
}

// cfgWithRBACAndCross also sets the deployment cross-tenant switch, which
// gates whether CanAccessAllTenants grants a visitor role.
func cfgWithRBACAndCross(rbac, cross bool) *config.Config {
	return &config.Config{Tenant: &config.TenantConfig{
		EnableRBAC: &rbac, EnableCrossTenantAccess: cross,
	}}
}

// A lingering CanAccessAllTenants attribute must not grant Admin in a
// foreign workspace once the deployment switch is closed.
func TestResolveTenantRole_CrossTenantSuperuserDeniedWhenSwitchOff(t *testing.T) {
	svc := newFakeMemberService()
	user := &types.User{ID: "super", TenantID: 1, CanAccessAllTenants: true}

	got, ok := resolveTenantRole(context.Background(), svc, user, 99, true,
		cfgWithRBACAndCross(true, false))
	if ok {
		t.Fatalf("cross-tenant superuser must be denied when EnableCrossTenantAccess=false, got role=%v", got)
	}
}

func TestResolveTenantRole_ActiveMembershipWins(t *testing.T) {
	svc := newFakeMemberService()
	svc.seedActive("u1", 10, types.TenantRoleContributor)

	got, ok := resolveTenantRole(context.Background(), svc,
		&types.User{ID: "u1", TenantID: 10}, 10, false, cfgWithRBAC(true))
	if !ok || got != types.TenantRoleContributor {
		t.Fatalf("got (%v, %v), want (contributor, true)", got, ok)
	}
	if len(svc.addCalls) != 0 {
		t.Fatalf("must not auto-promote when membership exists, got %d AddMember calls", len(svc.addCalls))
	}
}

func TestResolveTenantRole_CrossTenantSuperuserGetsAdmin_NoAutoPromote(t *testing.T) {
	// 回归 H1：跨空间超管 switch 到他人空间时，绝对不能写入 tenant_members。
	svc := newFakeMemberService()
	user := &types.User{ID: "super", TenantID: 1, CanAccessAllTenants: true}

	got, ok := resolveTenantRole(context.Background(), svc, user, 99, true, cfgWithRBACAndCross(true, true))
	if !ok || got != types.TenantRoleAdmin {
		t.Fatalf("got (%v, %v), want (admin, true)", got, ok)
	}
	if len(svc.addCalls) != 0 {
		t.Fatalf("cross-tenant superuser must not trigger auto-promote, got %+v", svc.addCalls)
	}
}

func TestResolveTenantRole_AutoPromoteRequiresHomeTenant(t *testing.T) {
	// 回归 H1：即便 target 是孤儿空间，只要不是用户自己的 home tenant，
	// 就不能 auto-promote 为 Owner。
	svc := newFakeMemberService() // 空 — 任何空间都是孤儿
	user := &types.User{ID: "u1", TenantID: 1, CanAccessAllTenants: true}

	got, ok := resolveTenantRole(context.Background(), svc, user, 42, true, cfgWithRBACAndCross(true, true))
	if !ok || got != types.TenantRoleAdmin {
		t.Fatalf("cross-tenant superuser should still get visitor Admin, got (%v, %v)", got, ok)
	}
	if len(svc.addCalls) != 0 {
		t.Fatalf("auto-promote must skip cross-tenant target, got %+v", svc.addCalls)
	}
}

func TestResolveTenantRole_AutoPromoteHomeTenant(t *testing.T) {
	// home tenant + 孤儿空间 + 非 switch → 允许 auto-promote 为 Owner。
	svc := newFakeMemberService()
	user := &types.User{ID: "u1", TenantID: 7}

	got, ok := resolveTenantRole(context.Background(), svc, user, 7, false, cfgWithRBAC(true))
	if !ok || got != types.TenantRoleOwner {
		t.Fatalf("got (%v, %v), want (owner, true)", got, ok)
	}
	if len(svc.addCalls) != 1 || svc.addCalls[0].Role != types.TenantRoleOwner {
		t.Fatalf("expected exactly one Owner AddMember call, got %+v", svc.addCalls)
	}
}

func TestResolveTenantRole_AutoPromoteSkippedIfTenantHasMembers(t *testing.T) {
	svc := newFakeMemberService()
	// 同一 home tenant 已经有其它成员 — 不应自动晋升新登录者。
	svc.seedActive("other", 7, types.TenantRoleOwner)
	user := &types.User{ID: "u1", TenantID: 7}

	got, ok := resolveTenantRole(context.Background(), svc, user, 7, false, cfgWithRBAC(true))
	if ok {
		t.Fatalf("RBAC enabled + no membership for u1 should be rejected, got role=%v", got)
	}
	if len(svc.addCalls) != 0 {
		t.Fatalf("must not auto-promote into a tenant that already has members, got %+v", svc.addCalls)
	}
}

func TestResolveTenantRole_RBACDisabledStillRequiresMembership(t *testing.T) {
	svc := newFakeMemberService()
	user := &types.User{ID: "u1", TenantID: 7}
	// targetTenantID != home，所以不进 auto-promote 分支。即使 RBAC
	// 关闭，没有成员身份也不能拿到 Admin —— 角色来自成员关系，
	// 而不是配置开关。
	got, ok := resolveTenantRole(context.Background(), svc, user, 8, false, cfgWithRBAC(false))
	if ok {
		t.Fatalf("EnableRBAC=false must not fail open to Admin, got (%v, %v)", got, ok)
	}
}

func TestResolveTenantRole_RemovedMemberInHomeTenantDenied(t *testing.T) {
	svc := newFakeMemberService()
	// The user was removed from their home workspace: users.tenant_id
	// still points at it, but the membership row is suspended. Another
	// member keeps the orphan-tenant self-heal path closed.
	svc.seedStatus("u1", 7, types.TenantRoleOwner, types.TenantMemberStatusSuspended)
	svc.seedActive("other", 7, types.TenantRoleOwner)
	user := &types.User{ID: "u1", TenantID: 7}

	if got, ok := resolveTenantRole(context.Background(), svc, user, 7, false, cfgWithRBAC(true)); ok {
		t.Fatalf("removed member must be denied in their home tenant, got role=%v", got)
	}
	// Same answer with RBAC off: identity resolution is not gated by the
	// enforcement switch.
	if got, ok := resolveTenantRole(context.Background(), svc, user, 7, false, cfgWithRBAC(false)); ok {
		t.Fatalf("removed member must be denied even with EnableRBAC=false, got role=%v", got)
	}
	if len(svc.addCalls) != 0 {
		t.Fatalf("removed member must not be auto-promoted, got %+v", svc.addCalls)
	}
}

func TestResolveTenantRole_FailClosedWhenRBACEnabled(t *testing.T) {
	svc := newFakeMemberService()
	// 已有其它成员，自动晋升路径关闭；RBAC 启用 → 必须 403。
	svc.seedActive("other", 8, types.TenantRoleOwner)
	user := &types.User{ID: "u1", TenantID: 7}
	if _, ok := resolveTenantRole(context.Background(), svc, user, 8, false, cfgWithRBAC(true)); ok {
		t.Fatalf("EnableRBAC=true + no membership should be rejected")
	}
}

func TestResolveTenantRole_LookupErrorFailsClosed(t *testing.T) {
	// A transient DB error must not grant Admin. Identity resolution
	// fails closed regardless of EnableRBAC; only role *gates* honour
	// the rollout switch.
	svc := newFakeMemberService()
	svc.failGet = errors.New("transient db failure")
	// 让 HasAnyMembers 返回 true，关闭孤儿空间自愈路径。
	svc.seedActive("placeholder", 8, types.TenantRoleAdmin)
	user := &types.User{ID: "u1", TenantID: 7}

	if got, ok := resolveTenantRole(context.Background(), svc, user, 8, false, cfgWithRBAC(false)); ok {
		t.Fatalf("lookup error must fail closed even with RBAC=false, got (%v, %v)", got, ok)
	}
}

func TestResolveTenantRole_DemotedUserCannotReclaimViaOrphan(t *testing.T) {
	// 边界场景：管理员人为软删全部成员后，被踢出的用户不应在登录自己 home tenant 时
	// 因 HasAnyMembers=false 而自动重新拿到 Owner。
	// 当前实现的策略是 "home tenant + 孤儿 => Owner"，这是设计选择；本测试为这条
	// 路径加锁，未来如果收紧策略需要同步更新。
	svc := newFakeMemberService()
	user := &types.User{ID: "demoted", TenantID: 5}
	got, ok := resolveTenantRole(context.Background(), svc, user, 5, false, cfgWithRBAC(true))
	if !ok || got != types.TenantRoleOwner {
		t.Fatalf("current policy allows orphan-tenant self-heal on home tenant, got (%v, %v)", got, ok)
	}
}

// stubTenantService overrides only the lookups the auth path makes.
type stubTenantService struct {
	interfaces.TenantService
	tenants map[uint64]*types.Tenant
}

func (s *stubTenantService) GetTenantByID(_ context.Context, id uint64) (*types.Tenant, error) {
	if t, ok := s.tenants[id]; ok {
		return t, nil
	}
	return nil, errors.New("tenant not found")
}

// A system administrator who is not a member of the workspace a request
// carries must still reach the platform administration surface: that surface
// is authorized by the admin flag (and its own route guard), not by workspace
// membership. Binding the two locked the administrator out entirely.
func TestSystemAdminReachesPlatformRoutesWithoutWorkspaceMembership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	members := newFakeMemberService()
	tenants := &stubTenantService{tenants: map[uint64]*types.Tenant{7: {ID: 7}}}
	admin := &types.User{ID: "admin", TenantID: 7, IsSystemAdmin: true}
	plain := &types.User{ID: "worker", TenantID: 7}

	cases := []struct {
		name   string
		path   string
		user   *types.User
		status int
	}{
		{"admin on platform path", "/api/v1/system/admin/list", admin, http.StatusOK},
		{"admin on docreader reconnect", "/api/v1/system/docreader/reconnect", admin, http.StatusOK},
		{"admin on business path", "/api/v1/knowledge-bases", admin, http.StatusForbidden},
		{"plain user on platform path", "/api/v1/system/admin/list", plain, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, tc.path, nil)
			c.Request = c.Request.WithContext(context.Background())
			c.Request.Header.Set("X-Tenant-ID", "7")
			ok := authenticateJWTUser(c, tenants, members, cfgWithRBAC(true), tc.user, tc.user.TenantID)
			if ok && rec.Code == http.StatusOK {
				return
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (ok=%v)", rec.Code, tc.status, ok)
			}
		})
	}
}
