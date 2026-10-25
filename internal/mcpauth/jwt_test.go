package mcpauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var jwtNow = time.Unix(1_800_000_000, 0)

func testSigner() *tokenSigner {
	return &tokenSigner{key: []byte(testSigningKey), issuer: testBaseURL, resource: testBaseURL + "/mcp"}
}

func goodClaims() Claims {
	return Claims{
		Issuer: testBaseURL, Audience: testBaseURL + "/mcp", Subject: SubjectOwner,
		Scope: ScopeRead, ClientID: "dots", ID: "jti-1",
		IssuedAt: jwtNow.Unix(), NotBefore: jwtNow.Unix(), ExpiresAt: jwtNow.Add(time.Hour).Unix(),
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// forge 는 임의 헤더·본문으로 key 로 서명한 토큰을 만든다. 공격자가 "서명 키를
// 알고 있다면 무엇이 통과하는가"가 아니라 "헤더/클레임이 이상할 때 서명이 맞아도
// 거절되는가"를 확인하는 용도다.
func forge(header, claims any, key []byte) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(in))
	return in + "." + b64(m.Sum(nil))
}

var goodHeader = map[string]string{"alg": "HS256", "typ": "at+jwt"}

func TestJWT_RoundTrip(t *testing.T) {
	t.Parallel()
	s := testSigner()
	tok, err := s.sign(goodClaims())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.verify(tok, jwtNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Subject != SubjectOwner || got.Scope != ScopeRead || got.ID != "jti-1" || got.ClientID != "dots" {
		t.Fatalf("claims = %+v", got)
	}
}

func TestJWT_Rejections(t *testing.T) {
	t.Parallel()
	s := testSigner()
	key := []byte(testSigningKey)
	base := goodClaims()
	mut := func(f func(*Claims)) Claims { c := base; f(&c); return c }

	type tc struct {
		name  string
		token string
		now   time.Time
	}
	in := jwtNow.Add(time.Minute)
	cases := []tc{
		{"wrong issuer", forge(goodHeader, mut(func(c *Claims) { c.Issuer = "https://other.example.com" }), key), in},
		{"audience confusion: other resource", forge(goodHeader, mut(func(c *Claims) { c.Audience = "https://other.example.com/mcp" }), key), in},
		{"audience confusion: issuer as aud", forge(goodHeader, mut(func(c *Claims) { c.Audience = testBaseURL }), key), in},
		{"audience array with extra", forge(goodHeader, map[string]any{
			"iss": testBaseURL, "aud": []string{testBaseURL + "/mcp", "https://other.example.com"}, "sub": "owner",
			"scope": ScopeRead, "jti": "x", "iat": base.IssuedAt, "nbf": base.NotBefore, "exp": base.ExpiresAt}, key), in},
		{"audience missing", forge(goodHeader, map[string]any{
			"iss": testBaseURL, "sub": "owner", "scope": ScopeRead, "jti": "x",
			"iat": base.IssuedAt, "nbf": base.NotBefore, "exp": base.ExpiresAt}, key), in},
		{"audience number", forge(goodHeader, map[string]any{
			"iss": testBaseURL, "aud": 5, "sub": "owner", "scope": ScopeRead, "jti": "x",
			"iat": base.IssuedAt, "nbf": base.NotBefore, "exp": base.ExpiresAt}, key), in},
		{"wrong subject", forge(goodHeader, mut(func(c *Claims) { c.Subject = "someone" }), key), in},
		{"missing jti", forge(goodHeader, mut(func(c *Claims) { c.ID = "" }), key), in},
		{"missing exp", forge(goodHeader, mut(func(c *Claims) { c.ExpiresAt = 0 }), key), in},
		{"expired", forge(goodHeader, base, key), jwtNow.Add(time.Hour)},
		{"expired long ago", forge(goodHeader, base, key), jwtNow.Add(48 * time.Hour)},
		{"not yet valid", forge(goodHeader, mut(func(c *Claims) { c.NotBefore = jwtNow.Add(time.Hour).Unix() }), key), in},
		{"iat in future", forge(goodHeader, mut(func(c *Claims) { c.IssuedAt = jwtNow.Add(time.Hour).Unix() }), key), in},
		{"alg none", forge(map[string]string{"alg": "none", "typ": "at+jwt"}, base, key), in},
		{"alg RS256 label with HS256 mac", forge(map[string]string{"alg": "RS256", "typ": "at+jwt"}, base, key), in},
		{"alg lowercase", forge(map[string]string{"alg": "hs256", "typ": "at+jwt"}, base, key), in},
		{"typ missing", forge(map[string]string{"alg": "HS256"}, base, key), in},
		{"typ JWT (other token kind)", forge(map[string]string{"alg": "HS256", "typ": "JWT"}, base, key), in},
		{"crit header", forge(map[string]any{"alg": "HS256", "typ": "at+jwt", "crit": []string{"x"}}, base, key), in},
		{"signed with another key", forge(goodHeader, base, []byte(strings.Repeat("k", 40))), in},
	}
	for _, c := range cases {
		if _, err := s.verify(c.token, c.now); err == nil {
			t.Errorf("%s: token accepted", c.name)
		}
	}

	// 대조군: 같은 forge 로 만든 정상 토큰은 통과한다(위 거절이 forge 자체의 결함이
	// 아님을 보인다).
	if _, err := s.verify(forge(goodHeader, base, key), in); err != nil {
		t.Fatalf("control token rejected: %v", err)
	}
}

func TestJWT_Malformed(t *testing.T) {
	t.Parallel()
	s := testSigner()
	good, _ := s.sign(goodClaims())
	parts := strings.Split(good, ".")
	now := jwtNow.Add(time.Minute)

	bad := []string{
		"",
		"abc",
		"a.b",
		"a.b.c.d",
		good + ".",
		parts[0] + "." + parts[1] + ".", // 빈 서명
		parts[0] + "." + parts[1] + "." + parts[2][:10],          // 잘린 서명
		parts[0] + "." + b64([]byte(`{"x":1}`)) + "." + parts[2], // 본문 바꿔치기
		parts[0] + "=." + parts[1] + "." + parts[2],              // 패딩이 섞인 base64
		strings.Repeat("a", jwtMaxBytes+1),
	}
	for i, tok := range bad {
		if _, err := s.verify(tok, now); err == nil {
			t.Errorf("malformed #%d accepted: %q", i, tok)
		}
	}

	// 서명 한 글자 변조
	flipped := []byte(good)
	last := len(flipped) - 2
	if flipped[last] == 'A' {
		flipped[last] = 'B'
	} else {
		flipped[last] = 'A'
	}
	if _, err := s.verify(string(flipped), now); err == nil {
		t.Error("tampered signature accepted")
	}
}

// TestJWT_ClaimsPayloadBoundToSignature 는 본문을 바꾸면(예: 스코프 상승) 같은
// 서명으로는 통과하지 못함을 확인한다.
func TestJWT_ClaimsPayloadBoundToSignature(t *testing.T) {
	t.Parallel()
	s := testSigner()
	c := goodClaims()
	c.Scope = ""
	tok, _ := s.sign(c)
	parts := strings.Split(tok, ".")
	c.Scope = ScopeRead + " " + ScopeWrite
	forged, _ := json.Marshal(c)
	if _, err := s.verify(parts[0]+"."+b64(forged)+"."+parts[2], jwtNow.Add(time.Minute)); err == nil {
		t.Fatal("payload swap accepted")
	}
}

func TestJWT_ExpiryBoundary(t *testing.T) {
	t.Parallel()
	s := testSigner()
	tok, _ := s.sign(goodClaims())
	exp := time.Unix(goodClaims().ExpiresAt, 0)
	if _, err := s.verify(tok, exp.Add(-time.Second)); err != nil {
		t.Errorf("1s before exp rejected: %v", err)
	}
	if _, err := s.verify(tok, exp); err == nil {
		t.Error("token accepted at exactly exp")
	}
}
