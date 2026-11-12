package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// A title-only session update must keep the stored description; an explicit
// empty string still clears it, and the skill-maintenance marker keeps its
// reserved protection regardless of what the client sends.
func TestUpdateSessionDescriptionPresence(t *testing.T) {
	cases := []struct {
		name        string
		title       string
		description string
		provided    bool
		wantTitle   string
		wantDesc    string
	}{
		{
			name:      "title only keeps stored description",
			title:     "renamed",
			wantTitle: "renamed",
			wantDesc:  "original description",
		},
		{
			name:        "explicit empty description clears it",
			title:       "renamed",
			description: "",
			provided:    true,
			wantTitle:   "renamed",
			wantDesc:    "",
		},
		{
			name:        "explicit description replaces it",
			title:       "renamed",
			description: "new description",
			provided:    true,
			wantTitle:   "renamed",
			wantDesc:    "new description",
		},
		{
			name:      "maintenance marker stays on omitted description",
			title:     "renamed",
			wantTitle: "renamed",
			wantDesc:  types.SkillMaintenanceSessionMarker + "install",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := newTestSessionService(t)
			row := &types.Session{
				TenantID:    1,
				UserID:      "alice",
				Title:       "original",
				Description: tc.wantDesc,
			}
			if tc.name == "maintenance marker stays on omitted description" {
				row.Description = types.SkillMaintenanceSessionMarker + "install"
			}
			require.NoError(t, db.Create(row).Error)

			err := svc.UpdateSession(testSessionScopeContext(1, "alice"), &types.Session{
				ID:                  row.ID,
				TenantID:            1,
				Title:               tc.title,
				Description:         tc.description,
				DescriptionProvided: tc.provided,
			})
			require.NoError(t, err)

			var got types.Session
			require.NoError(t, db.First(&got, "id = ?", row.ID).Error)
			require.Equal(t, tc.wantTitle, got.Title)
			require.Equal(t, tc.wantDesc, got.Description)
		})
	}
}
