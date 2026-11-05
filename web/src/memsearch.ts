// memsearch — a faithful client mirror of pkg/memory's BM25 index
// (index.go), so the memory panel's 检索验证 (Image #70 F1) ranks facts
// the way the agent's read_memory would. MemoryService has no search
// RPC and there is no buf/protoc to add one, so the ONLY way to answer
// "can this fact even be found?" in the panel is to re-run the same
// tokenizer, field weights and BM25 formula over the ListMemory payload
// already in hand.
//
// Mirror fidelity (index.go):
//   - tokenize: ASCII/digit/underscore runs → lowercased words; CJK
//     (ideograph/kana/hangul) runs → overlapping bigrams (substring
//     recall without a segmenter);
//   - field weights: name 3.0, tags 2.5, description 2.0, body 1.0;
//   - BM25: k1=1.2, b=0.75, idf = ln(1 + (n-df+0.5)/(df+0.5));
//   - ties prefer the closer scope (project > personal > team).
// Anything the server indexes between ListMemory and this run (a fact
// saved milliseconds ago) is the only drift — the score is a mirror,
// not a wire answer.

export interface SearchableFact {
  name: string;
  description: string;
  scope: string;
  tags: string[];
  body: string;
}

export interface MemHit<T extends SearchableFact> {
  fact: T;
  score: number;
}

const K1 = 1.2;
const B = 0.75;
const W_NAME = 3.0;
const W_TAGS = 2.5;
const W_DESC = 2.0;
const W_BODY = 1.0;

// isCJK mirrors index.go isCJK: unified ideographs, ext A, compat,
// kana, hangul.
function isCJK(code: number): boolean {
  return (
    (code >= 0x4e00 && code <= 0x9fff) ||
    (code >= 0x3400 && code <= 0x4dbf) ||
    (code >= 0xf900 && code <= 0xfaff) ||
    (code >= 0x3040 && code <= 0x30ff) ||
    (code >= 0xac00 && code <= 0xd7af)
  );
}

/** tokenize: the exact split index.go performs (lowercase first). */
export function tokenize(s: string): string[] {
  const out: string[] = [];
  const lower = s.toLowerCase();
  let word = "";
  let cjk: string[] = [];
  const flushWord = () => {
    if (word) {
      out.push(word);
      word = "";
    }
  };
  const flushCJK = () => {
    if (cjk.length === 1) out.push(cjk[0]);
    else for (let i = 0; i + 1 < cjk.length; i++) out.push(cjk[i] + cjk[i + 1]);
    cjk = [];
  };
  for (const r of lower) {
    const code = r.codePointAt(0) ?? 0;
    if ((code >= 97 && code <= 122) || (code >= 48 && code <= 57) || r === "_") {
      flushCJK();
      word += r;
    } else if (isCJK(code)) {
      flushWord();
      cjk.push(r);
    } else {
      flushWord();
      flushCJK();
    }
  }
  flushWord();
  flushCJK();
  return out;
}

/** uniqueTokens: query terms in first-seen order (index.go). */
export function uniqueTokens(s: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const t of tokenize(s)) {
    if (!seen.has(t)) {
      seen.add(t);
      out.push(t);
    }
  }
  return out;
}

function scopeRank(scope: string): number {
  if (scope === "project") return 3;
  if (scope === "personal") return 2;
  if (scope === "team") return 1;
  return 0;
}

type Doc = { tfs: Map<string, number>; len: number };

function docTerms(f: SearchableFact): Doc {
  const tfs = new Map<string, number>();
  const add = (text: string, w: number) => {
    for (const t of tokenize(text)) tfs.set(t, (tfs.get(t) ?? 0) + w);
  };
  add(f.name, W_NAME);
  add(f.tags.join(" "), W_TAGS);
  add(f.description, W_DESC);
  add(f.body, W_BODY);
  let len = 0;
  for (const tf of tfs.values()) len += tf;
  return { tfs, len };
}

/** memSearch: score every fact carrying ≥1 query term, BM25-ranked,
 *  closer scope winning ties. Empty query or no matching term → []. */
export function memSearch<T extends SearchableFact>(query: string, facts: T[]): MemHit<T>[] {
  const qTerms = uniqueTokens(query);
  if (qTerms.length === 0 || facts.length === 0) return [];
  const docs = facts.map(docTerms);
  const n = facts.length;
  let total = 0;
  for (const d of docs) total += d.len;
  const avgLen = n > 0 ? total / n : 1;
  // term → docs containing it (one posting per doc per term, like index.go)
  const postings = new Map<string, number[]>();
  docs.forEach((d, i) => {
    for (const t of d.tfs.keys()) {
      const arr = postings.get(t);
      if (arr) arr.push(i);
      else postings.set(t, [i]);
    }
  });
  const scores = new Map<number, number>();
  for (const t of qTerms) {
    const ps = postings.get(t);
    if (!ps || ps.length === 0) continue;
    const df = ps.length;
    const idf = Math.log(1 + (n - df + 0.5) / (df + 0.5));
    for (const doc of ps) {
      const tf = docs[doc].tfs.get(t)!;
      const norm = K1 * (1 - B + (B * docs[doc].len) / avgLen) + tf;
      scores.set(doc, (scores.get(doc) ?? 0) + (idf * tf * (K1 + 1)) / norm);
    }
  }
  const hits: MemHit<T>[] = [];
  scores.forEach((score, doc) => hits.push({ fact: facts[doc], score }));
  hits.sort((a, b) =>
    a.score !== b.score
      ? b.score - a.score
      : scopeRank(b.fact.scope) - scopeRank(a.fact.scope),
  );
  return hits;
}

/** highlight: split text into segments, marking every case-insensitive
 *  occurrence of any query term (CJK bigrams mark their 2-char runs —
 *  noisy by design: it shows WHY the row matched). */
export function highlight(text: string, terms: string[]): { s: string; on: boolean }[] {
  if (!text || terms.length === 0) return [{ s: text, on: false }];
  const low = text.toLowerCase();
  const out: { s: string; on: boolean }[] = [];
  let buf = "";
  let i = 0;
  outer: while (i < text.length) {
    for (const t of terms) {
      if (t && low.startsWith(t, i)) {
        if (buf) {
          out.push({ s: buf, on: false });
          buf = "";
        }
        out.push({ s: text.slice(i, i + t.length), on: true });
        i += t.length;
        continue outer;
      }
    }
    buf += text[i];
    i++;
  }
  if (buf) out.push({ s: buf, on: false });
  return out;
}

/** excerpt: a body window around the FIRST term hit (±context runes),
 *  so the reader sees the matching passage; full text is one click
 *  away in the expanded row. No hit in the body → leading window. */
export function excerpt(text: string, terms: string[], before = 40, span = 150): string {
  if (!text) return "";
  const low = text.toLowerCase();
  let idx = -1;
  for (const t of terms) {
    if (!t) continue;
    const at = low.indexOf(t);
    if (at >= 0 && (idx < 0 || at < idx)) idx = at;
  }
  if (idx < 0) return text.slice(0, before + span);
  const start = Math.max(0, idx - before);
  const end = Math.min(text.length, idx + span);
  return (start > 0 ? "…" : "") + text.slice(start, end) + (end < text.length ? "…" : "");
}
