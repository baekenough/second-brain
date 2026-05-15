package setup

import (
	"os"
	"strings"
)

// lang is the two-letter language code detected at startup.
// Supported values: "ko", "en". Defaults to "en".
var lang = detectLang()

// detectLang infers the preferred language from the LANG or LANGUAGE
// environment variable. It handles values such as:
//
//	ko_KR.UTF-8  → "ko"
//	en_US.UTF-8  → "en"
//	C            → "en"
//	POSIX        → "en"
//	(empty)      → "en"
func detectLang() string {
	for _, key := range []string{"LANG", "LANGUAGE"} {
		val := os.Getenv(key)
		if val == "" {
			continue
		}
		// LANGUAGE may be colon-separated priority list; take the first.
		val = strings.SplitN(val, ":", 2)[0]
		// Strip encoding suffix (e.g. ".UTF-8").
		if idx := strings.IndexByte(val, '.'); idx != -1 {
			val = val[:idx]
		}
		// Strip territory suffix (e.g. "_KR").
		if idx := strings.IndexByte(val, '_'); idx != -1 {
			val = val[:idx]
		}
		val = strings.ToLower(val)
		switch val {
		case "ko":
			return "ko"
		case "en", "c", "posix":
			return "en"
		}
	}
	return "en"
}

// msg returns the localised string for key, falling back to English.
func msg(key string) string {
	if m, ok := messages[lang]; ok {
		if s, ok := m[key]; ok {
			return s
		}
	}
	return messages["en"][key]
}

// messages holds UI strings keyed by language code then message key.
var messages = map[string]map[string]string{
	"en": {
		"welcome":          "second-brain setup wizard",
		"welcome_desc":     "This wizard will help you configure your .env file. Press Enter to confirm each field, Ctrl+C to abort.",
		"channel_title":    "Select channels to configure",
		"channel_desc":     "Use space to toggle, enter to confirm.",
		"info_only_prefix": "ℹ",
		"skip_empty":       "(skipped — field left blank)",
		"written":          "Configuration written to %s",
		"backup":           "Existing .env backed up to %s",
		"aborted":          "Setup aborted.",
		"done":             "Done! Start the collector with: go run ./cmd/collector/",
		"stub_error":       "rebuild with -tags setup to enable the interactive wizard",
		"non_interactive":  "--non-interactive flag: running in accessible (plain-text) mode",
	},
	"ko": {
		"welcome":          "second-brain 설정 마법사",
		"welcome_desc":     "이 마법사가 .env 파일 설정을 도와드립니다. 각 항목을 입력한 후 Enter를 누르세요. Ctrl+C로 중단할 수 있습니다.",
		"channel_title":    "설정할 채널을 선택하세요",
		"channel_desc":     "스페이스로 선택/해제, Enter로 확인합니다.",
		"info_only_prefix": "ℹ",
		"skip_empty":       "(입력 없음 — 건너뜀)",
		"written":          "%s 파일에 설정이 저장되었습니다",
		"backup":           "기존 .env 파일을 %s 으로 백업했습니다",
		"aborted":          "설정이 취소되었습니다.",
		"done":             "완료! 다음 명령으로 수집기를 시작하세요: go run ./cmd/collector/",
		"stub_error":       "-tags setup 플래그로 다시 빌드해야 인터랙티브 마법사를 사용할 수 있습니다",
		"non_interactive":  "--non-interactive 플래그: 텍스트 전용 모드로 실행합니다",
	},
}
