package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// attributeKeyAllowlist is the complete set of span names and attribute keys
// the feedback/tuning instrumentation may emit.
//
// This list is duplicated from feedback_attrs.go on purpose. The point is not
// to check that the file equals itself — it is that adding an attribute
// requires editing a list whose header says what belongs on it, in a test that
// fails loudly in CI. The failure mode being defended against is somebody
// adding "the document title too, just for debugging" to a span that is
// exported to a hosted service with a long retention window.
var attributeKeyAllowlist = []string{
	// span names
	"feedback.evidence.recorded",
	"tune.dataset.loaded",
	"tune.coordinate.completed",
	"tune.promotion.decision",
	"weights.action.applied",

	// feedback attributes
	"feedback.document_id",
	"feedback.thumbs",
	"feedback.split",

	// tuning attributes
	"tune.train_queries",
	"tune.holdout_queries",
	"tune.total_feedback_rows",
	"tune.sweeps",
	"tune.evals",
	"tune.base_objective",
	"tune.best_objective",
	"tune.gate_passed",
	"tune.gate_reason",
	"tune.ndcg_gain",
	"tune.fp_delta",

	// weights history attributes
	"weights.history_id",
	"weights.action",
	"weights.previous_history_id",
}

// declaredKeys reads the Span*/Attr* string constants out of
// feedback_attrs.go. Only those two prefixes are considered: the file also
// declares the closed set of *values* for weights.action (WeightsAction*),
// which are payload, not keys.
func declaredKeys(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "feedback_attrs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse feedback_attrs.go: %v", err)
	}

	var keys []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "Attr") && !strings.HasPrefix(name.Name, "Span") {
				continue
			}
			if i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("%s is not a string literal constant", name.Name)
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", name.Name, err)
			}
			keys = append(keys, v)
		}
		return true
	})
	return keys
}

func TestAttrKeys_MatchAllowlist(t *testing.T) {
	t.Parallel()
	got := declaredKeys(t)
	sort.Strings(got)
	want := append([]string(nil), attributeKeyAllowlist...)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("declared %d span/attribute constants, allowlist has %d\n declared: %v\nallowlist: %v",
			len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("constant %q is not on the allowlist (allowlist has %q at that position)", got[i], want[i])
		}
	}
}

// TestAttrKeys_CarryNoQueryText is the blunt version of the same rule: the raw
// query is the user's own speech, and even its hash is kept out of Langfuse
// because that service's retention is longer than the local logs'.
func TestAttrKeys_CarryNoQueryText(t *testing.T) {
	t.Parallel()
	for _, k := range declaredKeys(t) {
		lower := strings.ToLower(k)
		for _, banned := range []string{"query", "text", "title", "content", "body", "snippet", "user"} {
			if strings.Contains(lower, banned) && !strings.HasSuffix(lower, "_queries") {
				t.Errorf("attribute key %q contains %q; span attributes carry document ids and aggregate numbers only", k, banned)
			}
		}
	}
}
