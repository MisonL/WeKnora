package telegram

import (
	"errors"
	"net/url"
	"regexp"

	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// telegramAPIBase is the Bot API root. Every request URL embeds the bot token
// in its path, so the construction points live here and every transport error
// is passed through redactTelegramError before it is wrapped or logged.
const telegramAPIBase = "https://api.telegram.org"

// botAPIURL builds https://api.telegram.org/bot<token>/<method>.
func botAPIURL(botToken, method string) string {
	return telegramAPIBase + "/bot" + botToken + "/" + method
}

// botFileURL builds https://api.telegram.org/file/bot<token>/<filePath>.
func botFileURL(botToken, filePath string) string {
	return telegramAPIBase + "/file/bot" + botToken + "/" + filePath
}

// botTokenPathRegex matches the /bot<token> path segment in a Bot API URL.
var botTokenPathRegex = regexp.MustCompile(`/bot[^/?#\s]+`)

// redactTelegramError masks the bot token in every *url.Error of err's chain.
// RedactRequestError handles credential query parameters; the Telegram token
// travels in the path instead, so it needs the extra pattern.
//
// Call it on the raw error from http.NewRequestWithContext / http.Client.Do,
// before wrapping: fmt.Errorf renders the message immediately, so mutating an
// already-wrapped error cannot rewrite text that was already formatted.
func redactTelegramError(err error) error {
	if err == nil {
		return nil
	}
	// RedactRequestError mutates the same *url.Error.URL values in place, and
	// the loop below then masks the path token on those same values.
	err = secutils.RedactRequestError(err)
	for current := err; current != nil; current = errors.Unwrap(current) {
		urlErr, ok := current.(*url.Error)
		if !ok || urlErr == nil {
			continue
		}
		urlErr.URL = botTokenPathRegex.ReplaceAllString(urlErr.URL, "/bot***")
	}
	return err
}
