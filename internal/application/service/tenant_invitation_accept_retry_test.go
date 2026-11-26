package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// flakyAddMemberService fails AddMember a configurable number of times before
// delegating to the real membership service.
type flakyAddMemberService struct {
	interfaces.TenantMemberService
	failures int
}

func (s *flakyAddMemberService) AddMember(
	ctx context.Context, userID string, tenantID uint64, role types.TenantRole, invitedBy *string,
) (*types.TenantMember, error) {
	if s.failures > 0 {
		s.failures--
		return nil, errors.New("transient membership write failure")
	}
	return s.TenantMemberService.AddMember(ctx, userID, tenantID, role, invitedBy)
}

// A transient AddMember failure must leave the invitation pending so the
// invitee can retry. The old ordering marked the invitation accepted first,
// consuming it while the user never joined the workspace.
func TestInvitationAcceptRetryAfterAddMemberFailure(t *testing.T) {
	invRepo := newFakeInvitationRepo()
	memberSvc, _ := newServiceWithRepo()
	flaky := &flakyAddMemberService{TenantMemberService: memberSvc, failures: 1}
	svc := NewTenantInvitationService(invRepo, flaky, nil)
	ctx := context.Background()

	inv, err := svc.Create(ctx, 1, "u-bob", types.TenantRoleContributor, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Accept(ctx, inv.ID, "u-bob"); err == nil {
		t.Fatal("first accept must surface the membership write failure")
	}

	row, err := invRepo.GetByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("load invitation: %v", err)
	}
	if row.Status != types.TenantInvitationStatusPending {
		t.Fatalf("invitation status = %q, want pending after a failed accept", row.Status)
	}

	member, err := svc.Accept(ctx, inv.ID, "u-bob")
	if err != nil {
		t.Fatalf("retry accept: %v", err)
	}
	if member == nil || member.UserID != "u-bob" || member.TenantID != 1 {
		t.Fatalf("unexpected membership after retry: %+v", member)
	}
	row, _ = invRepo.GetByID(ctx, inv.ID)
	if row.Status != types.TenantInvitationStatusAccepted {
		t.Fatalf("invitation status = %q, want accepted", row.Status)
	}
	got, _ := memberSvc.GetMembership(ctx, "u-bob", 1)
	if got == nil {
		t.Fatal("membership row missing after retry")
	}
}

// revokeRacingRepo simulates the admin's revoke committing between Accept's
// read and its conditional claim.
type revokeRacingRepo struct {
	*fakeInvitationRepo
}

func (r *revokeRacingRepo) MarkStatusIfPending(
	ctx context.Context, id uint64, status types.TenantInvitationStatus, respondedAt time.Time,
) error {
	if status == types.TenantInvitationStatusAccepted {
		if err := r.fakeInvitationRepo.MarkStatusIfPending(
			ctx, id, types.TenantInvitationStatusRevoked, respondedAt); err != nil {
			return err
		}
		return gormErrRecordNotFound
	}
	return r.fakeInvitationRepo.MarkStatusIfPending(ctx, id, status, respondedAt)
}

// A revoke that lands first must win outright: the accept must not create a
// membership the administrator just withdrew.
func TestInvitationAcceptLosesToAConcurrentRevoke(t *testing.T) {
	base := newFakeInvitationRepo()
	racing := &revokeRacingRepo{fakeInvitationRepo: base}
	memberSvc, _ := newServiceWithRepo()
	svc := NewTenantInvitationService(racing, memberSvc, nil)
	ctx := context.Background()

	inv, err := svc.Create(ctx, 1, "u-bob", types.TenantRoleContributor, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Accept(ctx, inv.ID, "u-bob"); !errors.Is(err, ErrInvitationNotPending) {
		t.Fatalf("accept = %v, want ErrInvitationNotPending", err)
	}
	if member, _ := memberSvc.GetMembership(ctx, "u-bob", 1); member != nil {
		t.Fatalf("membership %+v must not exist after the revoke won", member)
	}
	row, _ := base.GetByID(ctx, inv.ID)
	if row.Status != types.TenantInvitationStatusRevoked {
		t.Fatalf("invitation status = %q, want revoked", row.Status)
	}
}
