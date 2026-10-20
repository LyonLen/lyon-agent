// MemoryPanel — the project-scope memory store (TUI /memory parity for
// what the browser can reach: FileService sees the workspace only, so the
// team/personal layers in $LYON_HOME stay out of reach and are noted as
// such). Each row is one <workspace>/.lyon/memory/*.md fact: frontmatter
// parsed on load (field semantics mirror pkg/memory/fact.go decodeFact),
// hot is a one-line flag rewrite, forget deletes the file. All fact files
// load in parallel — a store with many facts must not stagger in.

import { useCallback, useEffect, useRef, useState } from "react";
import { errText, fileClient } from "../api";

const MEM_DIR = ".lyon/memory";

interface MemFact {
  file: string; // "some-fact.md"
  name: string;
  description: string;
  scope: string;
  hot: boolean;
  hits: number;
  body: string;
}

// decode mirrors decodeFact: optional frontmatter ("---" fence), key:
// value lines, the rest is the body; without frontmatter the file base
// is the name and the whole text is the body.
function decode(file: string, text: string): MemFact {
  const base = file.replace(/\.md$/i, "");
  const f: MemFact = {
    file,
    name: base,
    description: "",
    scope: "project",
    hot: false,
    hits: 0,
    body: text.trim(),
  };
  if (!text.startsWith("---\n")) return f;
  const rest = text.slice(4);
  const end = rest.indexOf("\n---");
  if (end < 0) return f;
  const fm = rest.slice(0, end);
  let body = rest.slice(end + 4);
  const nl = body.indexOf("\n");
  if (nl >= 0) body = body.slice(nl + 1); // newline right after the fence
  f.body = body.trim();
  for (const line of fm.split("\n")) {
    const i = line.indexOf(":");
    if (i < 0) continue;
    const key = line.slice(0, i).trim().toLowerCase();
    const val = line.slice(i + 1).trim();
    switch (key) {
      case "name":
        if (val) f.name = val;
        break;
      case "description":
        f.description = val;
        break;
      case "scope":
        if (val) f.scope = val;
        break;
      case "hot":
        f.hot = val === "true" || val === "yes" || val === "1";
        break;
      case "hits":
        f.hits = Number(val) || 0;
        break;
    }
  }
  return f;
}

// hotFlip rewrites only the hot line (encode() writes "hot: true" and
// omits it when false), leaving body and every other field intact.
function hotFlip(text: string, hot: boolean): string {
  if (!text.startsWith("---\n")) {
    // hand-written fact without frontmatter — prepend a minimal head
    return hot ? `---\nhot: true\n---\n\n${text.trim()}\n` : text;
  }
  const rest = text.slice(4);
  const end = rest.indexOf("\n---");
  if (end < 0) return text;
  const fm = rest
    .slice(0, end)
    .split("\n")
    .filter((l) => !/^\s*hot\s*:/i.test(l));
  if (hot) {
    const at = fm.findIndex((l) => /^\s*scope\s*:/i.test(l));
    if (at >= 0) fm.splice(at + 1, 0, "hot: true");
    else fm.push("hot: true");
  }
  return `---\n${fm.join("\n")}\n---${rest.slice(end + 4)}`;
}

export default function MemoryPanel() {
  const [facts, setFacts] = useState<MemFact[]>([]);
  const [raw, setRaw] = useState<Record<string, string>>({}); // file → on-disk text
  const [err, setErr] = useState("");
  const [confirmDel, setConfirmDel] = useState("");
  const [flash, setFlash] = useState("");
  const flashTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const note = (m: string) => {
    setFlash(m);
    if (flashTimer.current) clearTimeout(flashTimer.current);
    flashTimer.current = setTimeout(() => setFlash(""), 2500);
  };

  const load = useCallback(async () => {
    setErr("");
    try {
      const dir = await fileClient.listDirectory({ path: MEM_DIR });
      const md = dir.entries.filter((e) => e.name.toLowerCase().endsWith(".md") && !e.isDir);
      const texts = await Promise.all(
        md.map((e) => fileClient.readTextFile({ path: `${MEM_DIR}/${e.name}` })),
      );
      const rawMap: Record<string, string> = {};
      const parsed = md.map((e, i) => {
        rawMap[e.name] = texts[i].content;
        return decode(e.name, texts[i].content);
      });
      parsed.sort((a, b) => a.name.localeCompare(b.name));
      setRaw(rawMap);
      setFacts(parsed);
    } catch (e) {
      setFacts([]);
      setErr(errText(e));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Second click confirms the delete; the prompt self-expires.
  useEffect(() => {
    if (!confirmDel) return;
    const t = setTimeout(() => setConfirmDel(""), 3000);
    return () => clearTimeout(t);
  }, [confirmDel]);

  const toggleHot = async (f: MemFact) => {
    const next = !f.hot;
    const text = hotFlip(raw[f.file] ?? f.body, next);
    try {
      await fileClient.writeFile({ path: `${MEM_DIR}/${f.file}`, content: text });
      setRaw((r) => ({ ...r, [f.file]: text }));
      setFacts((fs) => fs.map((x) => (x.file === f.file ? { ...x, hot: next } : x)));
      note(next ? `hot: ${f.name}` : `warm: ${f.name}`);
    } catch (e) {
      setErr(errText(e));
    }
  };

  const forget = async (f: MemFact) => {
    if (confirmDel !== f.file) {
      setConfirmDel(f.file);
      return;
    }
    setConfirmDel("");
    try {
      await fileClient.delete({ path: `${MEM_DIR}/${f.file}`, recursive: false });
      setFacts((fs) => fs.filter((x) => x.file !== f.file));
      note(`forgotten: ${f.name}`);
    } catch (e) {
      setErr(errText(e));
    }
  };

  const hotCount = facts.filter((f) => f.hot).length;
  return (
    <div className="mem-panel">
      <div className="mem-head">
        <span className="mem-stats">
          memory project={facts.length} hot={hotCount}
        </span>
        <span className="mem-flash">{flash}</span>
        <button className="ghost" onClick={() => void load()}>
          reload
        </button>
      </div>
      {err && <div className="mem-err err">{err}</div>}
      {facts.length === 0 && !err && (
        <div className="placeholder">
          empty — save_memory from the model, /remember to distill this session
        </div>
      )}
      {facts.map((f) => (
        <div key={f.file} className={"mem-row" + (f.hot ? " hot" : "")}>
          <button
            className={"mem-hot" + (f.hot ? " on" : "")}
            title="toggle hot tier"
            onClick={() => void toggleHot(f)}
          >
            ●
          </button>
          <div className="mem-main">
            <div className="mem-name">
              {f.name}
              {f.hits > 0 && <span className="mem-hits">hits={f.hits}</span>}
            </div>
            {f.description && <div className="mem-desc">{f.description}</div>}
          </div>
          <span className="mem-scope">{f.scope}</span>
          <button className="ghost mem-forget" onClick={() => void forget(f)}>
            {confirmDel === f.file ? "confirm?" : "forget"}
          </button>
        </div>
      ))}
      <div className="mem-note">
        project scope only — team/personal live in $LYON_HOME (unreachable from the browser)
      </div>
    </div>
  );
}
