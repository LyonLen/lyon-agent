// Workspace layout state: the tile tree, focus, and maximize, plus the
// operations App drives from UI and slash commands. Persistence rides
// localStorage under lyon_layout_v1 — sanitized on read, debounced on
// write (the SCC OpenclawPage pattern). The key is namespaced per session
// (lyon_layout_v1:<sid>) so switching sessions restores each session's own
// pane arrangement; a session with no stored layout inherits whatever is
// on screen as its starting point.

import { useCallback, useEffect, useRef, useState } from "react";
import {
  type Dir,
  type DropZone,
  type PaneKind,
  type TileNode,
  countLeaves,
  findFirstLeaf,
  findLeafByPane,
  getLeaf,
  makeLeaf,
  moveLeaf as moveLeafIn,
  removeLeaf,
  sanitizeNode,
  splitLeaf,
  swapLeaves as swapLeavesIn,
  updateRatio as updateRatioIn,
} from "./types";

const STORAGE_KEY = "lyon_layout_v1";

const storageKey = (sessionId: string) =>
  sessionId ? `${STORAGE_KEY}:${sessionId}` : STORAGE_KEY;

// Default layout ≈ today's three columns as a tree:
// sessions 18% | chat (72% of the rest) | right column: files over plan/steps.
function defaultLayout(): TileNode {
  const steps = makeLeaf("steps");
  const plan = makeLeaf("plan");
  const files = makeLeaf("files");
  const chat = makeLeaf("chat");
  const sessions = makeLeaf("sessions");
  return {
    type: "split",
    nodeId: `t0_default_v`,
    dir: "h",
    ratio: 0.18,
    first: sessions,
    second: {
      type: "split",
      nodeId: `t0_default_mid`,
      dir: "h",
      ratio: 0.72,
      first: chat,
      second: {
        type: "split",
        nodeId: `t0_default_right`,
        dir: "v",
        ratio: 0.45,
        first: files,
        second: {
          type: "split",
          nodeId: `t0_default_br`,
          dir: "v",
          ratio: 0.45,
          first: plan,
          second: steps,
        },
      },
    },
  };
}

function loadStored(key: string): { root: TileNode; focus: string | null } | null {
  try {
    const raw = localStorage.getItem(key);
    if (raw) {
      const parsed = JSON.parse(raw) as { v?: number; root?: unknown; focus?: unknown };
      if (parsed?.v === 1) {
        const root = sanitizeNode(parsed.root);
        if (root && countLeaves(root) > 0) {
          const focus =
            typeof parsed.focus === "string" && getLeaf(root, parsed.focus)
              ? parsed.focus
              : null;
          return { root, focus };
        }
      }
    }
  } catch {
    // corrupt JSON or storage unavailable — treat as unstored
  }
  return null;
}

function saveStored(key: string, root: TileNode, focus: string | null) {
  try {
    localStorage.setItem(key, JSON.stringify({ v: 1, root, focus }));
  } catch {
    // storage full/blocked: layout just won't survive reloads
  }
}

export interface WorkspaceLayout {
  root: TileNode;
  focus: string | null;
  maximized: string | null;
  setFocus: (leafId: string | null) => void;
  setMaximized: (leafId: string | null) => void;
  /** Focus an existing pane of this kind, or open one beside the chat pane. */
  openPane: (kind: PaneKind) => void;
  closeLeaf: (leafId: string) => void;
  /** Split leaf by adding kind on `dir`; focuses the new leaf. Returns its
   *  nodeId (null when the anchor vanished) so callers can seed per-leaf
   *  state (preview tab stacks) immediately. */
  splitLeafAt: (leafId: string, dir: Dir, kind: PaneKind) => string | null;
  updateRatio: (splitId: string, ratio: number) => void;
  swapLeaves: (a: string, b: string) => void;
  moveLeaf: (sourceId: string, targetId: string, zone: DropZone) => void;
  hasPane: (kind: PaneKind) => boolean;
  /** Leaf id currently hosting kind, if any. */
  leafOf: (kind: PaneKind) => string | null;
  resetLayout: () => void;
}

