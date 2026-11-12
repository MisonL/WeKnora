package im

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	// Keys are 32 raw bytes each, distinct so encryption round-trips fail
	// across the rotation.
	guardKeyOriginal = "0123456789abcdef0123456789abcdef"
	guardKeyRotated  = "fedcba9876543210fedcba9876543210"
)

func storedCredentialsRaw(t *testing.T, svc *Service, id string) string {
	t.Helper()
	var raw string
	require.NoError(t, svc.db.Raw("SELECT credentials FROM im_channels WHERE id = ?", id).Scan(&raw).Error)
	return raw
}

// A rotated SYSTEM_AES_KEY leaves credentials that cannot be read. Toggling or
// renaming the channel is an unrelated operation and must leave the stored
// ciphertext byte-for-byte intact, because restoring the original key is still
// possible.
func TestChannelWritesPreserveUndecryptableCredentials(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", guardKeyOriginal)
	db := newLifecycleTestDB(t)
	channel := createLifecycleChannel(t, db, "channel-guard", "agent-old")
	svc := newLifecycleTestService(db, nil, "instance-guard")
	svc.RegisterAdapterFactory("test", (&lifecycleFactoryCounters{}).factory())
	t.Cleanup(svc.Stop)

	// Give the row a bot identity so the test can prove it is not wiped.
	require.NoError(t, db.Model(&IMChannel{}).Where("id = ?", channel.ID).
		UpdateColumn("bot_identity", "test:bot-1").Error)
	original := storedCredentialsRaw(t, svc, channel.ID)
	require.NotEmpty(t, original)
	require.NotContains(t, original, `"token":"v1"`, "ciphertext must not carry plaintext")

	// Rotate the key: loading the row now yields the unreadable marker.
	t.Setenv("SYSTEM_AES_KEY", guardKeyRotated)
	var loaded IMChannel
	require.NoError(t, db.Where("id = ?", channel.ID).First(&loaded).Error)
	require.True(t, loaded.Credentials.Undecryptable())

	// Toggle only flips enabled.
	_, err := svc.ToggleChannel(channel.ID, channel.TenantID)
	require.NoError(t, err)
	require.Equal(t, original, storedCredentialsRaw(t, svc, channel.ID))
	var afterToggle IMChannel
	require.NoError(t, db.Where("id = ?", channel.ID).First(&afterToggle).Error)
	require.Equal(t, "test:bot-1", afterToggle.BotIdentity,
		"bot_identity derived from unreadable credentials must not be blanked")

	// Rename only changes a non-secret column.
	toggled := afterToggle
	toggled.Name = "renamed-channel"
	require.NoError(t, svc.UpdateChannel(&toggled))
	require.Equal(t, original, storedCredentialsRaw(t, svc, channel.ID))
}

// Explicitly replacing credentials on such a row must be refused with a
// readable error instead of silently overwriting recoverable ciphertext.
func TestChannelCredentialReplacementRefusedWhenUndecryptable(t *testing.T) {
	t.Setenv("SYSTEM_AES_KEY", guardKeyOriginal)
	db := newLifecycleTestDB(t)
	channel := createLifecycleChannel(t, db, "channel-replace", "agent-old")
	original := storedCredentialsRaw(t, newLifecycleTestService(db, nil, "x"), channel.ID)

	t.Setenv("SYSTEM_AES_KEY", guardKeyRotated)
	var loaded IMChannel
	require.NoError(t, db.Where("id = ?", channel.ID).First(&loaded).Error)
	require.True(t, loaded.Credentials.Undecryptable())

	err := loaded.CheckCredentialReplacement(true)
	require.ErrorIs(t, err, ErrChannelCredentialsUndecryptable)
	require.Contains(t, err.Error(), "SYSTEM_AES_KEY")

	// Non-credential updates (the default path) stay allowed.
	require.NoError(t, loaded.CheckCredentialReplacement(false))

	// A row with readable credentials may be replaced.
	t.Setenv("SYSTEM_AES_KEY", guardKeyOriginal)
	var readable IMChannel
	require.NoError(t, db.Where("id = ?", channel.ID).First(&readable).Error)
	require.False(t, readable.Credentials.Undecryptable())
	require.NoError(t, readable.CheckCredentialReplacement(true))

	// And the original ciphertext was never touched by any check.
	require.Equal(t, original, storedCredentialsRaw(t, newLifecycleTestService(db, nil, "y"), channel.ID))
}
