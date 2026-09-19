// Package classify implements the retention/segment classification pipeline
// for SMS, Gmail, and call-transcript documents: a deterministic Gate that
// short-circuits obvious cases and asymmetrically bounds the Jev-based
// Classifier's output, plus the Classifier itself (see classifier.go).
//
// The Gate is intentionally asymmetric, not two mirrored checks:
//
//   - BulkSender signals ("this looks like an automated/company sender")
//     forbid promotion to retention=keep, because a bulk sender's messages
//     are never worth indefinite retention even when Jev's segment classifier
//     mistakes an automated notification for a personal one.
//   - PersonSignal signals ("this looks like an ongoing human relationship")
//     forbid demotion to retention=disposable, because deleting eligibility
//     for a real conversation is a much worse failure mode than keeping a
//     few extra low-value messages around.
//
// Neither signal can hand back the OTHER's default outcome — flooring
// disposable to low is not the same as promoting to keep, and capping keep
// to low is not the same as demoting to disposable. See classifier.go's
// decideRetention for how these two flags combine with the Jev answer.
package classify

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// Tag is a fully-determined classification, produced by a deterministic
// rule rather than a Jev call.
type Tag struct {
	Segment   string
	Retention string
}

// Gate holds the deterministic signals computed for a single document.
type Gate struct {
	// BulkSender indicates an automated/company sender. See package doc.
	BulkSender bool
	// PersonSignal indicates an ongoing human relationship. See package doc.
	PersonSignal bool
	// Decided is non-nil when a rule already fully determines the tag
	// without any Jev call — see classifier.go's Classify, which returns
	// immediately when this is set.
	Decided *Tag
}

// Deterministic rule patterns. See the scratchpad reference scripts
// (sms_backfill.py AUTH_PATTERN/AD_PATTERN) this gate's regex rules are
// carried over from — the wording was validated against a live SMS sample
// before this worker existed.
var (
	// authPattern matches one-time codes/OTP text. Applied to every source
	// type: a corporate notification email or an ARS call reading out a
	// verification code are equally disposable regardless of source.
	authPattern = regexp.MustCompile(`(?i)(인증번호|인증 번호|verification code|OTP|일회용|본인확인)`)

	// smsAdPattern matches advertising/marketing disclosure text mandated by
	// Korean telecom regulation. Scoped to SMS: it decides the SMS-specific
	// "ad" segment directly (see classifier.go's segment criteria), which
	// has no exact equivalent segment name for Gmail (there it is
	// "newsletter", left to the Jev score/segment call) or calls.
	smsAdPattern = regexp.MustCompile(`(\(광고\)|\[광고\]|무료수신거부|무료 수신거부|수신거부|무료거부)`)

	// smsWebSenderPattern flags the "[Web발신]" marker Korean carriers add to
	// messages sent from a web/API gateway rather than a handset. It is a
	// BulkSender signal only — unlike smsAdPattern it does not by itself
	// decide the segment (a web-sent message can still be a legitimate
	// business notification, not necessarily "ad").
	smsWebSenderPattern = regexp.MustCompile(`\[Web발신\]`)

	// smsBulkNumberPattern matches sender numbers typical of automated
	// senders: 15xx-18xx short-code-style numbers (e.g. "1588-1234"),
	// 080/0800 toll-free numbers, and bare 4-digit shortcodes.
	smsBulkNumberPattern = regexp.MustCompile(`^(1[5-8]\d{2}-?\d{4}|0800?-?\d+|\d{4})$`)

	// mailBulkLocalPartPattern matches the local part (before '@') of
	// addresses that never expect a human reply.
	mailBulkLocalPartPattern = regexp.MustCompile(`(?i)^(noreply|no-reply|notifications?|mailer)`)
)

// mailBulkLabelIDs are Gmail category labels applied only to automated bulk
// mail (promotions/updates/forums/social), never to 1:1 human conversation.
var mailBulkLabelIDs = map[string]bool{
	"CATEGORY_PROMOTIONS": true,
	"CATEGORY_UPDATES":    true,
	"CATEGORY_FORUMS":     true,
	"CATEGORY_SOCIAL":     true,
}

// mailPersonalLabelID is the Gmail category label Google itself applies to
// 1:1 personal correspondence.
const mailPersonalLabelID = "CATEGORY_PERSONAL"

// shortCallMinLength is the transcript-length threshold below which a call
// recording is treated as content-free (too short to classify meaningfully)
// rather than run through Jev.
const shortCallMinLength = 120

// SenderLookup answers whether the user has ever sent an outbound SMS to a
// given sender — the strongest available signal that a 010-prefixed number
// belongs to a real ongoing relationship rather than a one-off contact.
// *store.DocumentStore satisfies this via HasOutboundTo.
type SenderLookup interface {
	HasOutboundTo(ctx context.Context, sender string) (bool, error)
}