export function useWorkspaceLayout(sessionId: string): WorkspaceLayout {
  const keyRef = useRef(storageKey(sessionId));
  const [{ root, focus }, setState] = useState(
    () => loadStored(keyRef.current) ?? { root: defaultLayout(), focus: null },
  );
  const [maximized, setMaximized] = useState<string | null>(null);
  const rootRef = useRef(root);
  rootRef.current = root;

  // Debounced persist — every ratio nudge during a drag writes once.
  useEffect(() => {
    const t = setTimeout(() => saveStored(keyRef.current, root, focus), 500);
    return () => clearTimeout(t);
  }, [root, focus]);

  // Session switch: flush the outgoing session's layout to its key
  // synchronously (the closure still holds the old tree), then hydrate the
  // new session's key — or, when it has none yet, inherit the on-screen
  // layout and persist it as that session's starting point.
  const prevSid = useRef(sessionId);
  useEffect(() => {
    if (prevSid.current === sessionId) return;
    prevSid.current = sessionId;
    saveStored(keyRef.current, root, focus); // old key, old layout
    keyRef.current = storageKey(sessionId);
    const loaded = loadStored(keyRef.current);
    setMaximized(null);
    if (loaded) setState(loaded);
    else saveStored(keyRef.current, root, focus); // inherit current
  }, [sessionId]); // root/focus intentionally captured at switch time

  const setFocus = useCallback((leafId: string | null) => {
    setState((s) => (s.focus === leafId ? s : { ...s, focus: leafId }));
  }, []);

  const setMax = useCallback((leafId: string | null) => setMaximized(leafId), []);

  const updateRatio = useCallback((splitId: string, ratio: number) => {
    setState((s) => ({ ...s, root: updateRatioIn(s.root, splitId, ratio) }));
  }, []);

  const swapLeaves = useCallback((a: string, b: string) => {
    setState((s) => ({ ...s, root: swapLeavesIn(s.root, a, b) }));
  }, []);

  const moveLeaf = useCallback((sourceId: string, targetId: string, zone: DropZone) => {
    setState((s) => ({ ...s, root: moveLeafIn(s.root, sourceId, targetId, zone) }));
  }, []);

  const splitLeafAt = useCallback((leafId: string, dir: Dir, kind: PaneKind): string | null => {
    if (!getLeaf(rootRef.current, leafId)) return null;
    const leaf = makeLeaf(kind);
    setState((s) => ({
      ...s,
      root: splitLeaf(s.root, leafId, dir, leaf),
      focus: leaf.nodeId,
    }));
    return leaf.nodeId;
  }, []);

  const closeLeaf = useCallback((leafId: string) => {
    setState((s) => {
      if (countLeaves(s.root) <= 1) return s; // never close the last pane
      const root = removeLeaf(s.root, leafId);
      if (!root) return s;
      const focus =
        s.focus === leafId ? findFirstLeaf(root)?.nodeId ?? null : s.focus;
      return { root, focus };
    });
    setMaximized((m) => (m === leafId ? null : m));
  }, []);

  const openPane = useCallback((kind: PaneKind) => {
    const r = rootRef.current;
    const existing = findLeafByPane(r, kind);
    if (existing) {
      setFocus(existing.nodeId);
      return;
    }
    // No instance yet: split beside chat (or the first leaf as fallback) —
    // the new pane lands to the right.
    const anchor = findLeafByPane(r, "chat") ?? findFirstLeaf(r);
    if (!anchor) return;
    const leaf = makeLeaf(kind);
    setState((s) => ({
      ...s,
      root: splitLeaf(s.root, anchor.nodeId, "h", leaf),
      focus: leaf.nodeId,
    }));
  }, [setFocus]);

  const hasPane = useCallback((kind: PaneKind) => findLeafByPane(rootRef.current, kind) !== null, []);
  const leafOf = useCallback((kind: PaneKind) => findLeafByPane(rootRef.current, kind)?.nodeId ?? null, []);

  const resetLayout = useCallback(() => {
    setState({ root: defaultLayout(), focus: null });
  }, []);

  return {
    root,
    focus,
    maximized,
    setFocus,
    setMaximized: setMax,
    openPane,
    closeLeaf,
    splitLeafAt,
    updateRatio,
    swapLeaves,
    moveLeaf,
    hasPane,
    leafOf,
    resetLayout,
  };
}
