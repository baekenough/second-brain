package mcpauth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// 소유자 키 시도 제한. 시도는 키를 비교하기 "전에" 예약하고, 성공하면 되돌린다.
// 실패 뒤에 세는 방식은 동시 요청 N개가 모두 한도를 통과해 버리는 경쟁이 있다.
const (
	attemptWindow = 15 * time.Minute
	attemptsPerIP = 5
	// attemptsGlobally 는 전체 합산 상한이다. 소유자 키는 무작위 32바이트라 무차별
	// 대입은 IP 별 제한만으로도 무의미하다. 전체 상한은 여러 IP 에서 오는 대규모
	// 공격(비용·로그 폭주)만 막는 용도라 높게 둔다. 낮게(예: 20) 두면 IP 4개가 15분마다
	// 5회씩만 실패해도 소유자가 계속 잠기는 DoS 가 된다.
	attemptsGlobally = 500
)

type attemptEntry struct {
	id uint64
	at time.Time
}

// attemptLimiter 는 슬라이딩 윈도우 방식의 시도 제한이다. 한도에 닿아 거절된
// 요청은 기록하지 않으므로 계속 두드려도 잠금이 연장되지 않고, 가장 오래된
// 시도가 윈도우를 벗어나면 풀린다. 거절된 동안에는 올바른 키도 평가하지 않는다.
//
// 거절된 요청을 기록하지 않기 때문에 전체 항목 수는 globalMax 이하이고
// 맵 크기도 그 이하다 — 출처를 바꿔 가며 메모리를 키울 수 없다.
type attemptLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	window    time.Duration
	perKeyMax int
	globalMax int
	nextID    uint64
	perKey    map[string][]attemptEntry
	global    []attemptEntry
}

func newAttemptLimiter(now func() time.Time, window time.Duration, perKey, global int) *attemptLimiter {
	return &attemptLimiter{now: now, window: window, perKeyMax: perKey, globalMax: global,
		perKey: map[string][]attemptEntry{}}
}

func prune(es []attemptEntry, cutoff time.Time) []attemptEntry {
	i := 0
	for i < len(es) && !es[i].at.After(cutoff) {
		i++
	}
	return es[i:]
}

// reserve 는 key 의 시도 하나를 예약한다. 거절되면 ok=false 와 풀리기까지의
// 대기 시간을 돌려준다. 성공 시 돌려주는 release 는 시도가 성공했을 때
// 불러 예약을 되돌린다(정상 사용자가 한도를 갉아먹지 않게).
func (l *attemptLimiter) reserve(key string) (release func(), ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	cutoff := now.Add(-l.window)
	l.global = prune(l.global, cutoff)
	for k, es := range l.perKey {
		es = prune(es, cutoff)
		if len(es) == 0 {
			delete(l.perKey, k)
		} else {
			l.perKey[k] = es
		}
	}

	if len(l.global) >= l.globalMax {
		return nil, false, l.global[0].at.Add(l.window).Sub(now)
	}
	if es := l.perKey[key]; len(es) >= l.perKeyMax {
		return nil, false, es[0].at.Add(l.window).Sub(now)
	}

	l.nextID++
	e := attemptEntry{id: l.nextID, at: now}
	l.global = append(l.global, e)
	l.perKey[key] = append(l.perKey[key], e)

	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.global = removeEntry(l.global, e.id)
		if es := removeEntry(l.perKey[key], e.id); len(es) == 0 {
			delete(l.perKey, key)
		} else {
			l.perKey[key] = es
		}
	}, true, 0
}

func removeEntry(es []attemptEntry, id uint64) []attemptEntry {
	for i := range es {
		if es[i].id == id {
			out := make([]attemptEntry, 0, len(es)-1)
			out = append(out, es[:i]...)
			return append(out, es[i+1:]...)
		}
	}
	return es
}

// clientKey 는 속도 제한의 키가 되는 클라이언트 주소를 정한다.
//
// cloudflared 뒤에서는 RemoteAddr 가 루프백이나 도커 게이트웨이라 모든 요청이
// 같은 주소로 보인다. 그래서 CF-Connecting-IP 를 쓰되, 요청을 실제로 보낸
// 연결(RemoteAddr)이 신뢰 프록시 대역일 때만 믿는다. 아니면 헤더는 공격자가
// 마음대로 쓸 수 있는 값이므로 무시하고 RemoteAddr 를 쓴다. X-Forwarded-For 등
// 다른 헤더는 어떤 경우에도 보지 않는다.
//
// IPv6 는 /64 로 묶는다. 한 가입자가 /64 안의 주소를 마음대로 바꿔 가며 쓸 수
// 있어서 주소 단위로 세면 제한을 비껴간다.
func clientKey(r *http.Request, trusted []netip.Prefix) string {
	remote := parseHostAddr(r.RemoteAddr)
	if remote.IsValid() && inPrefixes(remote, trusted) {
		if h := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); h != "" {
			if a, err := netip.ParseAddr(h); err == nil {
				return normalizeKey(a)
			}
		}
	}
	if !remote.IsValid() {
		return "unknown"
	}
	return normalizeKey(remote)
}

func parseHostAddr(hostport string) netip.Addr {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.WithZone("")
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func normalizeKey(a netip.Addr) string {
	a = a.Unmap().WithZone("")
	if a.Is6() {
		p, err := a.Prefix(64)
		if err == nil {
			return p.String()
		}
	}
	return a.String()
}
