package service

import (
	"context"
	"errors"
	"testing"

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
