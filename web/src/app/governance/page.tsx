import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "거버넌스 — Second Brain",
  description: "큐레이션, PII 가드레일, 지식 그래프 거버넌스",
};

// ── PII Guardrail Status ──────────────────────────────────────────────────

type PIIStatus = "covered" | "partial" | "pending" | "na";

interface PIIRow {
  source: string;
  label: string;
  status: PIIStatus;
  notes: string;
}

const PII_ROWS: PIIRow[] = [
  {
    source: "sms",
    label: "SMS",
    status: "covered",
    notes: "OTP 패턴 해시, 전화번호 마스킹 적용",
  },
  {
    source: "call-log",
    label: "통화 로그",
    status: "covered",
    notes: "발신/수신 번호 메타데이터만 저장, 오디오 없음",
  },
  {
    source: "call-transcript",
    label: "통화 전사",
    status: "partial",
    notes: "전사 텍스트에 PII 필터 미적용 (#112 #2a). 주민번호·계좌·OTP 포함 가능",
  },
  {
    source: "gmail",
    label: "Gmail",
    status: "pending",
    notes: "PII 분석 미완료. 개인정보 포함 가능성 높음",
  },
  {
    source: "calendar",
    label: "Calendar",
    status: "partial",
    notes: "참석자 이메일·전화번호 노출. 필터 미적용",
  },
  {
    source: "filesystem",
    label: "Files",
    status: "na",
    notes: "사용자 직접 업로드 — PII 책임은 사용자",
  },
  {
    source: "upload",
    label: "Upload",
    status: "na",
    notes: "직접 업로드 파일. PII 책임은 업로드 주체",
  },
];

const STATUS_CONFIG: Record<PIIStatus, { label: string; className: string }> = {
  covered: { label: "✅ 적용됨", className: "text-success" },
  partial: { label: "⚠️ 부분적용", className: "text-warning" },
  pending: { label: "🔴 미적용", className: "text-danger" },
  na: { label: "— 해당없음", className: "text-foreground-subtle" },
};

