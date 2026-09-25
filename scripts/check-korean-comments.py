#!/usr/bin/env python3
"""새로 추가된 주석 중 한글이 없는 줄을 경고로 보고한다 (#285).

unified diff 를 표준 입력으로 받는다 — 호출부(ci-checks.sh)가 CI 에서는
커밋 diff(`<base>..HEAD`)를, 로컬에서는 작업트리+미추적 파일까지 포함한
diff 를 만들어 이 스크립트로 파이프한다. `+`로 시작하는 추가 줄만 보고,
Go(`//`)·셸/YAML/Makefile(`#`) 주석 프리픽스를 가진 줄에서 한글(가-힣)이
하나도 없으면 경고한다. "검사한 파일 수" 를 항상 출력해 검사 대상이
0건인 상태(=커밋 diff 모드에서 미커밋 변경을 놓치는 가짜 통과)를 눈으로
구분할 수 있게 한다. 차단용이 아니므로 항상 exit code 0으로 종료한다 —
호출부가 경고 유무와 무관하게 다음 단계로 진행할 수 있어야 한다.
"""

from __future__ import annotations

import re
import sys

HANGUL = re.compile(r"[가-힣]")
# 식별자 하나뿐인 주석(예: "// TODO" 없이 "// fooBar")은 번역 대상이 아니므로 제외한다.
IDENT_ONLY = re.compile(r"^[A-Za-z_][A-Za-z0-9_.:/-]*$")


def extract_comment(current_file: str, stripped: str) -> str | None:
    """주석 프리픽스를 벗겨 본문을 돌려준다. 주석이 아니면 None."""
    if current_file.endswith(".go") and stripped.startswith("//"):
        return stripped[2:]
    is_shellish = current_file.endswith((".sh", ".yml", ".yaml")) or current_file.endswith(
        "Makefile"
    )
    if is_shellish and stripped.startswith("#"):
        return stripped[1:]
    return None


def is_excluded(stripped: str) -> bool:
    """shebang·nosec·go 지시어·shellcheck 지시어는 번역 검사 대상이 아니다."""
    return (
        stripped.startswith("#!")
        or stripped.startswith("#nosec")
        or stripped.startswith("# nosec")
        or stripped.startswith("//go:")
        or stripped.startswith("# shellcheck")
    )


def main() -> int:
    current_file: str | None = None
    files_seen: set[str] = set()
    warnings: list[str] = []

    for raw in sys.stdin:
        line = raw.rstrip("\n")
        if line.startswith("+++ "):
            path = line[4:]
            current_file = path[2:] if path.startswith("b/") else path
            if current_file != "/dev/null":
                files_seen.add(current_file)
            continue
        if not line.startswith("+") or line.startswith("+++"):
            continue
        if current_file is None or current_file == "/dev/null":
            continue

        stripped = line[1:].strip()
        if not stripped or is_excluded(stripped):
            continue

        comment_text = extract_comment(current_file, stripped)
        if comment_text is None:
            continue

        comment_text = comment_text.strip()
        if not comment_text or IDENT_ONLY.match(comment_text):
            continue

        if not HANGUL.search(comment_text):
            warnings.append(f"{current_file}: {stripped}")

    # 항상 출력한다 — "검사 대상 0건"이 곧 통과가 아니라 검사 자체가 안 된
    # 것일 수 있으므로, N=0 이 눈에 띄어야 그 차이를 알아챌 수 있다(#285 보완).
    print(f"검사한 파일 수: {len(files_seen)}, 경고: {len(warnings)}")

    if warnings:
        print("::warning::새로 추가된 주석 중 한글이 없는 줄이 있습니다 (번역 검토 권장)")
        for warning in warnings:
            print(f"  {warning}")
    else:
        print("OK: 새로 추가된 주석은 모두 한글을 포함합니다")

    return 0  # 경고 전용 — exit code 에 영향을 주지 않는다.


if __name__ == "__main__":
    sys.exit(main())
