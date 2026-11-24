// Mindmap node JSON — the §3 wire shape (pkg/mindmap Node's json tags,
// the same array that rides EVENT_MINDMAP_UPDATE's attrs["nodes"] and
// GetMindmapResponse.nodes_json). Keys are the wire names: sample.js's
// shorthand (parent/turn) is the static demo only, never the RPC.

export type MapNode = {
  id: string;
  parent_id: string;
  label: string;
  source_turn: number;
  kind: string; // branch | decision | fact | open_question
  status: string; // active | parked
  pinned: boolean;
  edited_by_user: boolean;
  tombstone: boolean;
  state: string; // proposed | confirmed
  // M3 cross-branch link (§9): absent on the pure-tree v1 map; when a
  // `relate` op sets it the card draws a dashed edge to the target.
  relation?: { type: string; target: string } | null;
  // Event-diagram placement (§14): absolute canvas px + shape. Absent on
  // pure-tree nodes — the card falls back to the auto layout.
  shape?: string;
  x?: number;
  y?: number;
  w?: number;
  h?: number;
};

// One event-diagram arrow (§14) — {id, from, to, label, style} on the
// wire (edges_json / EVENT attrs["edges"]); style solid|dashed.
export type MapEdge = {
  id: string;
  from: string;
  to: string;
  label?: string;
  style?: string;
};

// One card snapshot — what GetMindmap answers and what every
// mindmap_update event carries (mindmap_edit carries nodes + revision
// only; title/subtitle ride the update events and the RPC).
export type MindmapSnap = {
  revision: number;
  title: string;
  subtitle: string;
  nodes: MapNode[];
  // Event-diagram arrows (§14); [] on a pure tree. Absent on legacy
  // payloads keeps the previous edges (merge semantics, like title).
  edges: MapEdge[];
};

// parseNodes turns a nodes_json payload into rows. Malformed JSON is a
// soft miss (empty list) — the card degrades to hidden, never to a
// stream error (same contract as files_changed's attrs parse).
export function parseNodes(json: string | undefined): MapNode[] {
  try {
    const raw: unknown = JSON.parse(json || "[]");
    return Array.isArray(raw) ? (raw as MapNode[]) : [];
  } catch {
    return [];
  }
}

// parseEdges is parseNodes' twin for edges_json — malformed JSON is a
// soft miss (empty list), never a stream error.
export function parseEdges(json: string | undefined): MapEdge[] {
  try {
    const raw: unknown = JSON.parse(json || "[]");
    return Array.isArray(raw) ? (raw as MapEdge[]) : [];
  } catch {
    return [];
  }
}
