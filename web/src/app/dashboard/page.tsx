"use client";

import { useState, useEffect } from "react";
import { getStats, getBaselineStats, listRecentByKind } from "@/lib/api";
import type { StatsResponse, RecentItem, RecentItemsResponse } from "@/lib/types";
import { formatDateTime, formatRelative } from "@/lib/dates";
import { SOURCE_LABELS, DASHBOARD_SOURCES, CUTOVER_DATE } from "@/lib/constants";
import type { SourceType } from "@/lib/types";

// ── Source stats card ─────────────────────────────────────────────────────

interface SourceCardProps {
  source: SourceType;
  count: number | undefined;
  lastCollected?: string;
}

function SourceCard({ source, count, lastCollected }: SourceCardProps) {
  return (
    <div className="space-y-2 rounded-lg border border-border bg-surface p-4">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium tracking-wide text-foreground-muted uppercase">
          {SOURCE_LABELS[source]}
        </span>
        {count !== undefined && (
          <span className="font-serif text-2xl font-semibold text-foreground tabular-nums">
            {count.toLocaleString()}
          </span>
        )}
      </div>
      {lastCollected && (
        <p className="text-xs text-foreground-subtle">최근 수집 {formatRelative(lastCollected)}</p>
      )}
      {count === undefined && <div className="h-8 animate-pulse rounded bg-surface-subtle" />}
    </div>
  );
}

// ── Recent items panel ────────────────────────────────────────────────────

interface RecentPanelProps {
  title: string;
  items: RecentItem[];
  loading: boolean;
  kind: string;
}

function RecentPanel({ title, items, loading, kind }: RecentPanelProps) {
  return (
    <div className="rounded-lg border border-border bg-surface">
      <div className="border-b border-border px-4 py-3">
        <h3 className="text-sm font-medium text-foreground">{title}</h3>
        <p className="mt-0.5 text-xs text-foreground-subtle">
          {loading ? "불러오는 중…" : `${items.length}건 (kind: ${kind})`}
        </p>
      </div>
      <ul className="divide-y divide-border">
        {loading && (
          <li className="px-4 py-3">
            <div className="h-4 w-3/4 animate-pulse rounded bg-surface-subtle" />
          </li>
        )}
        {!loading && items.length === 0 && (
          <li className="px-4 py-3 text-sm text-foreground-subtle">항목 없음</li>
        )}
        {items.slice(0, 5).map((item) => (
          <li key={item.id} className="px-4 py-2.5">
            <a href={`/documents/${item.id}`} className="block transition-colors hover:text-accent">
              <p className="line-clamp-1 text-sm text-foreground">{item.title}</p>
              <p className="mt-0.5 text-xs text-foreground-subtle">
                {item.occurred_at
                  ? formatDateTime(item.occurred_at)
                  : formatDateTime(item.collected_at)}
              </p>
            </a>
          </li>
        ))}
      </ul>
    </div>
  );
}

// ── Whisper queue estimation ──────────────────────────────────────────────

interface WhisperQueueProps {
  callLogCount: number | undefined;
  callTranscriptCount: number | undefined;
}

function WhisperQueue({ callLogCount, callTranscriptCount }: WhisperQueueProps) {
  if (callLogCount === undefined || callTranscriptCount === undefined) {
    return (
      <div className="rounded-lg border border-border bg-surface p-4">
        <div className="h-8 animate-pulse rounded bg-surface-subtle" />
      </div>
    );
  }

  const pending = Math.max(0, callLogCount - callTranscriptCount);
  const isHealthy = pending === 0;

  return (
    <div
      className={`space-y-2 rounded-lg border p-4 ${
        isHealthy ? "border-border bg-surface" : "border-warning/30 bg-warning/5"
      }`}
    >
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium tracking-wide text-foreground-muted uppercase">
          Whisper 전사 큐
        </span>
        <span
          className={`font-serif text-2xl font-semibold tabular-nums ${
            isHealthy ? "text-success" : "text-warning"
          }`}
        >
          {pending.toLocaleString()}
        </span>
      </div>
      <p className="text-xs text-foreground-subtle">
        통화 로그 {callLogCount.toLocaleString()}건 · 전사 완료{" "}
        {callTranscriptCount.toLocaleString()}건{pending > 0 && ` · ${pending}건 대기 중`}
      </p>
    </div>
  );
}

