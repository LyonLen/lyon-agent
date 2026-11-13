// Per-session persistence for the composer's durable bits.
//
// App keys ChatView by session (key={selectedId}), so every session
// switch unmounts the pane and a Fast Refresh can reset it mid-flight.
// Three things must survive that:
//
//   queued — the parked-prompt mirror. The server exposes only QueueLen
//            (the text is client-side by design — ChatView's setQueued
//            comment), and kickQueued re-sends from this list at every
//            prompt_end. Losing it strands lines the backend already
//            accepted (Image #12: "后台已经收了，但是前端渲染看不到").
//   hist   — the ↑ recall list of prompts the human typed.
//   draft  — half-typed input, so a switch and back doesn't eat it.
//
// sessionStorage (per tab) covers switch, reload, and HMR resets with
// writes that cost nothing; a quota/private-mode failure degrades to the
// old in-memory behavior instead of throwing. Cross-tab mirror sync and
// server-side queue text need an RPC — see BACKLOG (queued 暴露).

export type ComposerSnap = {
  queued: string[];
  hist: string[];
  draft: string;
};

const keyOf = (sid: string) => `lyon-composer:${sid}`;

const EMPTY: ComposerSnap = { queued: [], hist: [], draft: "" };

export function loadComposer(sid: string): ComposerSnap {
  if (!sid) return { ...EMPTY };
  try {
    const raw = sessionStorage.getItem(keyOf(sid));
    if (!raw) return { ...EMPTY };
    const v = JSON.parse(raw) as Partial<ComposerSnap>;
    const strArr = (a: unknown): string[] =>
      Array.isArray(a) ? a.filter((s): s is string => typeof s === "string") : [];
    return {
      queued: strArr(v.queued),
      hist: strArr(v.hist),
      draft: typeof v.draft === "string" ? v.draft : "",
    };
  } catch {
    return { ...EMPTY };
  }
}

export function patchComposer(sid: string, patch: Partial<ComposerSnap>): void {
  if (!sid) return;
  try {
    const cur = loadComposer(sid);
    sessionStorage.setItem(keyOf(sid), JSON.stringify({ ...cur, ...patch }));
  } catch {
    // quota exceeded / storage disabled — in-memory state still works
    // for the life of this pane; nothing to do.
  }
}
