// Per-leaf file-tab stacks for preview panes (the sibling page
// Record<leafId, {files, active}> pattern). Keyed by tile nodeId so a
// dragged/swapped pane keeps its tabs; entries for leaves that left the
// tree are pruned when the layout changes, and the record persists under
// lyon_files_leaves_v1 (500ms debounce, like the layout itself). The key
// is namespaced per session — each session restores its own open tabs;
// a session with no stored stacks inherits the current ones.

import { useCallback, useEffect, useRef, useState } from "react";
import type { FileKind } from "./gen/lyon/v1/file_pb";
import { type TileNode, getLeaf } from "./tiling/types";

const STORAGE_KEY = "lyon_files_leaves_v1";

const storageKey = (sessionId: string) =>
  sessionId ? `${STORAGE_KEY}:${sessionId}` : STORAGE_KEY;

export interface PVFile {
  path: string;
  // DirEntry kind at click time — survives reloads so an image tab still
  // previews as media after a refresh (fetchPreview without it would
  // sniff text).
  kind?: FileKind;
}

export interface LeafTabs {
  files: PVFile[];
  active: number;
  lastActive: number; // monotonic "most recently focused" clock
}

export type FileTabsState = Record<string, LeafTabs>;

function load(key: string): FileTabsState | null {
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as { v?: number; leaves?: unknown };
    if (parsed?.v !== 1 || typeof parsed.leaves !== "object" || !parsed.leaves) return null;
    const out: FileTabsState = {};
    for (const [id, rec] of Object.entries(parsed.leaves as Record<string, unknown>)) {
      const r = rec as { files?: unknown; active?: unknown };
      if (!Array.isArray(r.files) || r.files.length === 0) continue;
      const files: PVFile[] = [];
      for (const f of r.files) {
        const fv = f as { path?: unknown; kind?: unknown };
        if (typeof fv.path !== "string" || fv.path === "") continue;
        files.push({
          path: fv.path,
          kind: typeof fv.kind === "number" ? (fv.kind as FileKind) : undefined,
        });
      }
      if (files.length === 0) continue;
      const active =
        typeof r.active === "number" && r.active >= 0 && r.active < files.length
          ? r.active
          : 0;
      out[id] = { files, active, lastActive: 0 };
    }
    return out;
  } catch {
    return null;
  }
}

function save(key: string, tabs: FileTabsState) {
  try {
    const leaves: FileTabsState = {};
    for (const [id, rec] of Object.entries(tabs)) {
      leaves[id] = { files: rec.files, active: rec.active, lastActive: 0 };
    }
    localStorage.setItem(key, JSON.stringify({ v: 1, leaves }));
  } catch {
    // storage blocked: stacks just won't survive reloads
  }
}

function prune(tabs: FileTabsState, root: TileNode): FileTabsState {
  let changed = false;
  const out: FileTabsState = {};
  for (const [id, rec] of Object.entries(tabs)) {
    // Only preview leaves keep stacks; a closed/swapped-away pane drops
    // its record on the next tree change.
    if (getLeaf(root, id)?.pane === "preview") out[id] = rec;
    else changed = true;
  }
  return changed ? out : tabs;
}

export interface FileTabsApi {
  tabs: FileTabsState;
  /** Push (or re-focus, if already open) a file in this leaf's stack. */
  open: (leafId: string, file: PVFile) => void;
  activate: (leafId: string, index: number) => void;
  closePath: (leafId: string, index: number) => void;
}

export function useFileTabs(root: TileNode, sessionId: string): FileTabsApi {
  const keyRef = useRef(storageKey(sessionId));
  const [tabs, setTabs] = useState<FileTabsState>(() => load(keyRef.current) ?? {});
  const clock = useRef(0);

  // The tree changed: drop stacks whose leaf is gone (or no longer a
  // preview). Identity-preserving changes (resize/drag) keep every entry.
  useEffect(() => {
    setTabs((t) => prune(t, root));
  }, [root]);

  useEffect(() => {
    const t = setTimeout(() => save(keyRef.current, tabs), 500);
    return () => clearTimeout(t);
  }, [tabs]);

  // Session switch: flush outgoing stacks, then hydrate the new session's
  // stacks — or inherit the on-screen ones when it has none yet.
  const prevSid = useRef(sessionId);
  useEffect(() => {
    if (prevSid.current === sessionId) return;
    prevSid.current = sessionId;
    save(keyRef.current, tabs); // old key, old stacks
    keyRef.current = storageKey(sessionId);
    const loaded = load(keyRef.current);
    if (loaded) setTabs(loaded);
    else save(keyRef.current, tabs); // inherit current
  }, [sessionId]); // tabs intentionally captured at switch time

  const open = useCallback((leafId: string, file: PVFile) => {
    setTabs((t) => {
      const cur = t[leafId] ?? { files: [], active: 0, lastActive: 0 };
      const idx = cur.files.findIndex((f) => f.path === file.path);
      const files = idx >= 0 ? cur.files : [...cur.files, file];
      return {
        ...t,
        [leafId]: {
          files,
          active: idx >= 0 ? idx : files.length - 1,
          lastActive: ++clock.current,
        },
      };
    });
  }, []);

  const activate = useCallback((leafId: string, index: number) => {
    setTabs((t) => {
      const cur = t[leafId];
      if (!cur || index < 0 || index >= cur.files.length) return t;
      return {
        ...t,
        [leafId]: { ...cur, active: index, lastActive: ++clock.current },
      };
    });
  }, []);

  const closePath = useCallback((leafId: string, index: number) => {
    setTabs((t) => {
      const cur = t[leafId];
      if (!cur || index < 0 || index >= cur.files.length) return t;
      const files = cur.files.filter((_, j) => j !== index);
      const active =
        cur.active > index ? cur.active - 1 : Math.min(cur.active, files.length - 1);
      return {
        ...t,
        [leafId]: { ...cur, files, active: Math.max(0, active) },
      };
    });
  }, []);

  return { tabs, open, activate, closePath };
}
