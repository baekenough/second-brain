package mcpauth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const (
	testOwnerKey   = "owner-key-for-tests-0123456789abcdef"
	testSigningKey = "signing-key-for-tests-0123456789abcdef-xyz"
	testBaseURL    = "https://brain-mcp.example.com"
	testRedirect   = "https://chatgpt.com/connector_platform_oauth_redirect"
)

func ownerHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		EnvPublicBaseURL:  testBaseURL,
		EnvRedirectURIs:   testRedirect,
		EnvOwnerKeySHA256: ownerHash(testOwnerKey),
		EnvSigningKey:     testSigningKey,
	}
}

func TestConfigFromEnv_DisabledWhenUnset(t *testing.T) {
	t.Parallel()
	cfg, err := ConfigFromEnv(envOf(map[string]string{}))
	if err != nil || cfg != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", cfg, err)
	}
	// 공백만 있는 값도 "설정 안 함"이다(.env 의 `KEY= ` 같은 줄).
	cfg, err = ConfigFromEnv(envOf(map[string]string{EnvPublicBaseURL: "  ", EnvSigningKey: ""}))
	if err != nil || cfg != nil {
		t.Fatalf("blank values: got (%v, %v), want (nil, nil)", cfg, err)
	}
}

func TestConfigFromEnv_Defaults(t *testing.T) {
	t.Parallel()
	cfg, err := ConfigFromEnv(envOf(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuer != testBaseURL || cfg.Resource != testBaseURL+"/mcp" || cfg.ClientID != "dots" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.RedirectURIs) != 1 || cfg.RedirectURIs[0] != testRedirect {
		t.Fatalf("redirect uris = %v", cfg.RedirectURIs)
	}
	if cfg.AccessTokenTTL.Hours() != 1 || cfg.RefreshTokenTTL.Hours() != 720 || cfg.CodeTTL.Seconds() != 60 {
		t.Fatalf("ttl defaults: %v %v %v", cfg.AccessTokenTTL, cfg.RefreshTokenTTL, cfg.CodeTTL)
	}
	if len(cfg.TrustedProxies) == 0 {
		t.Fatal("default trusted proxies missing")
	}
}

func TestConfigFromEnv_RedirectDefaultAndCIMDDefaults(t *testing.T) {
	t.Parallel()
	env := validEnv()
	delete(env, EnvRedirectURIs)
	cfg, err := ConfigFromEnv(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RedirectURIs) != 1 || cfg.RedirectURIs[0] != DefaultRedirectURI {
		t.Fatalf("default redirect = %v", cfg.RedirectURIs)
	}
	if strings.Join(cfg.CIMDClientIDPatterns, ",") != "https://chatgpt.com/oauth/client.json,https://chatgpt.com/oauth/{id}/client.json" ||
		strings.Join(cfg.CIMDRedirectHosts, ",") != "chatgpt.com" {
		t.Fatalf("cimd defaults = %v %v", cfg.CIMDClientIDPatterns, cfg.CIMDRedirectHosts)
	}
	// 명시하면 기본값을 대체한다.
	env[EnvRedirectURIs] = "https://chatgpt.com/connector/oauth/abc"
	cfg, _ = ConfigFromEnv(envOf(env))
	if len(cfg.RedirectURIs) != 1 || cfg.RedirectURIs[0] != "https://chatgpt.com/connector/oauth/abc" {
		t.Fatalf("explicit redirect = %v", cfg.RedirectURIs)
	}
	// CIMD 끄기
	env[EnvCIMDPatterns] = "off"
	cfg, err = ConfigFromEnv(envOf(env))
	if err != nil || len(cfg.CIMDClientIDPatterns) != 0 {
		t.Fatalf("cimd off: %v %v", cfg, err)
	}
}

func TestConfigFromEnv_Normalisation(t *testing.T) {
	t.Parallel()
	env := validEnv()
	env[EnvPublicBaseURL] = "https://Brain-MCP.Example.com/"
	env[EnvRedirectURIs] = " " + testRedirect + " , https://chatgpt.com/connector/oauth/abc ,," + testRedirect
	env[EnvClientID] = "my-client"
	env[EnvOwnerKeySHA256] = strings.ToUpper(ownerHash(testOwnerKey))
	cfg, err := ConfigFromEnv(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuer != "https://brain-mcp.example.com" {
		t.Fatalf("issuer = %q", cfg.Issuer)
	}
	if len(cfg.RedirectURIs) != 2 {
		t.Fatalf("redirect uris (dedup) = %v", cfg.RedirectURIs)
	}
	if cfg.ClientID != "my-client" {
		t.Fatalf("client id = %q", cfg.ClientID)
	}
}

func TestConfigFromEnv_PartialIsFatal(t *testing.T) {
	t.Parallel()
	for _, drop := range []string{EnvPublicBaseURL, EnvOwnerKeySHA256, EnvSigningKey} {
		env := validEnv()
		delete(env, drop)
		_, err := ConfigFromEnv(envOf(env))
		if err == nil {
			t.Fatalf("missing %s: expected error", drop)
		}
		if !strings.Contains(err.Error(), drop) {
			t.Errorf("error for missing %s does not name it: %v", drop, err)
		}
	}
	// 보조 변수 하나만 있어도 "켜려는 의도"로 보고 필수 값 누락을 지적한다.
	_, err := ConfigFromEnv(envOf(map[string]string{EnvClientID: "dots"}))
	if err == nil {
		t.Fatal("client id alone must be treated as partial configuration")
	}
}

func TestConfigFromEnv_Invalid(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]string{
		"http base":          {EnvPublicBaseURL: "http://brain-mcp.example.com"},
		"base with path":     {EnvPublicBaseURL: "https://brain-mcp.example.com/mcp"},
		"base with query":    {EnvPublicBaseURL: "https://brain-mcp.example.com?x=1"},
		"base userinfo":      {EnvPublicBaseURL: "https://u:p@brain-mcp.example.com"},
		"base relative":      {EnvPublicBaseURL: "brain-mcp.example.com"},
		"resource other org": {EnvResource: "https://evil.example.org/mcp"},
		"resource no path":   {EnvResource: testBaseURL},
		"resource query":     {EnvResource: testBaseURL + "/mcp?x=1"},
		"redirect http":      {EnvRedirectURIs: "http://chatgpt.com/cb"},
		"redirect wildcard":  {EnvRedirectURIs: "https://chatgpt.com/connector/oauth/*"},
		"redirect template":  {EnvRedirectURIs: "https://chatgpt.com/connector/oauth/{callback_id}"},
		"redirect fragment":  {EnvRedirectURIs: testRedirect + "#frag"},
		"redirect relative":  {EnvRedirectURIs: "/cb"},
		"redirect empty":     {EnvRedirectURIs: " , "},
		"hash short":         {EnvOwnerKeySHA256: "abcd"},
		"hash not hex":       {EnvOwnerKeySHA256: strings.Repeat("zz", 32)},
		"signing short":      {EnvSigningKey: "too-short"},
		"client id bad":      {EnvClientID: "has space"},
		"proxies bad":        {EnvTrustedProxies: "not-an-ip"},
		"cimd http":          {EnvCIMDPatterns: "http://chatgpt.com/oauth/client.json"},
		"cimd wildcard":      {EnvCIMDPatterns: "https://*.chatgpt.com/oauth/client.json"},
		"cimd partial seg":   {EnvCIMDPatterns: "https://chatgpt.com/oauth/x{id}/client.json"},
		"cimd bare host":     {EnvCIMDPatterns: "https://chatgpt.com/"},
		"cimd query":         {EnvCIMDPatterns: "https://chatgpt.com/oauth/client.json?a=1"},
		"cimd redirect host": {EnvCIMDRedirectHost: "https://chatgpt.com"},
		"cimd redirect ip":   {EnvCIMDRedirectHost: "127.0.0.1"},
	}
	for name, override := range cases {
		env := validEnv()
		for k, v := range override {
			env[k] = v
		}
		cfg, err := ConfigFromEnv(envOf(env))
		if err == nil {
			t.Errorf("%s: expected error, got config %+v", name, cfg)
			continue
		}
		// 오류 문구에 비밀 값이 실리면 안 된다.
		for _, secret := range []string{testSigningKey, testOwnerKey, ownerHash(testOwnerKey)} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error leaks a secret: %v", name, err)
			}
		}
	}
}

func TestConfigFromEnv_LoopbackHTTPAllowedForDev(t *testing.T) {
	t.Parallel()
	env := validEnv()
	env[EnvPublicBaseURL] = "http://localhost:8090"
	env[EnvRedirectURIs] = "http://127.0.0.1:3000/callback"
	cfg, err := ConfigFromEnv(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuer != "http://localhost:8090" || cfg.Resource != "http://localhost:8090/mcp" {
		t.Fatalf("unexpected: %+v", cfg)
	}
}

func TestConfigFromEnv_TrustedProxies(t *testing.T) {
	t.Parallel()
	env := validEnv()
	env[EnvTrustedProxies] = "10.0.0.0/8, 192.168.1.5"
	cfg, err := ConfigFromEnv(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[1].Bits() != 32 {
		t.Fatalf("proxies = %v", cfg.TrustedProxies)
	}
}
