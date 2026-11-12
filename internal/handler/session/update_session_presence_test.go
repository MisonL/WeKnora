package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stubSessionPresenceService struct {
	interfaces.SessionService
	gotSession *types.Session
}

func (s *stubSessionPresenceService) UpdateSession(_ context.Context, session *types.Session) error {
	cp := *session
	s.gotSession = &cp
	return nil
}

func (s *stubSessionPresenceService) GetSession(_ context.Context, id string) (*types.Session, error) {
	return &types.Session{ID: id, Title: s.gotSession.Title, Description: s.gotSession.Description}, nil
}

// A title-only PUT must not clear the stored description: the handler marks
// DescriptionProvided only when the body actually carried a description.
func TestUpdateSessionMarksDescriptionPresence(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantDesc    string
		wantDescSet bool
	}{
		{
			name:     "title only",
			body:     `{"title":"renamed"}`,
			wantDesc: "", wantDescSet: false,
		},
		{
			name:     "explicit empty description",
			body:     `{"title":"renamed","description":""}`,
			wantDesc: "", wantDescSet: true,
		},
		{
			name:     "explicit description",
			body:     `{"title":"renamed","description":"hello"}`,
			wantDesc: "hello", wantDescSet: true,
		},
		{
			name:     "null description counts as provided",
			body:     `{"title":"renamed","description":null}`,
			wantDesc: "", wantDescSet: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stubSessionPresenceService{}
			h := &Handler{sessionService: svc}
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(func(c *gin.Context) {
				ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(1))
				c.Request = c.Request.WithContext(ctx)
				c.Set(types.TenantIDContextKey.String(), uint64(1))
				c.Next()
			})
			r.PUT("/sessions/:id", h.UpdateSession)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/sessions/s-1", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.NotNil(t, svc.gotSession)
			require.Equal(t, tc.wantDesc, svc.gotSession.Description)
			require.Equal(t, tc.wantDescSet, svc.gotSession.DescriptionProvided)
			require.Equal(t, "renamed", svc.gotSession.Title)
		})
	}
}
