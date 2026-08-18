"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import ForceGraph2D from "react-force-graph-2d";
import { displayName } from "./mask";
import { REL_TYPE_LABELS } from "./labels";

// Client-only canvas wrapper. `react-force-graph-2d` touches `window` and
// `canvas` at import time, so the page must pull this in through
// next/dynamic({ ssr: false }) — that also keeps the library out of the
// initial bundle.

export interface CanvasNode {
  id: number;
  name: string;
  type: string;
  degree: number;
  /** Entry points are drawn slightly larger than nodes pulled in by expansion. */
  seed: boolean;
}

export interface CanvasLink {
  /** Stable identity: one Postgres relation row = one edge. */
  key: string;
  source: number;
  target: number;
  relType: string;
  weight: number;
}

export interface GraphCanvasProps {
  nodes: CanvasNode[];
  links: CanvasLink[];
  onNodeClick: (node: CanvasNode) => void;
  onLinkClick: (link: CanvasLink) => void;
  maskNames: boolean;
  /** Currently focused entity, drawn with a ring. */
  selectedId: number | null;
}

// Tinted, non-primary hues so the four entity types stay distinguishable
// without reading as a traffic-light palette.
const TYPE_COLOR: Record<string, string> = {
  PERSON: "#3f6ee0",
  ORG: "#0f9488",
  CONCEPT: "#b4690e",
  OTHER: "#7a7f8c",
};

const FALLBACK_COLOR = "#7a7f8c";

function colorFor(type: string): string {
  return TYPE_COLOR[type] ?? FALLBACK_COLOR;
}

export default function GraphCanvas({
  nodes,
  links,
  onNodeClick,
  onLinkClick,
  maskNames,
  selectedId,
}: GraphCanvasProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState({ width: 0, height: 480 });

  // The force layout needs pixel dimensions; the container is fluid, so track
  // it rather than hardcoding a width.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const update = () => setSize({ width: el.clientWidth, height: el.clientHeight });
    update();
    const observer = new ResizeObserver(update);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // force-graph mutates the objects it is given (adding x/y/vx/vy), so hand it
  // copies keyed off the incoming props.
  const graphData = useMemo(
    () => ({
      nodes: nodes.map((n) => ({ ...n })),
      links: links.map((l) => ({ ...l })),
    }),
    [nodes, links],
  );

  const paintNode = useCallback(
    (
      node: CanvasNode & { x?: number; y?: number },
      ctx: CanvasRenderingContext2D,
      scale: number,
    ) => {
      const { x = 0, y = 0 } = node;
      const radius = node.seed ? 6 : 4;

      ctx.beginPath();
      ctx.arc(x, y, radius, 0, 2 * Math.PI);
      ctx.fillStyle = colorFor(node.type);
      ctx.fill();

      if (node.id === selectedId) {
        ctx.strokeStyle = "#111827";
        ctx.lineWidth = 1.5 / scale;
        ctx.stroke();
      }

      // Labels are the point of this screen, but they turn into noise when
      // zoomed far out — drop them below a readable scale.
      if (scale < 0.7) return;
      const label = displayName(node.name, maskNames);
      const fontSize = Math.max(10 / scale, 2);
      ctx.font = `${fontSize}px sans-serif`;
      ctx.textAlign = "center";
      ctx.textBaseline = "top";
      ctx.fillStyle = "#374151";
      ctx.fillText(label, x, y + radius + 1);
    },
    [maskNames, selectedId],
  );

  // Widen edges by observation count, log-scaled: a pair seen 40 times should
  // read as heavier than one seen twice without swamping the canvas.
  const linkWidth = useCallback(
    (link: CanvasLink) => 1 + Math.log2(Math.max(link.weight, 1)) / 2,
    [],
  );

  return (
    <div
      ref={containerRef}
      className="h-[480px] w-full overflow-hidden rounded-lg border border-border bg-surface"
      role="application"
      aria-label="지식 그래프 캔버스. 노드를 클릭하면 1홉 확장, 링크를 클릭하면 근거 목록이 열립니다."
    >
      {size.width > 0 && (
        <ForceGraph2D<CanvasNode, CanvasLink>
          graphData={graphData}
          width={size.width}
          height={size.height}
          backgroundColor="transparent"
          nodeId="id"
          nodeRelSize={5}
          nodeLabel={(n) => displayName(n.name, maskNames)}
          nodeCanvasObject={paintNode}
          nodePointerAreaPaint={(node, color, ctx) => {
            const { x = 0, y = 0 } = node;
            ctx.fillStyle = color;
            ctx.beginPath();
            ctx.arc(x, y, 8, 0, 2 * Math.PI);
            ctx.fill();
          }}
          linkLabel={(l) => REL_TYPE_LABELS[l.relType] ?? l.relType}
          linkColor={() => "#cbd5e1"}
          linkWidth={linkWidth}
          linkDirectionalArrowLength={4}
          linkDirectionalArrowRelPos={1}
          cooldownTicks={120}
          onNodeClick={(n) => onNodeClick(n as CanvasNode)}
          onLinkClick={(l) => onLinkClick(l as unknown as CanvasLink)}
        />
      )}
    </div>
  );
}