// ── Cutover boundary ─────────────────────────────────────────────────────

function CutoverBanner({ total }: { total: number | undefined }) {
  return (
    <div className="rounded-lg border border-border bg-surface-subtle px-4 py-3 text-xs text-foreground-muted">
      <span className="font-medium text-foreground">Cutover </span>
      <span className="font-mono">{CUTOVER_DATE}</span>
      {" — "}이 날짜 이후 수집된 문서는 신규 파이프라인을 사용합니다.
      {total !== undefined && (
        <span className="ml-2 text-foreground-subtle">전체 {total.toLocaleString()}건</span>
      )}
    </div>
  );
}

// ── Page ─────────────────────────────────────────────────────────────────

export default function DashboardPage() {
  const [stats, setStats] = useState<StatsResponse | null>(null);
  // baseline stats reserved for future percentile charts
  const [, setBaselineStats] = useState<unknown>(null);
  const [smsRecent, setSmsRecent] = useState<RecentItem[]>([]);
  const [callRecent, setCallRecent] = useState<RecentItem[]>([]);
  const [voiceRecent, setVoiceRecent] = useState<RecentItem[]>([]);
  const [loadingStats, setLoadingStats] = useState(true);
  const [loadingRecent, setLoadingRecent] = useState(true);

  useEffect(() => {
    let cancelled = false;

    Promise.all([getStats(), getBaselineStats()])
      .then(([s, b]) => {
        if (cancelled) return;
        setStats(s);
        setBaselineStats(b);
      })
      .catch(console.error)
      .finally(() => {
        if (!cancelled) setLoadingStats(false);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    // loadingRecent starts as true (useState(true)) — no synchronous setState needed here
    let cancelled = false;

    Promise.all([
      listRecentByKind("sms", 10),
      listRecentByKind("call-recording", 10),
      listRecentByKind("voice-memo", 10),
    ])
      .then(
        ([sms, call, voice]: [RecentItemsResponse, RecentItemsResponse, RecentItemsResponse]) => {
          if (cancelled) return;
          setSmsRecent(sms.items ?? []);
          setCallRecent(call.items ?? []);
          setVoiceRecent(voice.items ?? []);
        },
      )
      .catch(console.error)
      .finally(() => {
        if (!cancelled) setLoadingRecent(false);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  const bySource = stats?.by_source as Record<string, number> | undefined;

  return (
    <div className="space-y-8">
      {/* Page header */}
      <header>
        <h1 className="font-serif text-2xl font-semibold text-foreground">수집 현황</h1>
        <p className="mt-1 text-sm text-foreground-muted">
          소스별 문서 수, 모바일 동기화 상태, Whisper 전사 큐
        </p>
      </header>

      {/* Cutover banner */}
      {!loadingStats && <CutoverBanner total={stats?.total} />}

      {/* Source stats grid */}
      <section className="space-y-3">
        <h2 className="text-sm font-semibold text-foreground">소스별 문서 수</h2>
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4">
          {DASHBOARD_SOURCES.map((src) => (
            <SourceCard
              key={src}
              source={src}
              count={loadingStats ? undefined : (bySource?.[src] ?? 0)}
            />
          ))}
        </div>
      </section>

      {/* Whisper queue */}
      <section className="space-y-3">
        <h2 className="text-sm font-semibold text-foreground">Whisper 전사 큐</h2>
        <WhisperQueue
          callLogCount={loadingStats ? undefined : (bySource?.["call-log"] ?? 0)}
          callTranscriptCount={loadingStats ? undefined : (bySource?.["call-transcript"] ?? 0)}
        />
      </section>

      {/* Mobile sync recent items */}
      <section className="space-y-3">
        <h2 className="text-sm font-semibold text-foreground">모바일 동기화 — 최근 수집</h2>
        <div className="grid gap-4 md:grid-cols-3">
          <RecentPanel title="SMS" items={smsRecent} loading={loadingRecent} kind="sms" />
          <RecentPanel
            title="통화 녹음"
            items={callRecent}
            loading={loadingRecent}
            kind="call-recording"
          />
          <RecentPanel
            title="보이스 메모"
            items={voiceRecent}
            loading={loadingRecent}
            kind="voice-memo"
          />
        </div>
      </section>
    </div>
  );
}
