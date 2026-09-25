package logsafe

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 결정적 회귀 가드(#297, 계획 §2.3 — R023 Tier 1). 로그·오류를 만드는 코드를
// go/ast 로 읽어, 개인정보를 싣는 모양이 다시 들어오면 실패한다. 실행 테스트는
// 실제로 일으킬 수 있는 경로만 덮고, 이 가드는 나머지 호출 지점까지 덮는다.
//
// 보안 리뷰(우회 9종 재현) 뒤 강화한 내용:
//   - 키만이 아니라 "값"을 본다: 경로 계열 식(path, relPath, item.path, dest,
//     sidecar, filename, d.Name() …)이 로그 인자·slog Attr 생성자 인자·
//     fmt.Sprintf/Errorf 인자로 가면 실패한다. logsafe 헬퍼를 거치면 허용.
//   - 엄격 파일(whisper.go)의 로그 키는 허용 목록이다(금지 목록은 새 이름으로
//     비껴갈 수 있다).
//   - slog.String/Any/Group/…·slog.Attr{}·Logger.With 의 인자도 본다.
//   - log/slog 의 import 별칭을 해석하고, slog.Default().Warn 같은 사슬도
//     전역 로거 직접 호출로 잡는다.
//   - 키·값 짝은 slog 의 실제 해석 규칙(문자열 = 키+값, Attr = 1칸, 그 밖 =
//     1칸)으로 맞춘다. 짝을 확정할 수 없는 인자가 섞이면 모든 문자열
//     리터럴을 키 후보로 본다.
//   - source_id 개수에 slog.String("source_id", …) 도 센다.

// guardedFile 은 가드를 적용할 파일과 규칙이다.
type guardedFile struct {
	path string
	// strict: whisper.go 규칙 — 키 허용 목록, 경로·오류 값 검사, 메시지는
	// 문자열 리터럴, 전역 slog 직접 호출 금지, fmt.Sprintf/Errorf 경로 인자 금지.
	strict bool
	// sourceIDKeys: "source_id" 키 개수. 값은 반드시 logsafe.SafeSourceID(…).
	// 개수가 다르면(키 이름을 바꿔 비껴가는 경우 포함) 실패한다. 이 규칙을 쓰는
	// 파일에서는 .SourceID 가 SafeSourceID 없이 로그 값으로 가는 것도 실패한다.
	sourceIDKeys int
}

// 적용 파일 목록(패키지 디렉터리 기준 상대 경로).
var guardedFiles = []guardedFile{
	{path: "../collector/whisper.go", strict: true},
	{path: "../scheduler/scheduler.go", sourceIDKeys: 6},
	{path: "../store/document.go", sourceIDKeys: 3},
	{path: "../store/document_source_guard.go", sourceIDKeys: 1},
	{path: "../worker/entity_worker.go", sourceIDKeys: 1},
	{path: "../worker/summarizer.go", sourceIDKeys: 1},
	{path: "../worker/extraction_retry.go", sourceIDKeys: 8},
}

// strictAllowedKeys 는 엄격 파일에서 쓸 수 있는 로그 키 전부다. 새 키가
// 필요하면 여기에 넣으면서 값이 개인정보가 아님을 확인한다.
var strictAllowedKeys = setOf(
	// 파일 참조(whisperFileAttrs)
	"file_ref", "ext", "kind",
	// 크기·시각·설정값
	"size_bytes", "limit_bytes", "mtime", "parsed", "now", "model", "endpoint",
	"count", "audio_dir", "source_id_bytes", "quarantine_dir",
	// 오류 모양(logsafe.ErrAttrs 와 whisper 오류 타입의 LogAttrs)
	"reason", "step", "status", "upstream_error_type", "upstream_error_code",
)

// 경로 계열 식: 이 이름의 식별자나 필드·메서드가 로그·오류 문구로 가면 실패.
var (
	strictPathIdents = setOf("path", "relPath", "rel", "dest", "sidecar", "sidecarDest",
		"filename", "fname", "name", "base", "sourceID", "docSourceID", "title", "audioPath")
	strictPathSels = setOf("path", "relPath", "sourceID", "title", "SourceID", "Title",
		"Name", "Path", "RelativePath")
	// 오류 값: 원문(*fs.PathError 문구에 경로)을 로그로 보내면 실패. ErrAttrs 로.
	strictErrIdent = func(n string) bool { return n == "err" || n == "reason" || strings.HasSuffix(n, "Err") }

	sourceIDIdents = setOf("sourceID")
	sourceIDSels   = setOf("SourceID")
)

// 로그 메서드와 키·값 인자가 시작하는 위치.
var logMethodKVStart = map[string]int{
	"Debug": 1, "Info": 1, "Warn": 1, "Error": 1,
	"DebugContext": 2, "InfoContext": 2, "WarnContext": 2, "ErrorContext": 2,
	"Log": 3, "LogAttrs": 3, "With": 0,
}

// slog Attr 생성자.
var slogAttrCtors = setOf("String", "Any", "Int", "Int64", "Uint64", "Float64", "Bool", "Time", "Duration", "Group")

// fmt 의 문구 생성 함수.
var fmtFormatters = setOf("Sprintf", "Sprint", "Sprintln", "Errorf", "Appendf")

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// guardStats 는 가드가 실제로 무엇을 봤는지 센다(음성 결과의 양성 대조).
type guardStats struct {
	logCalls     int
	keysChecked  int
	sourceIDKeys int
}

type checker struct {
	fset     *token.FileSet
	g        guardedFile
	slogName string // log/slog 의 로컬 이름("" = import 없음)
	fmtName  string
	problems []string
	st       guardStats
	seenKeys map[token.Pos]bool
}

func (ck *checker) report(n ast.Node, format string, args ...any) {
	ck.problems = append(ck.problems, fmt.Sprintf("%s: %s", ck.fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
}

func importName(f *ast.File, path, def string) string {
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return def
	}
	return ""
}

func checkLogKeys(fset *token.FileSet, f *ast.File, g guardedFile) ([]string, guardStats) {
	ck := &checker{
		fset: fset, g: g,
		slogName: importName(f, "log/slog", "slog"),
		fmtName:  importName(f, "fmt", "fmt"),
		seenKeys: map[token.Pos]bool{},
	}
	if ck.slogName == "." && g.strict {
		ck.report(f, `dot-import of "log/slog" is not allowed (calls cannot be attributed)`)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			ck.visitCall(x)
		case *ast.CompositeLit:
			if isAnySlice(x.Type) {
				ck.checkKVList(x.Elts, 0, false)
				ck.checkValues(x.Elts, true)
			}
			if ck.isSlogAttrLit(x) {
				ck.checkAttrLit(x)
			}
		}
		return true
	})
	return ck.problems, ck.st
}