function PIIGuardrailTable() {
  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <table className="min-w-full text-sm">
        <thead className="bg-surface-subtle">
          <tr>
            <th className="px-4 py-3 text-left text-xs font-semibold tracking-wide text-foreground-muted uppercase">
              소스
            </th>
            <th className="px-4 py-3 text-left text-xs font-semibold tracking-wide text-foreground-muted uppercase">
              PII 가드레일
            </th>
            <th className="px-4 py-3 text-left text-xs font-semibold tracking-wide text-foreground-muted uppercase">
              비고
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {PII_ROWS.map((row) => {
            const cfg = STATUS_CONFIG[row.status];
            return (
              <tr key={row.source} className="bg-surface">
                <td className="px-4 py-3 text-xs font-medium text-foreground">{row.label}</td>
                <td className={`px-4 py-3 text-xs font-medium ${cfg.className}`}>{cfg.label}</td>
                <td className="px-4 py-3 text-xs text-foreground-muted">{row.notes}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ── Placeholder panels ────────────────────────────────────────────────────

function PlaceholderPanel({
  title,
  description,
  issueRef,
  children,
}: {
  title: string;
  description: string;
  issueRef?: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="space-y-3 rounded-lg border border-dashed border-border bg-surface-subtle p-6">
      <div className="flex items-start justify-between">
        <div>
          <h3 className="font-serif text-base font-semibold text-foreground">{title}</h3>
          <p className="mt-1 text-sm text-foreground-muted">{description}</p>
        </div>
        {issueRef && (
          <span className="shrink-0 rounded border border-border bg-surface px-2 py-0.5 font-mono text-xs text-foreground-subtle">
            {issueRef}
          </span>
        )}
      </div>
      {children}
      <div className="rounded-md border border-border bg-surface px-3 py-2">
        <p className="text-xs text-foreground-subtle">
          🚧 백엔드 API 구현 대기 중. 이 패널은 구조 확인을 위한 플레이스홀더입니다.
        </p>
      </div>
    </div>
  );
}

// ── Page ─────────────────────────────────────────────────────────────────

export default function GovernancePage() {
  return (
    <div className="space-y-8">
      {/* Page header */}
      <header>
        <h1 className="font-serif text-2xl font-semibold text-foreground">거버넌스</h1>
        <p className="mt-1 text-sm text-foreground-muted">
          큐레이션 · PII 가드레일 · 지식 그래프 (#112 연계)
        </p>
      </header>

      {/* PII Guardrail Status */}
      <section className="space-y-3">
        <div className="flex items-baseline justify-between">
          <h2 className="text-sm font-semibold text-foreground">PII 가드레일 현황</h2>
          <span className="rounded border border-border bg-surface px-2 py-0.5 font-mono text-xs text-foreground-subtle">
            #112 #2a
          </span>
        </div>
        <p className="text-xs text-foreground-muted">
          각 소스 타입의 개인정보(PII) 필터링 적용 상태입니다.{" "}
          <span className="text-warning">⚠️ 통화 전사</span>에 PII 필터가 미적용 상태입니다 — 전사
          텍스트에 주민번호·계좌번호·OTP가 포함될 수 있습니다.
        </p>
        <PIIGuardrailTable />
      </section>

      {/* HITL Curation Queue */}
      <section className="space-y-3">
        <h2 className="text-sm font-semibold text-foreground">HITL 큐레이션 큐</h2>
        <PlaceholderPanel
          title="능동 Pruning — HITL 삭제 승인"
          description="kimi-k2.6이 중복/구식/저효용 문서를 식별하면 여기서 검토하고 삭제를 승인합니다. 비가역 작업(R001)이므로 인간 확인이 필요합니다."
          issueRef="#112 #1 #2b"
        >
          <ul className="list-disc space-y-1 pl-4 text-xs text-foreground-muted">
            <li>큐레이션 LLM이 주기적으로 후보 문서 생성</li>
            <li>HITL 게이트: 사용자가 &quot;삭제 승인&quot; / &quot;유지&quot; 결정</li>
            <li>승인 후 소프트 삭제(status=deleted) → 검색에서 제외</li>
            <li>
              구현 필요: <code className="font-mono">GET /api/v1/governance/curation-queue</code>
            </li>
          </ul>
        </PlaceholderPanel>
      </section>

      {/* Knowledge Graph */}
      <section className="space-y-3">
        <h2 className="text-sm font-semibold text-foreground">지식 그래프 뷰</h2>
        <PlaceholderPanel
          title="GraphRAG — 인물·조직·사건 관계 탐색"
          description="통화 전사·SMS·Gmail에서 엔티티(인물·조직·사건)와 관계를 추출해 그래프로 시각화합니다. 인물 중심 타임라인, 관계 탐색 검색 품질 향상을 목표로 합니다."
          issueRef="#112 #3"
        >
          <ul className="list-disc space-y-1 pl-4 text-xs text-foreground-muted">
            <li>엔티티·관계 추출 파이프라인 구현 필요 (백엔드)</li>
            <li>그래프 렌더링: D3.js 또는 Cytoscape.js 예정</li>
            <li>
              구현 필요: <code className="font-mono">GET /api/v1/governance/graph</code>
            </li>
          </ul>
        </PlaceholderPanel>
      </section>

      {/* kimi-k2.6 Activity Log */}
      <section className="space-y-3">
        <h2 className="text-sm font-semibold text-foreground">kimi-k2.6 활동 로그</h2>
        <PlaceholderPanel
          title="큐레이션 LLM 활동 로그"
          description="kimi-k2.6이 수행한 큐레이션 작업, 검색 re-ranking, HITL 제안 내역을 시간순으로 표시합니다."
          issueRef="#112 #2"
        >
          <ul className="list-disc space-y-1 pl-4 text-xs text-foreground-muted">
            <li>큐레이션 요청·응답 로그 저장 필요 (백엔드)</li>
            <li>
              구현 필요: <code className="font-mono">GET /api/v1/governance/curation-log</code>
            </li>
          </ul>
        </PlaceholderPanel>
      </section>
    </div>
  );
}
