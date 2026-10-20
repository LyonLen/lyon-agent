// Normalized geometry for the tile tree: every rect is a fraction of the
// workspace [0,1], rendered as percentages. Ported from stereo-claw-cloud's
// TilingWorkspace.computeGeometry.

import type { TileNode } from "./types";

export interface LeafRect {
  pane: string;
  nodeId: string;
  left: number;
  top: number;
  width: number;
  height: number;
}

export interface HandleRect {
  splitId: string;
  dir: "h" | "v";
  pos: number; // split position on its axis (fraction)
  crossStart: number;
  crossEnd: number;
  axisStart: number;
  axisSize: number;
}

export interface Geometry {
  leaves: LeafRect[];
  handles: HandleRect[];
}

export function computeGeometry(
  node: TileNode,
  x: number,
  y: number,
  w: number,
  h: number,
): Geometry {
  if (node.type === "leaf") {
    return {
      leaves: [{ pane: node.pane, nodeId: node.nodeId, left: x, top: y, width: w, height: h }],
      handles: [],
    };
  }
  const { dir, ratio } = node;
  if (dir === "h") {
    const fw = w * ratio;
    const first = computeGeometry(node.first, x, y, fw, h);
    const second = computeGeometry(node.second, x + fw, y, w - fw, h);
    return {
      leaves: [...first.leaves, ...second.leaves],
      handles: [
        {
          splitId: node.nodeId,
          dir: "h",
          pos: x + fw,
          crossStart: y,
          crossEnd: y + h,
          axisStart: x,
          axisSize: w,
        },
        ...first.handles,
        ...second.handles,
      ],
    };
  }
  const fh = h * ratio;
  const first = computeGeometry(node.first, x, y, w, fh);
  const second = computeGeometry(node.second, x, y + fh, w, h - fh);
  return {
    leaves: [...first.leaves, ...second.leaves],
    handles: [
      {
        splitId: node.nodeId,
        dir: "v",
        pos: y + fh,
        crossStart: x,
        crossEnd: x + w,
        axisStart: y,
        axisSize: h,
      },
      ...first.handles,
      ...second.handles,
    ],
  };
}