// isPkg 는 e 가 name 패키지 식별자인지 본다.
func isPkg(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && name != "" && id.Name == name
}

// rootedIn 은 e 의 수신자 사슬이 pkg 식별자에서 시작하는지 본다
// (slog.Default().Warn, slog.With(…).Info 등).
func rootedIn(e ast.Expr, pkg string) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return pkg != "" && x.Name == pkg
	case *ast.SelectorExpr:
		return rootedIn(x.X, pkg)
	case *ast.CallExpr:
		return rootedIn(x.Fun, pkg)
	case *ast.ParenExpr:
		return rootedIn(x.X, pkg)
	}
	return false
}

func (ck *checker) visitCall(call *ast.CallExpr) {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		name := fun.Sel.Name
		// slog Attr 생성자: slog.String("k", v) …
		if isPkg(fun.X, ck.slogName) && slogAttrCtors[name] {
			ck.checkAttrCall(call)
			return
		}
		// fmt 문구 생성(엄격 파일): 경로 계열 인자 금지.
		if ck.g.strict && isPkg(fun.X, ck.fmtName) && fmtFormatters[name] {
			ck.checkValues(call.Args, false)
			return
		}
		start, isLog := logMethodKVStart[name]
		if !isLog || (name != "With" && len(call.Args) == 0) {
			return
		}
		ck.st.logCalls++
		if ck.g.strict && rootedIn(fun.X, ck.slogName) {
			ck.report(call, "global slog call (%s) — use the injected logger (c.log() / logger)", name)
		}
		if ck.g.strict && name != "With" && start-1 < len(call.Args) && start >= 1 {
			if lit, ok := call.Args[start-1].(*ast.BasicLit); !ok || lit.Kind != token.STRING {
				ck.report(call, "log message must be a string literal")
			}
		}
		ck.checkKVList(call.Args, start, call.Ellipsis.IsValid())
		ck.checkValues(call.Args, true)
	case *ast.Ident:
		if fun.Name == "append" && len(call.Args) > 1 && hasStringLit(call.Args[1:]) {
			ck.checkKVList(call.Args, 1, call.Ellipsis.IsValid())
			ck.checkValues(call.Args[1:], true)
		}
		// dot-import 된 slog 의 Warn(...) 등.
		if ck.slogName == "." {
			if _, ok := logMethodKVStart[fun.Name]; ok {
				ck.report(call, "dot-imported slog call %s", fun.Name)
			}
		}
	}
}

