"use client";

import { useState, useEffect, useCallback, useRef } from "react";
import {
  listDocuments,
  createNote,
  retryNoteEnrichment,
  deleteNote,
  uploadFile,
  MAX_UPLOAD_FILE_BYTES,
} from "@/lib/api";
import { getNoteMetadata } from "@/lib/types";
import type { DocumentDetail } from "@/lib/types";
import { describeEnrichmentStatus } from "@/lib/enrichmentStatus";
import { formatRelative } from "@/lib/dates";
import { Button } from "@/components/ui";

const NOTES_LIMIT = 20;
const POLL_INTERVAL_MS = 15_000;

const BADGE_CLASS: Record<"success" | "warning" | "danger", string> = {
  success: "bg-[--color-status-success-subtle] text-success",
  warning: "bg-[--color-status-warning-subtle] text-warning",
  danger: "bg-[--color-status-danger-subtle] text-danger",
};

function NoteRow({
  doc,
  onRetry,
  onDelete,
}: {
  doc: DocumentDetail;
  onRetry: (id: string) => void;
  onDelete: (id: string) => void;
}) {
  const meta = getNoteMetadata(doc);
  const badge = describeEnrichmentStatus(meta.enrichment_status);

  return (
    <li className="border-b border-border px-4 py-3 last:border-b-0">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="line-clamp-1 text-sm text-foreground">
            {doc.title || <span className="text-foreground-subtle italic">제목 정리 중…</span>}
          </p>
          <p className="mt-1 line-clamp-2 text-xs text-foreground-muted">{doc.content}</p>
          {badge.retryable && meta.enrichment_last_error && (
            <p className="mt-1 text-xs text-danger">{meta.enrichment_last_error}</p>
          )}
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1.5">
          <span
            className={`rounded px-1.5 py-0.5 text-xs font-medium ${BADGE_CLASS[badge.variant]}`}
          >
            {badge.label}
          </span>
          <div className="flex gap-2">
            {badge.retryable && (
              <button
                type="button"
                onClick={() => onRetry(doc.id)}
                className="text-xs text-accent hover:underline"
              >
                재시도
              </button>
            )}
            <button
              type="button"
              onClick={() => onDelete(doc.id)}
              className="text-xs text-foreground-subtle hover:text-danger hover:underline"
            >
              삭제
            </button>
          </div>
        </div>
      </div>
      <p className="mt-1.5 text-xs text-foreground-subtle">{formatRelative(doc.collected_at)}</p>
    </li>
  );
}