// Evaluator computes Gate values for documents. It caches SenderLookup
// results for its own lifetime — callers MUST construct a new Evaluator per
// worker tick (see internal/worker/classification_worker.go) so the cache
// never serves data staler than "this tick's batch".
type Evaluator struct {
	lookup SenderLookup
	cache  map[string]bool
}

// NewEvaluator constructs an Evaluator. lookup may be nil, in which case SMS
// PersonSignal falls back to the contact_name-only check (no outbound-history
// query is made) — useful for tests and for any deployment that has not
// wired a document store dependency into the gate.
func NewEvaluator(lookup SenderLookup) *Evaluator {
	return &Evaluator{lookup: lookup, cache: make(map[string]bool)}
}

// Evaluate computes the Gate for doc. Only SourceSMS, SourceGmail, and
// SourceCallTranscript documents produce a non-empty Gate; any other source
// type returns the zero Gate (no signal, no decision).
func (e *Evaluator) Evaluate(ctx context.Context, doc *model.Document) (Gate, error) {
	if authPattern.MatchString(doc.Content) {
		return Gate{Decided: &Tag{Segment: "auth_transient", Retention: model.RetentionDisposable}}, nil
	}

	switch doc.SourceType {
	case model.SourceSMS:
		return e.evaluateSMS(ctx, doc)
	case model.SourceGmail:
		return evaluateMail(doc), nil
	case model.SourceCallTranscript:
		return evaluateCall(doc), nil
	default:
		return Gate{}, nil
	}
}

func (e *Evaluator) evaluateSMS(ctx context.Context, doc *model.Document) (Gate, error) {
	if smsAdPattern.MatchString(doc.Content) {
		return Gate{
			BulkSender: true,
			Decided:    &Tag{Segment: "ad", Retention: model.RetentionDisposable},
		}, nil
	}

	sender := metaString(doc.Metadata, "sender", "number")
	g := Gate{
		BulkSender: smsBulkNumberPattern.MatchString(normalizeNumber(sender)) ||
			smsWebSenderPattern.MatchString(doc.Content),
	}

	if !isPersonalMobileNumber(sender) {
		return g, nil
	}

	if metaString(doc.Metadata, "contact_name") != "" {
		g.PersonSignal = true
		return g, nil
	}

	if e.lookup == nil {
		return g, nil
	}

	has, cached := e.cache[sender]
	if !cached {
		var err error
		has, err = e.lookup.HasOutboundTo(ctx, sender)
		if err != nil {
			return Gate{}, fmt.Errorf("classify: sender lookup for %q: %w", sender, err)
		}
		e.cache[sender] = has
	}
	g.PersonSignal = has
	return g, nil
}

func evaluateMail(doc *model.Document) Gate {
	from := metaString(doc.Metadata, "from", "sender")
	labelIDs := metaStringSlice(doc.Metadata, "label_ids")

	return Gate{
		BulkSender:   mailBulkLocalPartPattern.MatchString(localPart(from)) || anyLabelIn(labelIDs, mailBulkLabelIDs),
		PersonSignal: containsString(labelIDs, mailPersonalLabelID),
	}
}

func evaluateCall(doc *model.Document) Gate {
	if len([]rune(doc.Content)) < shortCallMinLength {
		return Gate{Decided: &Tag{Segment: "short_call", Retention: model.RetentionLow}}
	}
	return Gate{}
}

// isPersonalMobileNumber reports whether sender looks like a Korean mobile
// number (010 prefix), the population smsBulkNumberPattern's shortcode/toll
// patterns never overlap with.
func isPersonalMobileNumber(sender string) bool {
	return strings.HasPrefix(normalizeNumber(sender), "010")
}

// normalizeNumber strips separators so patterns don't need to account for
// "1588-1234" vs "15881234" formatting variance.
func normalizeNumber(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.TrimPrefix(s, "+82")
}

// localPart returns the portion of an email address before '@', or the
// whole string when no '@' is present (defensive — metadata is
// collector-provided, not schema-validated).
func localPart(addr string) string {
	if i := strings.IndexByte(addr, '@'); i >= 0 {
		return addr[:i]
	}
	return addr
}

// metaString returns the first non-empty string value found in doc metadata
// under any of keys, or "" if none is present or none is a string.
func metaString(meta map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := meta[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// metaStringSlice reads a []string-shaped metadata value, tolerating the
// []any shape produced by JSON-decoded metadata.
func metaStringSlice(meta map[string]any, key string) []string {
	v, ok := meta[key]
	if !ok {
		return nil
	}
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func anyLabelIn(labels []string, set map[string]bool) bool {
	for _, l := range labels {
		if set[l] {
			return true
		}
	}
	return false
}