func hasStringLit(args []ast.Expr) bool {
	for _, a := range args {
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return true
		}
	}
	return false
}

// isAttrExpr 는 a 가 slog.Attr 를 만드는 식인지 본다.
func (ck *checker) isAttrExpr(a ast.Expr) bool {
	switch x := a.(type) {
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		return ok && isPkg(sel.X, ck.slogName) && slogAttrCtors[sel.Sel.Name]
	case *ast.CompositeLit:
		return ck.isSlogAttrLit(x)
	}
	return false
}

func (ck *checker) isSlogAttrLit(x *ast.CompositeLit) bool {
	sel, ok := x.Type.(*ast.SelectorExpr)
	return ok && isPkg(sel.X, ck.slogName) && sel.Sel.Name == "Attr"
}

// checkKVList 는 args[start:] 를 slog 규칙대로 키·값으로 나눠 키를 본다.
func (ck *checker) checkKVList(args []ast.Expr, start int, spread bool) {
	ambiguous := false
	for i := start; i < len(args); {
		a := args[i]
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			var val ast.Expr
			if i+1 < len(args) {
				val = args[i+1]
			}
			ck.checkKey(lit, val)
			i += 2
			continue
		}
		if ck.isAttrExpr(a) {
			i++ // Attr 은 한 칸(생성자 자체는 visitCall/CompositeLit 에서 검사)
			continue
		}
		if !(spread && i == len(args)-1) {
			ambiguous = true
		}
		i++
	}
	if !ambiguous {
		return
	}
	// 짝을 확정할 수 없으면 모든 문자열 리터럴을 키 후보로 본다.
	for i := start; i < len(args); i++ {
		if lit, ok := args[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			var val ast.Expr
			if i+1 < len(args) {
				val = args[i+1]
			}
			ck.checkKey(lit, val)
		}
	}
}

func (ck *checker) checkKey(lit *ast.BasicLit, val ast.Expr) {
	if ck.seenKeys[lit.Pos()] {
		return
	}
	ck.seenKeys[lit.Pos()] = true
	key, err := strconv.Unquote(lit.Value)
	if err != nil {
		return
	}
	ck.st.keysChecked++
	if ck.g.strict && !strictAllowedKeys[key] {
		ck.report(lit, "log key %q is not in the allowlist (strictAllowedKeys)", key)
	}
	if key == "source_id" {
		ck.st.sourceIDKeys++
		if val == nil || !isSafeSourceIDCall(val) {
			ck.report(lit, `"source_id" value must be logsafe.SafeSourceID(...)`)
		}
	}
}

