package telegram

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// The Telegram Bot API puts the bot token in the URL path, so any transport
// error (*url.Error) mirrors it into its message. Errors returned to callers
// are logged, so every one of them must carry "/bot***/" instead of the token.
func TestRedactTelegramErrorMasksBotToken(t *testing.T) {
	const token = "123456789:AAH-secret-bot-token"

	err := fmt.Errorf("do request: %w", redactTelegramError(&url.Error{
		Op:  "Post",
		URL: botAPIURL(token, "sendMessage"),
		Err: errors.New("dial tcp: i/o timeout"),
	}))

	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the bot token: %v", err)
	}
	if !strings.Contains(err.Error(), "/bot***/sendMessage") {
		t.Fatalf("error lost the redacted endpoint: %v", err)
	}
	if !strings.Contains(err.Error(), "dial tcp") {
		t.Fatalf("error lost the transport detail: %v", err)
	}
}

func TestRedactTelegramErrorMasksFileDownloadToken(t *testing.T) {
	const token = "987654321:AAF-another-secret"
	err := redactTelegramError(&url.Error{
		Op:  "Get",
		URL: botFileURL(token, "documents/file_1.pdf"),
		Err: errors.New("connection reset"),
	})
	if strings.Contains(err.Error(), token) {
		t.Fatalf("download error leaked the bot token: %v", err)
	}
	if !strings.Contains(err.Error(), "/file/bot***/documents/file_1.pdf") {
		t.Fatalf("download error lost the redacted path: %v", err)
	}
}

// A redirect chain can wrap another *url.Error; every link must be scrubbed.
func TestRedactTelegramErrorMasksNestedURLError(t *testing.T) {
	const token = "555:secret-in-nested"
	inner := &url.Error{Op: "Get", URL: botAPIURL(token, "getFile"), Err: errors.New("redirect failed")}
	outer := &url.Error{Op: "Post", URL: botAPIURL(token, "getUpdates"), Err: inner}

	err := redactTelegramError(outer)
	if strings.Contains(err.Error(), token) {
		t.Fatalf("nested error leaked the bot token: %v", err)
	}
}

func TestRedactTelegramErrorLeavesOtherErrors(t *testing.T) {
	plain := errors.New("plain failure")
	if got := redactTelegramError(plain); got != plain {
		t.Fatalf("redactTelegramError(plain) = %v, want the same error", got)
	}
	if redactTelegramError(nil) != nil {
		t.Fatal("redactTelegramError(nil) must stay nil")
	}
}
