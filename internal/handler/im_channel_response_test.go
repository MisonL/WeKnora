package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/im"
)

// IM channel mutations used to echo the stored credentials document (plaintext
// bot tokens, app secrets, ...). The HTTP shape must keep the routing metadata
// the console needs and replace the credentials with a configured flag.
func TestIMChannelResponseOmitsCredentials(t *testing.T) {
	channel := &im.IMChannel{
		ID:              "channel-1",
		TenantID:        7,
		AgentID:         "agent-1",
		Platform:        "wecom",
		Name:            "support",
		Mode:            "websocket",
		OutputMode:      "stream",
		KnowledgeBaseID: "kb-1",
		Credentials:     im.NewCredentialsJSON([]byte(`{"bot_id":"bot-1","bot_secret":"placeholder-credential"}`)),
		Enabled:         true,
	}

	resp := imChannelResponse(channel)
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(body), "placeholder-credential") {
		t.Fatalf("response leaked the credential: %s", body)
	}
	if strings.Contains(string(body), `"credentials"`) {
		t.Fatalf("response still carries a credentials document: %s", body)
	}
	if !strings.Contains(string(body), `"credentials_configured":true`) {
		t.Fatalf("response must report credentials_configured: %s", body)
	}
	if resp.ID != "channel-1" || resp.AgentID != "agent-1" || resp.Platform != "wecom" ||
		resp.Name != "support" || resp.KnowledgeBaseID != "kb-1" {
		t.Fatalf("routing metadata was lost: %+v", resp)
	}
}

func TestIMChannelResponseReportsMissingCredentials(t *testing.T) {
	channel := &im.IMChannel{ID: "channel-2", Platform: "telegram", Credentials: im.NewCredentialsJSON([]byte(`{}`))}
	resp := imChannelResponse(channel)
	if resp.CredentialsConfigured {
		t.Fatalf("empty credentials must report configured=false: %+v", resp)
	}
}
