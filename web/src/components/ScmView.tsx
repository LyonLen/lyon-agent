// ScmView — the files panel's source-control section, mimicking the
// VS Code source-control sidebar: changes list with status letters, a
// commit box (Ctrl+Enter), a sync button carrying the ahead+behind
// count, and the commit graph with ref badges (main, origin/main).
// Read path = GitService.Status + .Log polled together; Commit/Sync
// are the only writes, both re-read the state they changed.

import { useCallback, useEffect, useRef, useState } from "react";
import type { GitCommit, StatusResponse } from "../gen/lyon/v1/git_pb";
import { FileKind } from "../gen/lyon/v1/file_pb";
import { errText, gitClient } from "../api";
import SkeletonRows from "./Skeleton";

// Status letter + tone per porcelain kind — the sidebar's icon colors.
const KIND_TONE: Record<string, { letter: string; cls: string }> = {
  modified: { letter: "M", cls: "k-mod" },
  added: { letter: "A", cls: "k-add" },
  deleted: { letter: "D", cls: "k-del" },
  renamed: { letter: "R", cls: "k-ren" },
  untracked: { letter: "?", cls: "k-un" },
  conflicted: { letter: "U", cls: "k-con" },
};

// One decoration → badge. "HEAD -> main" renders as the branch badge;
// "origin/main" as the remote badge; tags stay plain.
function refBadge(ref: string): { text: string; cls: string } {
  if (ref.startsWith("HEAD -> ")) return { text: ref.slice(8), cls: "b-head" };
  if (ref.startsWith("tag: ")) return { text: ref.slice(5), cls: "b-tag" };
  if (ref.includes("/")) return { text: ref, cls: "b-remote" };
  return { text: ref, cls: "b-head" };
}

export default function ScmView({
  onOpenFile,
}: {
  /** A changed file was clicked; open it in a preview tab. */
  onOpenFile: (path: string, kind: FileKind) => void;
}) {
  const [st, setSt] = useState<StatusResponse>();
  const [commits, setCommits] = useState<GitCommit[]>([]);
  const [err, setErr] = useState("");
  const [flash, setFlash] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");
  const msgRef = useRef<HTMLInputElement>(null);

  const load = useCallback(async () => {
    try {
      const [s, l] = await Promise.all([
        gitClient.status({}),
        gitClient.log({ limit: 30 }),
      ]);
      setSt(s);
      setCommits(l.commits);
      setErr("");
    } catch (e) {
      setErr(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    // The tree moves with every agent edit; 10s matches DashboardPanel.
    const t = setInterval(() => void load(), 10000);
    return () => clearInterval(t);
  }, [load]);

  // Transient success line ("committed abc1234") — self-clearing.
  useEffect(() => {
    if (!flash) return;
    const t = setTimeout(() => setFlash(""), 4000);
    return () => clearTimeout(t);
  }, [flash]);

  const commit = async () => {
    const message = msg.trim();
    if (!message || busy) return;
    setBusy(true);
    try {
      const res = await gitClient.commit({ message });
      setMsg("");
      setFlash(`committed ${res.sha}`);
      await load();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  };

  const sync = async () => {
    if (busy) return;
    setBusy(true);
    try {
      await gitClient.sync({});
      setFlash("synced");
      await load();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  };

  if (loading && !st) {
    return (
      <div className="scm">
        <SkeletonRows n={6} />
      </div>
    );
  }

  // Not a git workspace: the whole section collapses to one line.
  if (st && !st.isRepo) {
    return (
      <div className="scm">
        <div className="placeholder">not a git repository</div>
      </div>
    );
  }

  const changes = st?.changes ?? [];
  const ahead = st?.ahead ?? 0;
  const behind = st?.behind ?? 0;
  const outgoing = commits.filter((c) => c.outgoing);
  const rest = commits.filter((c) => !c.outgoing);

  return (
    <div className="scm">
      {err && <div className="err">{err}</div>}
      {flash && <div className="scm-flash">{flash}</div>}

      {/* 变更 — the changes list + commit box + sync */}
      <div className="scm-head">
        <span className="scm-title">变更</span>
        <span className="scm-count">{changes.length}</span>
        <button className="ghost scm-icon" title="refresh" onClick={() => void load()}>
          ⟳
        </button>
      </div>

      <div className="scm-commit">
        <input
          ref={msgRef}
          placeholder={`提交变更内容 (Ctrl+Enter 在 ${st?.branch ?? ""} 提交)`}
          value={msg}
          onChange={(e) => setMsg(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) void commit();
          }}
        />
        <button
          className="ghost scm-icon"
          title="commit (Ctrl+Enter)"
          disabled={busy || !msg.trim()}
          onClick={() => void commit()}
        >
          ✓
        </button>
      </div>

      {st?.upstream ? (
        <button className="ghost scm-sync" disabled={busy} onClick={() => void sync()}>
          {busy ? "syncing…" : `↻ 同步更改 ${ahead + behind}`}
        </button>
      ) : (
        <div className="scm-noupstream">无上游 — 仅本地提交</div>
      )}

      {changes.length === 0 ? (
        <div className="placeholder">工作区干净</div>
      ) : (
        <div className="scm-changes">
          {changes.map((c) => {
            const tone = KIND_TONE[c.kind] ?? KIND_TONE.modified;
            return (
              <button
                key={`${c.xy}:${c.path}`}
                className="scm-row"
                title={`${c.path} (${c.xy}${c.staged ? ", staged" : ""})`}
                onClick={() => onOpenFile(c.path, FileKind.TEXT)}
              >
                <span className={`scm-kind ${tone.cls}`}>{tone.letter}</span>
                <span className="scm-path">{c.path}</span>
                {c.staged && <span className="scm-staged">staged</span>}
              </button>
            );
          })}
        </div>
      )}

      {/* 图形 — outgoing group first (VS Code's 传出的更改), then the rest */}
      <div className="scm-head">
        <span className="scm-title">图形</span>
        <span className="scm-count">{commits.length}</span>
      </div>
      {commits.length === 0 ? (
        <div className="placeholder">no commits</div>
      ) : (
        <>
          {outgoing.length > 0 && (
            <div className="scm-group">
              传出的更改 <span className="scm-badge b-head">{st?.branch}</span>
            </div>
          )}
          {outgoing.map((c) => (
            <CommitRow key={c.sha} c={c} />
          ))}
          {outgoing.length > 0 && rest.length > 0 && (
            <div className="scm-group">最近提交</div>
          )}
          {rest.map((c) => (
            <CommitRow key={c.sha} c={c} />
          ))}
        </>
      )}
    </div>
  );
}

// One graph row: dot (accent when outgoing), subject, ref badges.
function CommitRow({ c }: { c: GitCommit }) {
  const sha = c.sha.slice(0, 7);
  const when = c.atUnix > 0n ? new Date(Number(c.atUnix) * 1000).toLocaleString() : "";
  return (
    <div className="scm-commit-row" title={`${c.sha} — ${c.author} — ${when}`}>
      <span className={"scm-dot" + (c.outgoing ? " out" : "")} />
      <span className="scm-subject">{c.subject}</span>
      <span className="scm-sha">{sha}</span>
      {c.refs.map((r) => {
        const b = refBadge(r);
        return (
          <span key={r} className={`scm-badge ${b.cls}`}>
            {b.text}
          </span>
        );
      })}
    </div>
  );
}