// checkAttrCall 은 slog.String("k", v) 류를 본다. 키는 문자열 리터럴이어야 하고
// (엄격 파일), 값은 checkValues 로 본다. Group 은 나머지를 키·값 목록으로 본다.
func (ck *checker) checkAttrCall(call *ast.CallExpr) {
	ck.st.logCalls++
	if len(call.Args) == 0 {
		return
	}
	var val ast.Expr
	if len(call.Args) > 1 {
		val = call.Args[1]
	}
	if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if sel := call.Fun.(*ast.SelectorExpr); sel.Sel.Name == "Group" {
			ck.checkKey(lit, nil)
			ck.checkKVList(call.Args, 1, call.Ellipsis.IsValid())
		} else {
			ck.checkKey(lit, val)
		}
	} else if ck.g.strict {
		ck.report(call, "slog attr key must be a string literal")
	}
	ck.checkValues(call.Args[1:], true)
}

func (ck *checker) checkAttrLit(x *ast.CompositeLit) {
	ck.st.logCalls++
	var keyLit *ast.BasicLit
	var val ast.Expr
	for _, e := range x.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch k, _ := kv.Key.(*ast.Ident); {
		case k != nil && k.Name == "Key":
			keyLit, _ = kv.Value.(*ast.BasicLit)
		case k != nil && k.Name == "Value":
			val = kv.Value
		}
	}
	if keyLit != nil {
		ck.checkKey(keyLit, val)
	} else if ck.g.strict {
		ck.report(x, "slog.Attr Key must be a string literal")
	}
	if val != nil {
		ck.checkValues([]ast.Expr{val}, true)
	}
}

// isAllowedWrapper 는 값 검사가 안으로 내려가지 않는 호출이다: logsafe 헬퍼,
// whisper 의 파일 속성 헬퍼, len.
func isAllowedWrapper(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return isPkg(fun.X, "logsafe")
	case *ast.Ident:
		switch fun.Name {
		case "len", "whisperFileAttrs", "whisperFileKind", "quarantineReasonAttrs":
			return true
		}
	}
	return false
}

// checkValues 는 식들에 경로 계열(엄격 파일) 또는 SourceID(source_id 파일)
// 값이 허용 헬퍼 없이 들어 있는지 본다. logSink 가 참이면 오류 값(err …)도 본다.
func (ck *checker) checkValues(exprs []ast.Expr, logSink bool) {
	idents, sels := sourceIDIdents, sourceIDSels
	if ck.g.strict {
		idents, sels = strictPathIdents, strictPathSels
	} else if ck.g.sourceIDKeys == 0 {
		return
	}
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if isAllowedWrapper(x) {
					return false
				}
			case *ast.SelectorExpr:
				if sels[x.Sel.Name] {
					ck.report(x, "value %s reaches a log/format sink without a logsafe helper", exprString(x))
					return false
				}
				// 수신자 쪽만 계속 본다(필드 이름 자체는 식별자가 아니다).
				ast.Inspect(x.X, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok {
						ck.checkIdent(id, idents, logSink)
					}
					return true
				})
				return false
			case *ast.Ident:
				ck.checkIdent(x, idents, logSink)
			}
			return true
		})
	}
}

func (ck *checker) checkIdent(id *ast.Ident, idents map[string]bool, logSink bool) {
	if idents[id.Name] {
		ck.report(id, "value %s reaches a log/format sink without a logsafe helper", id.Name)
		return
	}
	if ck.g.strict && logSink && strictErrIdent(id.Name) {
		ck.report(id, "raw error %s in a log — use logsafe.ErrAttrs", id.Name)
	}
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprString(x.Fun) + "()"
	}
	return fmt.Sprintf("%T", e)
}

func isSafeSourceIDCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "SafeSourceID" && isPkg(sel.X, "logsafe")
}

func isAnySlice(t ast.Expr) bool {
	arr, ok := t.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	switch elt := arr.Elt.(type) {
	case *ast.Ident:
		return elt.Name == "any"
	case *ast.InterfaceType:
		return elt.Methods == nil || len(elt.Methods.List) == 0
	}
	return false
}

