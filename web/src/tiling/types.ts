// Tile tree model for the workspace (ported from a sibling product's
// TilingWorkspace, generalized: a leaf names a pane *kind* — instances are
// the leaves themselves, so the same kind may appear twice). Pure functions
// only; React state lives in useWorkspaceLayout.

export type PaneKind =
  | "sessions"
  | "chat"
  | "files"
  | "steps"
  | "plan"
  | "preview"
  | "terminal"
  | "memory"
  | "settings";

export const PANE_KINDS: readonly PaneKind[] = [
  "sessions",
  "chat",
  "files",
  "steps",
  "plan",
  "preview",
  "terminal",
  "memory",
  "settings",
];

export function isPaneKind(v: unknown): v is PaneKind {
  return typeof v === "string" && (PANE_KINDS as readonly string[]).includes(v);
}

export type Dir = "h" | "v";
export type DropZone = "top" | "bottom" | "left" | "right";

export interface TileLeaf {
  type: "leaf";
  nodeId: string;
  pane: PaneKind;
}

export interface TileSplit {
  type: "split";
  nodeId: string;
  dir: Dir;
  ratio: number; // first child's share along dir, clamped [0.1, 0.9]
  first: TileNode;
  second: TileNode;
}

export type TileNode = TileLeaf | TileSplit;

let _seq = 0;
export function nid(): string {
  return `t${++_seq}_${Math.random().toString(36).slice(2, 6)}`;
}

export function makeLeaf(pane: PaneKind): TileLeaf {
  return { type: "leaf", nodeId: nid(), pane };
}

/** Replace leaf `leafId` with a split hosting it plus `newLeaf`. */
export function splitLeaf(
  root: TileNode,
  leafId: string,
  dir: Dir,
  newLeaf: TileLeaf,
): TileNode {
  if (root.type === "leaf") {
    if (root.nodeId !== leafId) return root;
    return {
      type: "split",
      nodeId: nid(),
      dir,
      ratio: 0.5,
      first: root, // original leaf preserved (same nodeId → no remount)
      second: newLeaf,
    };
  }
  return {
    ...root,
    first: splitLeaf(root.first, leafId, dir, newLeaf),
    second: splitLeaf(root.second, leafId, dir, newLeaf),
  };
}

/** Drop a leaf; the parent split collapses to the surviving side. */
export function removeLeaf(root: TileNode, leafId: string): TileNode | null {
  if (root.type === "leaf") return root.nodeId === leafId ? null : root;
  const f = removeLeaf(root.first, leafId);
  const s = removeLeaf(root.second, leafId);
  if (f === null) return s;
  if (s === null) return f;
  return { ...root, first: f, second: s };
}

export function updateRatio(
  root: TileNode,
  splitId: string,
  ratio: number,
): TileNode {
  if (root.type === "leaf") return root;
  if (root.nodeId === splitId) {
    return { ...root, ratio: Math.max(0.1, Math.min(0.9, ratio)) };
  }
  return {
    ...root,
    first: updateRatio(root.first, splitId, ratio),
    second: updateRatio(root.second, splitId, ratio),
  };
}

export function findFirstLeaf(node: TileNode): TileLeaf | null {
  if (node.type === "leaf") return node;
  return findFirstLeaf(node.first) ?? findFirstLeaf(node.second);
}

export function getLeaf(root: TileNode, leafId: string): TileLeaf | null {
  if (root.type === "leaf") return root.nodeId === leafId ? root : null;
  return getLeaf(root.first, leafId) ?? getLeaf(root.second, leafId);
}

/** First leaf hosting `pane`, or null. */
export function findLeafByPane(root: TileNode, pane: PaneKind): TileLeaf | null {
  if (root.type === "leaf") return root.pane === pane ? root : null;
  return findLeafByPane(root.first, pane) ?? findLeafByPane(root.second, pane);
}