export default function CapturePage() {
  const [content, setContent] = useState("");
  const [notes, setNotes] = useState<DocumentDetail[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const refresh = useCallback(async () => {
    try {
      const docs = await listDocuments({ source: "note", limit: NOTES_LIMIT });
      setNotes(docs);
    } catch {
      // A background refresh failing must not disrupt the compose field —
      // the user's in-progress typing is more important than a stale list.
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Poll only while at least one note is not yet "done" — the enrichment
  // worker ticks on its own schedule (backend plan: 5-minute interval), so
  // there is no push channel; short client polling is the simplest way to
  // reflect status changes without the user manually reloading.
  useEffect(() => {
    const hasPending = notes.some((n) => getNoteMetadata(n).enrichment_status !== "done");
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
    if (hasPending) {
      pollRef.current = setInterval(() => void refresh(), POLL_INTERVAL_MS);
    }
    return () => {
      if (pollRef.current) clearInterval(pollRef.current);
    };
  }, [notes, refresh]);

  async function handleSave() {
    const trimmed = content.trim();
    if (!trimmed || saving) return;
    setSaving(true);
    setError(null);
    try {
      const result = await createNote(trimmed);
      setContent("");
      const now = new Date().toISOString();
      // Optimistic prepend — title/enrichment status are filled in
      // asynchronously by the worker (spec §6.1); an empty title here is
      // correct, not a bug (see the "제목 정리 중…" placeholder above).
      setNotes((prev) => [
        {
          id: result.id,
          title: "",
          content: trimmed,
          source_type: "note",
          source_id: result.id,
          status: "active",
          collected_at: now,
          created_at: now,
          updated_at: now,
          metadata: { enrichment_status: "pending", enrichment_attempts: 0 },
        },
        ...prev,
      ]);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "저장 중 오류가 발생했습니다.");
    } finally {
      setSaving(false);
    }
  }

  async function handleFileSelected(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    // Always clear the input value so selecting the same file twice in a
    // row (e.g. retry after an error) still fires a change event.
    e.target.value = "";
    if (!file || uploading) return;

    if (file.size > MAX_UPLOAD_FILE_BYTES) {
      setError(
        `파일이 너무 큽니다 (최대 ${Math.floor(MAX_UPLOAD_FILE_BYTES / (1024 * 1024))}MB).`,
      );
      return;
    }

    setUploading(true);
    setError(null);
    try {
      await uploadFile(file);
      await refresh();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "파일 업로드 중 오류가 발생했습니다.");
    } finally {
      setUploading(false);
    }
  }

  async function handleRetry(id: string) {
    try {
      await retryNoteEnrichment(id);
      await refresh();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "재시도 요청이 실패했습니다.");
    }
  }

  async function handleDelete(id: string) {
    const prev = notes;
    setNotes((cur) => cur.filter((n) => n.id !== id)); // optimistic
    try {
      await deleteNote(id);
    } catch (e: unknown) {
      setNotes(prev); // roll back on failure
      setError(e instanceof Error ? e.message : "삭제가 실패했습니다.");
    }
  }

  return (
    <div className="space-y-6">
      {/* Compose — single field + large save button on every breakpoint
          (spec §7.3): the desktop and mobile requirements converge here,
          so this section does not need responsive variants the way Ask's
          input bar does (Task 9). */}
      <div className="space-y-2">
        <textarea
          value={content}
          onChange={(e) => setContent(e.target.value)}
          placeholder="무슨 생각이 드셨나요?"
          rows={4}
          disabled={saving}
          className="w-full resize-none rounded-lg border border-border bg-surface px-3 py-2.5 text-sm text-foreground placeholder:text-foreground-subtle focus:ring-2 focus:ring-accent/40 focus:outline-none disabled:opacity-50"
        />
        <Button
          type="button"
          variant="primary"
          size="lg"
          onClick={() => void handleSave()}
          loading={saving}
          disabled={saving || !content.trim()}
          className="w-full sm:w-auto"
        >
          저장
        </Button>
        {error && <p className="text-sm text-danger">{error}</p>}
      </div>

      {/* File upload — separate from the text compose field above; the
          backend ingests the file as its own document (source: upload). */}
      <div className="space-y-2 rounded-lg border border-border bg-surface p-4">
        <h2 className="text-sm font-medium text-foreground">파일로 저장</h2>
        <p className="text-xs text-foreground-muted">
          PDF, DOCX, XLSX, PPTX, HWPX, HTML, TXT, MD 파일을 업로드할 수 있습니다.
        </p>
        <div className="flex items-center gap-2">
          <input
            ref={fileInputRef}
            type="file"
            accept=".pdf,.docx,.xlsx,.pptx,.hwpx,.html,.htm,.txt,.md,.text"
            onChange={(e) => void handleFileSelected(e)}
            disabled={uploading}
            className="hidden"
          />
          <Button
            type="button"
            variant="secondary"
            onClick={() => fileInputRef.current?.click()}
            loading={uploading}
            disabled={uploading}
          >
            파일 선택 및 업로드
          </Button>
        </div>
      </div>

      {/* Recent notes */}
      <div className="rounded-lg border border-border bg-surface">
        <div className="border-b border-border px-4 py-3">
          <h2 className="text-sm font-medium text-foreground">최근 노트</h2>
        </div>
        {notes.length === 0 ? (
          <p className="px-4 py-8 text-center text-sm text-foreground-subtle">
            아직 노트가 없습니다
          </p>
        ) : (
          <ul>
            {notes.map((doc) => (
              <NoteRow key={doc.id} doc={doc} onRetry={handleRetry} onDelete={handleDelete} />
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