func TestLogKeysGuard(t *testing.T) {
	for _, g := range guardedFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, g.path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v (guard target moved? update guardedFiles)", g.path, err)
		}
		problems, st := checkLogKeys(fset, f, g)
		for _, p := range problems {
			t.Error(p)
		}
		t.Logf("%s: log/attr calls=%d keys=%d source_id=%d", g.path, st.logCalls, st.keysChecked, st.sourceIDKeys)
		// 양성 대조: 가드가 실제로 로그 호출·키를 봤는지 확인한다. 0 이면
		// 파일 구조가 바뀌어 가드가 아무것도 검사하지 못하는 것이다.
		if st.logCalls == 0 || st.keysChecked == 0 {
			t.Errorf("%s: guard inspected %d log calls / %d keys — expected > 0", g.path, st.logCalls, st.keysChecked)
		}
		if st.sourceIDKeys != g.sourceIDKeys {
			t.Errorf(`%s: found %d "source_id" log keys, want %d (a renamed key would bypass SafeSourceID — update guardedFiles deliberately)`,
				g.path, st.sourceIDKeys, g.sourceIDKeys)
		}
	}
}

// TestLogKeysGuard_DetectsBypasses 는 가드 자체의 양성 대조군이다. 보안 리뷰가
// 재현한 우회 9종(#297 리뷰)과 그 밖의 위반을 넣은 소스를 검사해 각각을
// 잡아내는지, 그리고 올바른 코드는 통과시키는지 본다.
func TestLogKeysGuard_DetectsBypasses(t *testing.T) {
	strict := guardedFile{strict: true}
	sid := guardedFile{sourceIDKeys: 1}
	cases := []struct {
		name string
		g    guardedFile
		src  string
		want []string // 모두 나와야 하는 문구
	}{
		// --- 리뷰어 재현 9종 ---
		{"R1 slog.String attr", strict,
			`package x; import "log/slog"; func f(c C, path string){ c.log().Warn("m", slog.String("path", path)) }`,
			[]string{`log key "path"`, "value path reaches"}},
		{"R2 With", strict,
			`package x; func f(c C, path string){ c.log().With("path", path).Warn("m") }`,
			[]string{`log key "path"`, "value path reaches"}},
		{"R3 denylist rename", strict,
			`package x; func f(c C, path string, err error){ c.log().Warn("m", "audio", path, "cause", err) }`,
			[]string{`log key "audio"`, `log key "cause"`, "value path reaches", "raw error err"}},
		{"R4 import alias", strict,
			`package x; import lg "log/slog"; func f(){ lg.Warn("m", "k", 1) }`,
			[]string{"global slog call (Warn)", `log key "k"`}},
		{"R5 slog.Default chain", strict,
			`package x; import "log/slog"; func f(){ slog.Default().Warn("m", "k", 1) }`,
			[]string{"global slog call (Warn)"}},
		{"R6 parity shift", strict,
			`package x; import "log/slog"; func f(c C, path string){ c.log().Warn("m", slog.Int("n",1), "path", path) }`,
			[]string{`log key "path"`, `log key "n"`, "value path reaches"}},
		// 짝 확정 폴백(모든 리터럴을 키 후보로)이 끼어들지 않는 모양: 값이 리터럴이라
		// 짝 계산만으로 "path" 를 키로 잡아야 한다.
		{"R6b parity shift, literal value", strict,
			`package x; import "log/slog"; func f(c C){ c.log().Warn("m", slog.Int("size_bytes",1), "path", "x") }`,
			[]string{`log key "path"`}},
		{"R7 Sprintf message", strict,
			`package x; import "fmt"; func f(c C, path string){ c.log().Warn(fmt.Sprintf("failed %s", path)) }`,
			[]string{"log message must be a string literal", "value path reaches"}},
		{"R8 source_id attr", sid,
			`package x; import "log/slog"; func f(c C, d D){ c.log().Info("m", slog.String("source_id", d.SourceID)) }`,
			[]string{`"source_id" value must be logsafe.SafeSourceID`, "value d.SourceID reaches"}},
		{"R9 Errorf path", strict,
			`package x; import "fmt"; func f(path string) error { return fmt.Errorf("open %s", path) }`,
			[]string{"value path reaches"}},
		// --- 그 밖의 모양 ---
		{"field selector + d.Name()", strict,
			`package x; func f(c C, item I, d D){ c.log().Warn("m", "kind", item.path); c.log().Info("m", "ext", d.Name()) }`,
			[]string{"value item.path reaches", "value d.Name reaches"}},
		{"append kv outside log", strict,
			`package x; func f(fa []any, dest string){ attrs := append(fa, "size_bytes", dest); _ = attrs }`,
			[]string{"value dest reaches"}},
		{"any-slice literal", strict,
			`package x; func f(sidecar string){ _ = []any{"reason", sidecar} }`,
			[]string{"value sidecar reaches"}},
		{"slog.Attr literal", strict,
			`package x; import "log/slog"; func f(c C, path string){ c.log().LogAttrs(nil, 0, "m", slog.Attr{Key: "dest", Value: slog.StringValue(path)}) }`,
			[]string{`log key "dest"`, "value path reaches"}},
		{"Group nested", strict,
			`package x; import "log/slog"; func f(c C, path string){ c.log().Warn("m", slog.Group("g", "filename", path)) }`,
			[]string{`log key "g"`, `log key "filename"`, "value path reaches"}},
		{"source_id renamed key", sid,
			`package x; import "log/slog"; func f(d D){ slog.Warn("m", "sid", d.SourceID) }`,
			[]string{"value d.SourceID reaches"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			problems, _ := checkLogKeys(fset, f, tc.g)
			joined := strings.Join(problems, "\n")
			if len(problems) == 0 {
				t.Fatalf("guard detected nothing")
			}
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("guard missed %q; problems:\n%s", w, joined)
				}
			}
		})
	}
}

