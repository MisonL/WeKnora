package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type stubEmbedChannelRepo struct {
	interfaces.EmbedChannelRepository
	ch *types.EmbedChannel
}

func (r *stubEmbedChannelRepo) GetByID(_ context.Context, id string) (*types.EmbedChannel, error) {
	if r.ch == nil || r.ch.ID != id {
		return nil, nil
	}
	cp := *r.ch
	return &cp, nil
}

func (r *stubEmbedChannelRepo) Update(_ context.Context, ch *types.EmbedChannel) error {
	cp := *ch
	r.ch = &cp
	return nil
}

func TestEmbedChannelUpdateAgentID(t *testing.T) {
	repo := &stubEmbedChannelRepo{
		ch: &types.EmbedChannel{
			ID:       "ch-1",
			TenantID: 42,
			AgentID:  "agent-old",
			Name:     "Support",
		},
	}
	svc := &embedChannelService{
		repo: repo,
		agentService: &stubAgentForEmbed{
			agent: &types.CustomAgent{ID: "agent-new", TenantID: 42},
		},
	}
	enabled := true
	updated, err := svc.Update(
		context.Background(),
		42,
		"ch-1",
		&types.EmbedChannelUpdate{AgentID: strPtr("agent-new"), Enabled: &enabled},
	)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.AgentID != "agent-new" {
		t.Fatalf("updated.AgentID = %q, want agent-new", updated.AgentID)
	}
	if repo.ch.AgentID != "agent-new" {
		t.Fatalf("persisted AgentID = %q, want agent-new", repo.ch.AgentID)
	}
}

func strPtr(v string) *string { return &v }

// A partial update must touch only the fields it carries: a toggle that
// sends one styling field must not clear the others.
func TestEmbedChannelUpdateKeepsOmittedFields(t *testing.T) {
	cases := []struct {
		name   string
		update types.EmbedChannelUpdate
		check  func(t *testing.T, ch *types.EmbedChannel)
	}{
		{
			name:   "welcome message only",
			update: types.EmbedChannelUpdate{WelcomeMessage: strPtr("hi")},
			check: func(t *testing.T, ch *types.EmbedChannel) {
				if ch.WelcomeMessage != "hi" {
					t.Fatalf("WelcomeMessage = %q, want hi", ch.WelcomeMessage)
				}
				if ch.PrimaryColor != "#00ff00" || ch.PageTitle != "My Page" {
					t.Fatalf("styling cleared: color=%q title=%q", ch.PrimaryColor, ch.PageTitle)
				}
				if ch.HeaderTitleMode != "session" {
					t.Fatalf("HeaderTitleMode = %q, want session", ch.HeaderTitleMode)
				}
			},
		},
		{
			name:   "explicit empty primary color clears it",
			update: types.EmbedChannelUpdate{PrimaryColor: strPtr("")},
			check: func(t *testing.T, ch *types.EmbedChannel) {
				if ch.PrimaryColor != "" {
					t.Fatalf("PrimaryColor = %q, want empty", ch.PrimaryColor)
				}
				if ch.WelcomeMessage != "old welcome" || ch.PageTitle != "My Page" {
					t.Fatalf("other styling cleared: welcome=%q title=%q", ch.WelcomeMessage, ch.PageTitle)
				}
			},
		},
		{
			name:   "empty update keeps everything",
			update: types.EmbedChannelUpdate{},
			check: func(t *testing.T, ch *types.EmbedChannel) {
				if ch.WelcomeMessage != "old welcome" || ch.PrimaryColor != "#00ff00" ||
					ch.PageTitle != "My Page" || ch.HeaderTitleMode != "session" {
					t.Fatalf("empty update cleared fields: %+v", ch)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubEmbedChannelRepo{
				ch: &types.EmbedChannel{
					ID:              "ch-1",
					TenantID:        42,
					AgentID:         "agent-old",
					Name:            "Support",
					WelcomeMessage:  "old welcome",
					PrimaryColor:    "#00ff00",
					PageTitle:       "My Page",
					HeaderTitleMode: "session",
				},
			}
			svc := &embedChannelService{
				repo: repo,
				agentService: &stubAgentForEmbed{
					agent: &types.CustomAgent{ID: "agent-old", TenantID: 42},
				},
			}
			updated, err := svc.Update(context.Background(), 42, "ch-1", &tc.update)
			if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			tc.check(t, updated)
			tc.check(t, repo.ch)
		})
	}
}
