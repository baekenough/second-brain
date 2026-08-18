import type { GraphEntityType, GraphRelType } from "@/lib/types";

/** Korean labels for the closed relation vocabulary (internal/model/relation.go). */
export const REL_TYPE_LABELS: Record<string, string> = {
  communicated_with: "연락",
  requested_of: "요청",
  committed_to: "약속",
  mentions: "언급",
  belongs_to: "소속",
  scheduled_with: "일정",
  about_topic: "주제",
  related_to: "관련",
} satisfies Record<GraphRelType, string>;

/** Korean labels for the four entity types (internal/model/entity.go). */
export const ENTITY_TYPE_LABELS: Record<string, string> = {
  PERSON: "사람",
  ORG: "조직",
  CONCEPT: "개념",
  OTHER: "기타",
} satisfies Record<GraphEntityType, string>;
