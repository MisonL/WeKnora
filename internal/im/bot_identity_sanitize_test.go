package im

import (
	"strings"
	"testing"
)

// sha256HexForTest is a 64-char hex digest shaped like computeBotIdentity output.
const sha256HexForTest = "8f4b1c2d8f4b1c2d8f4b1c2d8f4b1c2d8f4b1c2d8f4b1c2d8f4b1c2d8f4b1c2d"

// SanitizeBotIdentityForDisplay must strip anything usable for authentication
// from bot_identity while keeping the readable platform/mode prefix.
func TestSanitizeBotIdentityForDisplay(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		identity string
		want     string
	}{
		{name: "empty", platform: "mattermost", identity: "", want: ""},
		{name: "legacy mattermost raw token masked", platform: "mattermost", identity: "mattermost:wh:abcdwxyz1234", want: "mattermost:wh:****1234"},
		{name: "short mattermost token fully masked", platform: "mattermost", identity: "mattermost:wh:abc", want: "mattermost:wh:****"},
		{name: "hashed mattermost identity untouched", platform: "mattermost", identity: "mattermost:wh:" + sha256HexForTest, want: "mattermost:wh:" + sha256HexForTest},
		{name: "telegram numeric bot id kept", platform: "telegram", identity: "telegram:123456789", want: "telegram:123456789"},
		{name: "telegram raw token masked", platform: "telegram", identity: "telegram:secretpart", want: "telegram:****part"},
		{name: "wecom identity untouched", platform: "wecom", identity: "wecom:ws:bot123", want: "wecom:ws:bot123"},
		{name: "feishu identity untouched", platform: "feishu", identity: "feishu:cli123", want: "feishu:cli123"},
		{name: "yunzhijia hashed identity untouched", platform: "yunzhijia", identity: "yunzhijia:aabbcc", want: "yunzhijia:aabbcc"},
		{name: "unknown platform untouched", platform: "slack", identity: "slack:xoxb-1", want: "slack:xoxb-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeBotIdentityForDisplay(tc.platform, tc.identity); got != tc.want {
				t.Fatalf("SanitizeBotIdentityForDisplay(%q, %q) = %q, want %q", tc.platform, tc.identity, got, tc.want)
			}
		})
	}
}

// computeBotIdentity must no longer store the raw mattermost outgoing token.
func TestComputeBotIdentityMattermostHashed(t *testing.T) {
	mk := func(tok string) *IMChannel {
		return &IMChannel{
			Platform:    "mattermost",
			Mode:        "webhook",
			Credentials: NewCredentialsJSON([]byte(`{"outgoing_token":"` + tok + `"}`)),
		}
	}
	id := mk("supersecret").computeBotIdentity()
	if strings.Contains(id, "supersecret") {
		t.Fatalf("bot_identity contains the raw outgoing token: %q", id)
	}
	if !strings.HasPrefix(id, "mattermost:wh:") {
		t.Fatalf("bot_identity lost its prefix: %q", id)
	}
	// Stable and unique per token.
	if again := mk("supersecret").computeBotIdentity(); again != id {
		t.Fatalf("identity not stable: %q vs %q", id, again)
	}
	if other := mk("other").computeBotIdentity(); other == id {
		t.Fatal("different tokens must map to different identities")
	}
}
