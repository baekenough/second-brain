package briefing

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// maxPromptActions bounds how many actions are shown to the model. Every action
// in the prompt is input tokens paid for on each cache miss, and a briefing
// built from more than a few dozen items stops being readable anyway. When the
// open set is larger, the soonest-due actions win.
const maxPromptActions = 40

// maxSentences is the ceiling on returned sentences. It is enforced on the
// server rather than trusted to the prompt.
const maxSentences = 6

// systemPrompt states the output schema and the discard rule.
//
// The rule is stated to the model even though the server enforces it
// mechanically: telling a model the check that will be applied measurably
// raises compliance, and a discarded sentence is a wasted generation. The
// enforcement in validate.go is what actually guarantees the property.
const systemPrompt = `당신은 사용자의 열린 액션 목록을 한국어로 요약하는 비서다.

반드시 아래 JSON 객체 하나만 출력한다. 코드 펜스, 머리말, 설명 문장을 붙이지 않는다.

{"sentences":[{"text":"문장","document_ids":["문서 id"],"identity_keys":["액션 키"]}]}

규칙:
- 각 문장은 제공된 액션에서만 유래해야 한다. 제공되지 않은 사실을 쓰지 않는다.
- document_ids에는 그 문장이 근거로 삼은 액션의 document_id를 그대로 복사한다.
- 새 id를 만들지 않는다. 목록에 없는 id를 쓰면 그 문장은 서버에서 폐기된다.
- document_ids가 비어 있는 문장도 폐기된다.
- identity_keys에는 그 문장이 다루는 액션의 identity_key를 복사한다.
- 문장은 최대 6개, 각 문장은 400자 이내의 한 문장으로 쓴다.
- 우선순위: 기한이 임박한 것 > awaiting_my_reply > my_commitment > 나머지.`

// promptAction is the per-action shape sent to the model. Field names are
// snake_case so they match the vocabulary used in the system prompt.
type promptAction struct {
	IdentityKey string  `json:"identity_key"`
	Kind        string  `json:"kind"`
	DueAt       string  `json:"due_at,omitempty"`
	Confidence  float64 `json:"confidence"`
	DetectedBy  string  `json:"detected_by"`
	Summary     string  `json:"summary"`
	DocumentID  string  `json:"document_id"`
	Counterpart string  `json:"counterpart,omitempty"`
}

// selectPromptActions returns at most maxPromptActions actions, soonest due
// first with undated actions last. It copies before sorting: the caller's slice
// order is also the order the API layer reports to the user.
func selectPromptActions(actions []InputAction) []InputAction {
	sorted := make([]InputAction, len(actions))
	copy(sorted, actions)

	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].DueAt, sorted[j].DueAt
		switch {
		case a != nil && b != nil:
			return a.Before(*b)
		case a != nil:
			return true // dated sorts before undated
		case b != nil:
			return false
		default:
			return false
		}
	})

	if len(sorted) > maxPromptActions {
		sorted = sorted[:maxPromptActions]
	}
	return sorted
}

// buildUserMessage renders the action list the model summarises.
func buildUserMessage(in Input) string {
	selected := selectPromptActions(in.Actions)

	items := make([]promptAction, 0, len(selected))
	for _, a := range selected {
		item := promptAction{
			IdentityKey: a.IdentityKey,
			Kind:        a.Kind,
			Confidence:  a.Confidence,
			DetectedBy:  a.DetectedBy,
			Summary:     a.Summary,
			DocumentID:  a.DocumentID,
			Counterpart: a.CounterpartName,
		}
		if a.DueAt != nil {
			item.DueAt = a.DueAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}

	encoded, err := json.Marshal(items)
	if err != nil {
		// json.Marshal of a slice of plain structs cannot fail; degrade to an
		// empty list rather than propagating an impossible error.
		encoded = []byte("[]")
	}

	var b strings.Builder
	b.WriteString("현재 시각: ")
	b.WriteString(in.LatestDocumentAt.UTC().Format(time.RFC3339))
	b.WriteString("\n열린 액션 목록:\n")
	b.Write(encoded)
	return b.String()
}
