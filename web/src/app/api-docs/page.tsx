import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "API — Second Brain",
};

interface EndpointParam {
  name: string;
  type: string;
  required: boolean;
  desc: string;
}

interface Endpoint {
  method: "GET" | "POST";
  path: string;
  summary: string;
  description?: string;
  params?: EndpointParam[];
  body?: string;
  example?: string;
  response?: string;
}

const ENDPOINTS: Endpoint[] = [
  {
    method: "GET",
    path: "/health",
    summary: "헬스체크",
    example: `curl http://localhost:9200/health`,
    response: `{"status":"ok"}`,
  },
  {
    method: "POST",
    path: "/api/v1/search",
    summary: "하이브리드 검색 (fulltext + vector, RRF 결합)",
    description:
      "본문 검색과 벡터 유사도를 RRF(Reciprocal Rank Fusion)로 결합합니다. source_type으로 필터링, sort로 정렬 방식 지정.",
    body: `{
  "query": "BBQ 미팅",
  "source_type": "filesystem",
  "limit": 10,
  "sort": "relevance",
  "include_deleted": false
}`,
    example: `curl -X POST http://localhost:9200/api/v1/search \\
  -H "Content-Type: application/json" \\
  -d '{"query":"BBQ","limit":10,"sort":"relevance"}'`,
    response: `{
  "results": [{ "id": "...", "title": "...", "content": "...", "score": 0.83, ... }],
  "count": 10,
  "total": 10,
  "query": "BBQ",
  "took_ms": 42
}`,
  },
  {
    method: "GET",
    path: "/api/v1/documents",
    summary: "최근 문서 목록 (collected_at DESC)",
    params: [
      { name: "limit", type: "int", required: false, desc: "기본 20, 최대 100" },
      { name: "offset", type: "int", required: false, desc: "기본 0" },
      {
        name: "source",
        type: "string",
        required: false,
        desc: "filesystem | slack | github",
      },
    ],
    example: `curl 'http://localhost:9200/api/v1/documents?limit=10&source=filesystem'`,
    response: `{"documents": [{ "id": "...", "title": "...", ... }]}`,
  },
  {
    method: "GET",
    path: "/api/v1/documents/{id}",
    summary: "문서 상세 (JSON)",
    example: `curl http://localhost:9200/api/v1/documents/d8a3a613-...`,
    response: `{ "id": "...", "title": "...", "content": "...", "metadata": {...} }`,
  },
  {
    method: "GET",
    path: "/api/v1/documents/{id}/raw",
    summary: "원본 파일 바이트 스트리밍",
    description:
      "filesystem source_type 문서만 지원. Content-Type은 파일 확장자에 따라 자동 결정(text/markdown, image/png, application/pdf 등). 50 MiB 초과 파일은 413.",
    example: `curl -OJ http://localhost:9200/api/v1/documents/d8a3a613-.../raw`,
  },
  {
    method: "POST",
    path: "/api/v1/collect/trigger",
    summary: "수집 파이프라인 수동 트리거",
    description:
      "등록된 모든 collector를 순차 실행. scheduler mutex(atomic.Bool)로 동시 실행 방지 — 이미 실행 중이면 skip.",
    example: `curl -X POST http://localhost:9200/api/v1/collect/trigger`,
    response: `{"status":"collection triggered"}`,
  },
  {
    method: "GET",
    path: "/api/v1/sources",
    summary: "등록된 collector 목록",
    example: `curl http://localhost:9200/api/v1/sources`,
  },
];

function EndpointCard({ endpoint: ep }: { endpoint: Endpoint }) {
  return (
    <section className="border border-gray-200 dark:border-gray-800 rounded-lg p-4 space-y-3">
      <div className="flex items-baseline gap-2 flex-wrap">
        <span
          className={`inline-block px-2 py-0.5 rounded text-xs font-mono font-semibold ${
            ep.method === "GET"
              ? "bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300"
              : "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300"
          }`}
        >
          {ep.method}
        </span>
        <code className="text-sm font-mono text-gray-900 dark:text-gray-100">
          {ep.path}
        </code>
      </div>

      <p className="text-sm text-gray-700 dark:text-gray-300">{ep.summary}</p>

      {ep.description && (
        <p className="text-xs text-gray-500 dark:text-gray-400 leading-relaxed">
          {ep.description}
        </p>
      )}

      {ep.params && ep.params.length > 0 && (
        <div>
          <h4 className="text-xs font-semibold text-gray-500 dark:text-gray-400 mb-1">
            쿼리 파라미터
          </h4>
          <ul className="text-xs text-gray-600 dark:text-gray-400 space-y-0.5">
            {ep.params.map((p) => (
              <li key={p.name}>
                <code className="font-mono">{p.name}</code>
                <span className="text-gray-400">
                  {" "}
                  ({p.type}
                  {p.required ? ", required" : ""})
                </span>
                {" — "}
                {p.desc}
              </li>
            ))}
          </ul>
        </div>
      )}

      {ep.body && (
        <div>
          <h4 className="text-xs font-semibold text-gray-500 dark:text-gray-400 mb-1">
            요청 본문
          </h4>
          <pre className="text-xs font-mono bg-gray-50 dark:bg-gray-900 border border-gray-200 dark:border-gray-800 rounded p-2 overflow-x-auto">
            {ep.body}
          </pre>
        </div>
      )}

      {ep.example && (
        <div>
          <h4 className="text-xs font-semibold text-gray-500 dark:text-gray-400 mb-1">
            예시
          </h4>
          <pre className="text-xs font-mono bg-gray-50 dark:bg-gray-900 border border-gray-200 dark:border-gray-800 rounded p-2 overflow-x-auto">
            {ep.example}
          </pre>
        </div>
      )}

      {ep.response && (
        <div>
          <h4 className="text-xs font-semibold text-gray-500 dark:text-gray-400 mb-1">
            응답 예시
          </h4>
          <pre className="text-xs font-mono bg-gray-50 dark:bg-gray-900 border border-gray-200 dark:border-gray-800 rounded p-2 overflow-x-auto">
            {ep.response}
          </pre>
        </div>
      )}
    </section>
  );
}

export default function ApiDocsPage() {
  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <h1 className="text-2xl font-semibold text-gray-900 dark:text-gray-100">
          API 레퍼런스
        </h1>
        <p className="text-sm text-gray-500 dark:text-gray-400">
          second-brain 백엔드가 노출하는 REST 엔드포인트입니다. 모든 응답은
          JSON이며 기본 포트는{" "}
          <code className="px-1 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-xs">
            :9200
          </code>{" "}
          입니다. 프론트엔드는 동일 출처의{" "}
          <code className="px-1 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-xs">
            /api
          </code>{" "}
          프록시를 통해 호출합니다.
        </p>
      </div>

      {ENDPOINTS.map((ep) => (
        <EndpointCard key={`${ep.method}-${ep.path}`} endpoint={ep} />
      ))}
    </div>
  );
}
