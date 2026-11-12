package im

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// IMChannel represents an IM channel configuration stored in the database.
// Each channel binds to an agent and contains platform-specific credentials.
type IMChannel struct {
	ID              string          `json:"id"          gorm:"type:varchar(36);primaryKey;default:uuid_generate_v4()"`
	TenantID        uint64          `json:"tenant_id"   gorm:"not null;index:idx_im_channels_tenant"`
	AgentID         string          `json:"agent_id"    gorm:"type:varchar(36);not null;index:idx_im_channels_agent"`
	Platform        string          `json:"platform"    gorm:"type:varchar(20);not null"`
	Name            string          `json:"name"        gorm:"type:varchar(255);not null;default:''"`
	Enabled         bool            `json:"enabled"     gorm:"not null;default:true"`
	Mode            string          `json:"mode"        gorm:"type:varchar(20);not null;default:'websocket'"`
	OutputMode      string          `json:"output_mode"       gorm:"type:varchar(20);not null;default:'stream'"`
	Locale          string          `json:"locale"            gorm:"type:varchar(16);not null;default:''"`
	KnowledgeBaseID string          `json:"knowledge_base_id" gorm:"type:varchar(36);default:''"`
	BotIdentity     string          `json:"bot_identity"      gorm:"type:varchar(255);not null;default:'';uniqueIndex:idx_im_channels_bot_identity,where:deleted_at IS NULL AND bot_identity != ''"`
	SessionMode     string          `json:"session_mode"      gorm:"type:varchar(20);not null;default:'user'"`
	Credentials     CredentialsJSON `json:"credentials"       gorm:"type:jsonb;not null;default:'{}'"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	DeletedAt       gorm.DeletedAt  `json:"deleted_at"  gorm:"index"`
}

func (IMChannel) TableName() string {
	return "im_channels"
}

// IMChannelSummary is the HTTP-safe list shape for IM channels. Credentials
// are never included — use Admin+ Create/Update responses to read back
// values immediately after a mutation.
type IMChannelSummary struct {
	ID              string `json:"id"`
	TenantID        uint64 `json:"tenant_id"`
	AgentID         string `json:"agent_id"`
	Platform        string `json:"platform"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	Mode            string `json:"mode"`
	OutputMode      string `json:"output_mode"`
	Locale          string `json:"locale"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	// BotIdentity is display-sanitized (see SanitizeBotIdentityForDisplay):
	// it must never carry an authenticable secret to Viewer-facing lists.
	BotIdentity           string    `json:"bot_identity"`
	SessionMode           string    `json:"session_mode"`
	CredentialsConfigured bool      `json:"credentials_configured"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// SummarizeIMChannel converts a stored channel into its list response shape.
func SummarizeIMChannel(ch IMChannel) IMChannelSummary {
	return IMChannelSummary{
		ID:                    ch.ID,
		TenantID:              ch.TenantID,
		AgentID:               ch.AgentID,
		Platform:              ch.Platform,
		Name:                  ch.Name,
		Enabled:               ch.Enabled,
		Mode:                  ch.Mode,
		OutputMode:            ch.OutputMode,
		Locale:                ch.Locale,
		KnowledgeBaseID:       ch.KnowledgeBaseID,
		BotIdentity:           SanitizeBotIdentityForDisplay(ch.Platform, ch.BotIdentity),
		SessionMode:           ch.SessionMode,
		CredentialsConfigured: imCredentialsConfigured(ch.Credentials),
		CreatedAt:             ch.CreatedAt,
		UpdatedAt:             ch.UpdatedAt,
	}
}

// SummarizeIMChannels converts stored channels into list response shapes.
func SummarizeIMChannels(channels []IMChannel) []IMChannelSummary {
	out := make([]IMChannelSummary, 0, len(channels))
	for _, ch := range channels {
		out = append(out, SummarizeIMChannel(ch))
	}
	return out
}

// CheckCredentialReplacement reports whether a caller may replace this
// channel's stored credentials. When the stored ciphertext could not be
// decrypted, overwriting it destroys a secret the original SYSTEM_AES_KEY may
// still recover; the operator has to restore that key first (or recreate the
// channel) before re-entering credentials.
func (ch *IMChannel) CheckCredentialReplacement(replacing bool) error {
	if ch == nil || !replacing {
		return nil
	}
	if ch.Credentials.Undecryptable() {
		return ErrChannelCredentialsUndecryptable
	}
	return nil
}

func imCredentialsConfigured(cred CredentialsJSON) bool {
	if cred.Undecryptable() {
		// The stored ciphertext exists but is unreadable with the current
		// key: report it as configured so the UI does not invite a re-entry
		// that the write path would refuse anyway.
		return true
	}
	s := strings.TrimSpace(string(cred.Bytes()))
	return s != "" && s != "{}"
}

func (ch *IMChannel) BeforeCreate(tx *gorm.DB) error {
	if ch.ID == "" {
		ch.ID = uuid.New().String()
	}
	if ch.Mode == "" {
		if ch.Platform == "mattermost" || ch.Platform == "yunzhijia" {
			ch.Mode = "webhook"
		} else {
			ch.Mode = "websocket"
		}
	}
	if ch.OutputMode == "" {
		ch.OutputMode = "stream"
	}
	if ch.SessionMode == "" {
		ch.SessionMode = string(SessionModeUser)
	}
	if err := ch.validateSessionMode(); err != nil {
		return err
	}
	if err := ch.normalizeAndValidateLocale(); err != nil {
		return err
	}
	ch.BotIdentity = ch.computeBotIdentity()
	return nil
}

// BeforeSave ensures bot_identity is recomputed and session_mode is validated
// on every save (create + update).
func (ch *IMChannel) BeforeSave(tx *gorm.DB) error {
	if ch.SessionMode == "" {
		ch.SessionMode = string(SessionModeUser)
	}
	if err := ch.validateSessionMode(); err != nil {
		return err
	}
	if err := ch.normalizeAndValidateLocale(); err != nil {
		return err
	}
	ch.BotIdentity = ch.computeBotIdentity()
	return nil
}

func (ch *IMChannel) normalizeAndValidateLocale() error {
	locale := strings.TrimSpace(ch.Locale)
	if locale == "" {
		ch.Locale = ""
		return nil
	}
	if normalized := types.NormalizeSupportedLocale(locale); normalized != "" {
		ch.Locale = normalized
		return nil
	}
	return fmt.Errorf("invalid locale: %s", locale)
}

// validateSessionMode checks that SessionMode holds a supported value.
func (ch *IMChannel) validateSessionMode() error {
	switch SessionMode(ch.SessionMode) {
	case SessionModeUser, SessionModeThread:
		return nil
	default:
		return fmt.Errorf("invalid session_mode: %s", ch.SessionMode)
	}
}

// computeBotIdentity derives a unique bot identity string from the channel's
// platform, mode, and credentials. Returns "" if no identity can be extracted.
func (ch *IMChannel) computeBotIdentity() string {
	creds := make(map[string]interface{})
	if err := json.Unmarshal(ch.Credentials.Bytes(), &creds); err != nil {
		return ""
	}

	str := func(key string) string {
		if v, ok := creds[key]; ok {
			switch val := v.(type) {
			case string:
				return val
			case float64:
				return fmt.Sprintf("%.0f", val)
			}
		}
		return ""
	}

	switch ch.Platform {
	case "wecom":
		switch ch.Mode {
		case "websocket":
			if botID := str("bot_id"); botID != "" {
				return "wecom:ws:" + botID
			}
		case "webhook":
			corpID := str("corp_id")
			agentID := str("corp_agent_id")
			if corpID != "" && agentID != "" {
				return "wecom:wh:" + corpID + ":" + agentID
			}
		}
	// Feishu and Lark app_ids live in separate clouds and never collide, so the
	// platform prefix keeps the same app_id on both from looking like one bot.
	case "feishu", "lark":
		if appID := str("app_id"); appID != "" {
			return ch.Platform + ":" + appID
		}
	case "telegram":
		if botToken := str("bot_token"); botToken != "" {
			// Use the bot ID part (before the colon) as identity.
			if idx := strings.Index(botToken, ":"); idx > 0 {
				return "telegram:" + botToken[:idx]
			}
			return "telegram:" + botToken
		}
	case "dingtalk":
		if clientID := str("client_id"); clientID != "" {
			return "dingtalk:" + clientID
		}
	case "mattermost":
		if tok := str("outgoing_token"); tok != "" {
			// The outgoing token authenticates Mattermost callbacks, so it
			// must never be stored verbatim in bot_identity (which list APIs
			// expose). Hash it like yunzhijia: stable and unique per token,
			// without the secret. SanitizeBotIdentityForDisplay still masks
			// legacy rows that were saved before this change.
			return fmt.Sprintf("mattermost:wh:%x", sha256.Sum256([]byte(tok)))
		}
	case "wechat":
		if botID := str("ilink_bot_id"); botID != "" {
			return "wechat:" + botID
		}
	case "qqbot":
		if appID := str("app_id"); appID != "" {
			return "qqbot:" + appID
		}
	case "yunzhijia":
		if sendMsgURL := str("send_msg_url"); sendMsgURL != "" {
			parsed, err := url.Parse(sendMsgURL)
			if err != nil {
				return ""
			}
			if token := strings.TrimSpace(parsed.Query().Get("yzjtoken")); token != "" {
				return fmt.Sprintf("yunzhijia:%x", sha256.Sum256([]byte(token)))
			}
		}
	}
	return ""
}

// SanitizeBotIdentityForDisplay returns a display-safe version of a stored
// bot_identity for Viewer-facing list responses. Legacy mattermost rows embed
// the raw outgoing_token (usable to authenticate callbacks) and telegram rows
// can embed the raw bot_token when the token has no bot-ID prefix; neither
// may leave the server. Readable platform/mode prefixes are preserved.
func SanitizeBotIdentityForDisplay(platform, identity string) string {
	if identity == "" {
		return ""
	}
	switch platform {
	case "mattermost":
		const prefix = "mattermost:wh:"
		if rest, ok := strings.CutPrefix(identity, prefix); ok && !isIMSHA256Hex(rest) {
			// New rows store a 64-char hex digest (computeBotIdentity), which
			// carries no secret; older rows store the raw outgoing token.
			return prefix + maskIMSecret(rest)
		}
	case "telegram":
		const prefix = "telegram:"
		if rest, ok := strings.CutPrefix(identity, prefix); ok && !isIMNumeric(rest) {
			// Numeric identities are the public bot ID; anything else is a
			// raw token fragment and must be masked.
			return prefix + maskIMSecret(rest)
		}
	}
	return identity
}

// isIMSHA256Hex reports whether s looks like a stored sha256 hex digest.
func isIMSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// maskIMSecret keeps only the last four runes of a secret-looking segment.
func maskIMSecret(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return "****" + string(r[len(r)-4:])
}

func isIMNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ChannelSession maps an IM channel (user+chat combination) to a WeKnora session.
// This allows the IM integration to maintain conversation continuity.
type ChannelSession struct {
	ID          string         `json:"id"            gorm:"type:varchar(36);primaryKey;default:uuid_generate_v4()"`
	Platform    string         `json:"platform"      gorm:"type:varchar(20);not null"`
	UserID      string         `json:"user_id"       gorm:"type:varchar(128);not null"`
	ChatID      string         `json:"chat_id"       gorm:"type:varchar(128);not null;default:''"`
	ThreadID    string         `json:"thread_id"     gorm:"type:varchar(128);not null;default:''"`
	SessionID   string         `json:"session_id"    gorm:"type:varchar(36);not null;index"`
	TenantID    uint64         `json:"tenant_id"     gorm:"not null;index"`
	AgentID     string         `json:"agent_id"      gorm:"type:varchar(36);default:''"`
	IMChannelID string         `json:"im_channel_id" gorm:"type:varchar(36);default:''"`
	Status      string         `json:"status"        gorm:"type:varchar(20);not null;default:'active'"`
	Metadata    types.JSON     `json:"metadata"      gorm:"type:jsonb;default:'{}'"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"deleted_at"    gorm:"index"`
}

func (ChannelSession) TableName() string {
	return "im_channel_sessions"
}

func (cs *ChannelSession) BeforeCreate(tx *gorm.DB) error {
	if cs.ID == "" {
		cs.ID = uuid.New().String()
	}
	if cs.Status == "" {
		cs.Status = "active"
	}
	return nil
}
