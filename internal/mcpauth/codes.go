package mcpauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

// codeTombstone 은 사용된 인가 코드의 기록을 남겨 두는 시간이다. 이 안에
// 같은 코드가 다시 오면 재사용(replay)으로 보고 그 코드로 발급한 토큰 계보를
// 폐기한다(RFC 6749 §4.1.2).
const codeTombstone = 10 * time.Minute

// authCode 는 발급된 인가 코드에 묶인 값이다. 토큰 교환 때 모두 다시 대조한다.
type authCode struct {
	clientID      string
	redirectURI   string
	challenge     string
	scope         string
	resource      string
	expiresAt     time.Time
	used          bool
	replayed      bool // 사용된 뒤 같은 코드가 다시 제시됨(발급 도중 재사용 탐지용)
	familyID      string
	tombstoneEnds time.Time
}

type codeStatus int

const (
	codeOK codeStatus = iota
	codeUnknown
	codeExpired
	codeReplayed
)

// codeStore 는 인가 코드를 메모리에 둔다. 코드 수명이 60초라 재시작 때 잃어도
// 사용자는 연결을 다시 시도하면 된다. 맵 키는 코드의 해시여서 맵 덤프로 코드가
// 새지 않는다.
type codeStore struct {
	mu    sync.Mutex
	now   func() time.Time
	codes map[[32]byte]*authCode
}

func newCodeStore(now func() time.Time) *codeStore {
	return &codeStore{now: now, codes: map[[32]byte]*authCode{}}
}

func codeKey(code string) [32]byte { return sha256.Sum256([]byte(code)) }

// randomToken 은 n 바이트 무작위 값을 base64url(패딩 없음)로 돌려준다.
func randomToken(n int) string {
	b := make([]byte, n)
	// Go 1.24+ 의 crypto/rand.Read 는 실패 시 프로세스를 중단한다.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// issue 는 새 코드를 만들어 저장하고 원문을 돌려준다.
func (c *codeStore) issue(rec authCode, ttl time.Duration) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	code := randomToken(32)
	rec.expiresAt = c.now().Add(ttl)
	rec.tombstoneEnds = rec.expiresAt.Add(codeTombstone)
	c.codes[codeKey(code)] = &rec
	return code
}

// consume 은 코드를 한 번만 쓸 수 있게 소모한다. 검증 결과와 무관하게 첫
// 제시에서 소모되므로 PKCE 검증이 실패해도 같은 코드로 다시 시도할 수 없다.
// 재사용이면 codeReplayed 와 그 코드가 낳은 계보 ID 를 돌려준다.
func (c *codeStore) consume(code string) (authCode, codeStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	rec, ok := c.codes[codeKey(code)]
	if !ok {
		return authCode{}, codeUnknown
	}
	if rec.used {
		rec.replayed = true
		return *rec, codeReplayed
	}
	rec.used = true
	if !c.now().Before(rec.expiresAt) {
		return authCode{}, codeExpired
	}
	return *rec, codeOK
}

// attachFamily 는 소모한 코드에 그 코드로 만든 토큰 계보를 기록한다.
func (c *codeStore) attachFamily(code, familyID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, ok := c.codes[codeKey(code)]; ok {
		rec.familyID = familyID
	}
}

// wasReplayed 는 코드가 소모된 뒤 다시 제시된 적이 있는지 본다. 토큰 발급(저장소 쓰기)
// 도중에 들어온 재사용을 발급이 끝난 뒤에 잡는 데 쓴다.
func (c *codeStore) wasReplayed(code string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.codes[codeKey(code)]
	return ok && rec.replayed
}

func (c *codeStore) sweepLocked() {
	now := c.now()
	for k, rec := range c.codes {
		if now.After(rec.tombstoneEnds) {
			delete(c.codes, k)
		}
	}
}

// maxNonces 는 기억하는 nonce 수의 상한이다.
const maxNonces = 10000

type nonceEntry struct {
	nonce string
	exp   time.Time
}

// nonceStore 는 동의 폼 토큰의 일회용 nonce 를 추적한다. 승인 시도(속도 제한을 통과한
// 것)만 기록하므로 크기는 시도 한도로 묶인다. 모든 nonce 의 수명이 같아 삽입 순서가
// 곧 만료 순서이므로 앞에서부터 O(1) 로 정리·축출한다.
type nonceStore struct {
	mu   sync.Mutex
	now  func() time.Time
	used map[string]struct{}
	q    []nonceEntry
	head int
}

func newNonceStore(now func() time.Time) *nonceStore {
	return &nonceStore{now: now, used: map[string]struct{}{}}
}

func (n *nonceStore) popLocked() {
	delete(n.used, n.q[n.head].nonce)
	n.q[n.head] = nonceEntry{}
	n.head++
	if n.head > 1024 && n.head*2 > len(n.q) { // 가끔 앞 빈 칸을 걷어낸다
		n.q = append([]nonceEntry(nil), n.q[n.head:]...)
		n.head = 0
	}
}

// consume 은 nonce 를 처음 쓰면 true 다. exp 이후에는 토큰 자체가 만료로 거절되므로
// 그때까지만 기억하면 충분하다. 상한에 닿으면 실패하지 않고 가장 오래된 항목을
// 축출한다: 폭주 상황에서 소유자의 승인이 막히는 것보다, 축출된 오래된 토큰이
// (소유자 키 없이는 쓸 수 없는 채로) 재생 가능해지는 쪽이 낫다.
func (n *nonceStore) consume(nonce string, exp time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	for n.head < len(n.q) && now.After(n.q[n.head].exp) {
		n.popLocked()
	}
	if _, seen := n.used[nonce]; seen {
		return false
	}
	for len(n.used) >= maxNonces && n.head < len(n.q) {
		n.popLocked()
	}
	n.used[nonce] = struct{}{}
	n.q = append(n.q, nonceEntry{nonce: nonce, exp: exp})
	return true
}