// TestLogKeysGuard_AllowsSafeForms 는 음성 대조군이다: 올바른 코드에 오탐이
// 없어야 한다(오탐이 많으면 가드를 끄고 싶어진다).
func TestLogKeysGuard_AllowsSafeForms(t *testing.T) {
	cases := []struct {
		name string
		g    guardedFile
		src  string
		sids int
	}{
		{"strict safe", guardedFile{strict: true}, `package x
import ("fmt"; "log/slog"; "github.com/baekenough/second-brain/internal/logsafe")
func f(c C, path, audioDir string, err error, item I, doc D) error {
	fileAttrs := whisperFileAttrs(audioDir, path)
	c.log().Warn("whisper: x", append(fileAttrs, logsafe.ErrAttrs(err)...)...)
	c.log().Warn("whisper: y", append(fileAttrs, "reason", "too_short", "size_bytes", 3)...)
	c.log().Info("whisper: z", "source_id_bytes", len(doc.SourceID), "quarantine_dir", "q")
	c.log().Warn("whisper: a", slog.String("step", "read"), "status", 500)
	_ = []any{"reason", "unreadable"}
	return fmt.Errorf("whisper: walk %q: %w", c.cfg.WhisperAudioDir, err)
}`, 0},
		{"source_id safe", guardedFile{sourceIDKeys: 3}, `package x
import ("log/slog"; "github.com/baekenough/second-brain/internal/logsafe")
func f(d D, err error) {
	slog.Warn("m", "source_id", logsafe.SafeSourceID(d.SourceID), "error", err)
	slog.Info("m", slog.String("source_id", logsafe.SafeSourceID(d.SourceID)))
	attrs := []any{"source_type", d.SourceType, "source_id", logsafe.SafeSourceID(d.SourceID)}
	_ = attrs
}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			problems, st := checkLogKeys(fset, f, tc.g)
			sort.Strings(problems)
			for _, p := range problems {
				t.Errorf("false positive: %s", p)
			}
			if st.sourceIDKeys != tc.sids {
				t.Errorf("sourceIDKeys = %d, want %d", st.sourceIDKeys, tc.sids)
			}
		})
	}
}