/** The deepest ancestor split of `leafId` with the given orientation. */
export function findParentSplit(
  root: TileNode,
  leafId: string,
  dir: Dir,
): { splitId: string; ratio: number; isFirst: boolean } | null {
  if (root.type === "leaf") return null;
  const inFirst = getLeaf(root.first, leafId) !== null;
  if (!inFirst && getLeaf(root.second, leafId) === null) return null;
  const deeper = findParentSplit(inFirst ? root.first : root.second, leafId, dir);
  if (deeper) return deeper; // nearest matching-dir ancestor wins
  if (root.dir === dir) return { splitId: root.nodeId, ratio: root.ratio, isFirst: inFirst };
  return null;
}

export function countLeaves(node: TileNode): number {
  if (node.type === "leaf") return 1;
  return countLeaves(node.first) + countLeaves(node.second);
}

/** All leaves in tree order (first before second, depth-first). */
export function collectLeaves(node: TileNode): TileLeaf[] {
  if (node.type === "leaf") return [node];
  return [...collectLeaves(node.first), ...collectLeaves(node.second)];
}

/**
 * Swap two leaves in place — leaf identity (nodeId) is preserved so React
 * keys stay stable and panel state survives the move.
 */
export function swapLeaves(root: TileNode, idA: string, idB: string): TileNode {
  if (idA === idB) return root;
  const a = getLeaf(root, idA);
  const b = getLeaf(root, idB);
  if (!a || !b) return root;
  const map = (n: TileNode): TileNode => {
    if (n.type === "leaf") {
      if (n.nodeId === idA) return b;
      if (n.nodeId === idB) return a;
      return n;
    }
    return { ...n, first: map(n.first), second: map(n.second) };
  };
  return map(root);
}

/** Detach `sourceId` and re-insert it as a sibling of `targetId` on `position`. */
export function moveLeaf(
  root: TileNode,
  sourceId: string,
  targetId: string,
  position: DropZone,
): TileNode {
  if (sourceId === targetId) return root;
  const source = getLeaf(root, sourceId);
  if (!source) return root;
  const removed = removeLeaf(root, sourceId);
  if (!removed) return root; // would empty the tree — refuse
  const dir: Dir = position === "left" || position === "right" ? "h" : "v";
  const sourceFirst = position === "left" || position === "top";
  const insert = (n: TileNode): TileNode => {
    if (n.type === "leaf") {
      if (n.nodeId !== targetId) return n;
      return {
        type: "split",
        nodeId: nid(),
        dir,
        ratio: 0.5,
        first: sourceFirst ? source : n,
        second: sourceFirst ? n : source,
      };
    }
    return { ...n, first: insert(n.first), second: insert(n.second) };
  };
  return insert(removed);
}

// ── Persistence sanitizing ────────────────────────────────────────────────────

const MAX_DEPTH = 40; // a corrupt/hostile blob must not blow the stack

/**
 * Rebuild a tree from parsed JSON: unknown pane kinds and malformed nodes
 * drop out (a split with one surviving side collapses), ratios clamp to
 * [0.1, 0.9], and anything deeper than MAX_DEPTH fails whole — the caller
 * falls back to the default layout. Returns null when nothing usable is left.
 */
export function sanitizeNode(raw: unknown, depth = 0): TileNode | null {
  if (depth > MAX_DEPTH) return null;
  if (typeof raw !== "object" || raw === null) return null;
  const n = raw as Record<string, unknown>;
  if (n.type === "leaf") {
    if (typeof n.nodeId !== "string" || !isPaneKind(n.pane)) return null;
    return { type: "leaf", nodeId: n.nodeId, pane: n.pane };
  }
  if (n.type === "split") {
    if (typeof n.nodeId !== "string") return null;
    if (n.dir !== "h" && n.dir !== "v") return null;
    const first = sanitizeNode(n.first, depth + 1);
    const second = sanitizeNode(n.second, depth + 1);
    if (!first) return second;
    if (!second) return first;
    const ratio =
      typeof n.ratio === "number" && Number.isFinite(n.ratio)
        ? Math.max(0.1, Math.min(0.9, n.ratio))
        : 0.5;
    return {
      type: "split",
      nodeId: n.nodeId,
      dir: n.dir,
      ratio,
      first,
      second,
    };
  }
  return null;
}
