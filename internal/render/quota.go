package render

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/ChristopherDavenport/openresponses"
)

// Explain says a run's error in words when it is a request the key's or
// account's credit or spend limit cannot cover (openresponses'
// insufficient_quota, OpenRouter's 402 among them): what happened, that
// it was not retried, the provider's own message and remedy, and what
// may fit, naming /compact only where the front offers it (compact).
// The provider's raw JSON is left out; the whole error stays in the
// session's record. Any other error is not explained (ok false).
func Explain(err error, compact bool) (string, bool) {
	if err == nil || !openresponses.IsInsufficientQuota(err) {
		return "", false
	}
	var msg, remedy, source string
	var oe *openresponses.Error
	if errors.As(err, &oe) {
		msg = oe.Message
		remedy, source = quotaMetadata(oe.Extra["metadata"])
		// A body that was not the spec's envelope (OpenRouter's code is a
		// number) is the message as it came: read its own.
		var body struct {
			Error struct {
				Message  string `json:"message"`
				Metadata any    `json:"metadata"`
			} `json:"error"`
		}
		raw, fromMsg := oe.Body, false
		if len(raw) == 0 && strings.HasPrefix(strings.TrimSpace(msg), "{") {
			raw, fromMsg = []byte(msg), true
		}
		if len(raw) > 0 && json.Unmarshal(raw, &body) == nil {
			// The body's own message, where it says one; else the
			// adapter's stands, unless that was the body itself.
			switch {
			case body.Error.Message != "":
				msg = body.Error.Message
			case fromMsg:
				msg = ""
			}
			if r, s := quotaMetadata(body.Error.Metadata); r != "" || s != "" {
				remedy, source = r, s
			}
		}
	}
	whose := "the key's or account's credit or spend limit"
	if source == "openrouter_key_limit" {
		whose = "the OpenRouter key's spend limit"
	}
	var b strings.Builder
	b.WriteString("model call refused: " + whose + " cannot cover this request, so it was not retried.")
	if msg = strings.TrimSpace(msg); msg != "" {
		b.WriteString(" The provider says: " + strings.TrimSuffix(msg, ".") + ".")
	}
	if remedy = strings.TrimSpace(remedy); remedy != "" {
		b.WriteString(" " + remedy)
	}
	b.WriteString(" A shorter request may fit what is left")
	if compact {
		b.WriteString(": /compact folds the conversation")
	}
	b.WriteString(". The full error is in the session's record.")
	return Clean(b.String()), true
}

// quotaMetadata reads OpenRouter's metadata: its remedy_hint and its
// limit_source.
func quotaMetadata(v any) (remedy, source string) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", ""
	}
	remedy, _ = m["remedy_hint"].(string)
	source, _ = m["limit_source"].(string)
	return remedy, source
}
