import { useEffect, useRef, useState } from "react";
import { cloneRepo, errText, rememberWorkspace, workspaceHistory } from "../api";

// NewSessionDlg is the new-session workspace chooser, laid out like the
// VS Code welcome screen: one-click action rows (current directory,
// browse a folder, clone from git) plus a Recent list — a form only
// unfolds where a form is actually needed.
// WsRow is one welcome-style action row. Module-level on purpose: an
// inline component type remounts its whole subtree on every parent
// render — background polls re-render the dialog and swallow clicks
// mid-press, which is exactly the "点不动" report.
function WsRow(p: {
  ico: string;
  title: string;
  sub: string;
  onClick: () => void;
  active?: boolean;
  disabled?: boolean;
}) {
  return (
    <button
      className={"ws-opt" + (p.active ? " active" : "")}
      onClick={p.onClick}
      disabled={p.disabled}
    >
      <span className="ws-ico">{p.ico}</span>
      <span className="ws-txt">
        <span className="ws-t">{p.title}</span>
        <span className="ws-s">{p.sub}</span>
      </span>
    </button>
  );
}

export default function NewSessionDlg(props: {
  current?: string;
  onCreate: (workspace: string) => Promise<void> | void;
  onCancel: () => void;
}) {
  const [open, setOpen] = useState<"" | "dir" | "git">("");
  const [ws, setWs] = useState("");
  const [git, setGit] = useState("");
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState("");
  const [err, setErr] = useState("");
  const hist = useRef(workspaceHistory());

  const currentWs = props.current || localStorage.getItem("lyon_last_workspace") || "";

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !busy) props.onCancel();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [busy]);

  // commit creates the session rooted at root — the one exit for every
  // row and the unfolded forms.
  const commit = async (root: string) => {
    if (busy) return;
    setErr("");
    setBusy("正在创建会话 …");
    try {
      rememberWorkspace(root);
      await props.onCreate(root);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(""); // success unmounts the dialog; failure must re-enable rows
    }
  };

  const submitForm = async () => {
    if (open === "git") {
      const url = git.trim();
      if (!url) return;
      if (busy) return;
      setErr("");
      setBusy("正在 clone …");
      try {
        const root = await cloneRepo(url, target.trim());
        rememberWorkspace(root);
        setBusy("正在创建会话 …");
        await props.onCreate(root);
      } catch (e) {
        setErr(errText(e));
      } finally {
        setBusy("");
      }
      return;
    }
    const root = ws.trim();
    if (root) await commit(root);
  };

  return (
    <div className="dlg-backdrop" onClick={() => !busy && props.onCancel()}>
      <div className="dlg ws-dlg" onClick={(e) => e.stopPropagation()}>
        <div className="dlg-title">新会话 · 选择工作区</div>
        <div className="dlg-detail">每个会话都有自己的工作区 —— 选一个开始。</div>

        <WsRow
          disabled={busy !== ""}
          ico="▣"
          title="当前目录"
          sub={currentWs || "引擎默认工作区"}
          onClick={() => void commit(currentWs)}
        />
        <WsRow
          disabled={busy !== ""}
          ico="📂"
          title="打开本地目录…"
          sub={open === "dir" ? "输入路径后按回车创建" : "键入一个已有目录作为工作区"}
          onClick={() => setOpen(open === "dir" ? "" : "dir")}
          active={open === "dir"}
        />
        {open === "dir" && (
          <input
            className="ws-input"
            autoFocus
            list="ws-history"
            placeholder="K:/project"
            value={ws}
            onChange={(e) => setWs(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && void submitForm()}
          />
        )}
        <datalist id="ws-history">
          {hist.current.map((h) => (
            <option key={h} value={h} />
          ))}
        </datalist>

        <WsRow
          disabled={busy !== ""}
          ico="⎇"
          title="从 Git 克隆…"
          sub={open === "git" ? "克隆完成后自动以克隆目录建会话" : "https / ssh 地址"}
          onClick={() => setOpen(open === "git" ? "" : "git")}
          active={open === "git"}
        />
        {open === "git" && (
          <>
            <input
              className="ws-input"
              autoFocus
              placeholder="https://github.com/user/repo.git"
              value={git}
              onChange={(e) => setGit(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && void submitForm()}
            />
            <input
              className="ws-input"
              placeholder="目标目录（可选，默认克隆到当前目录同级）"
              value={target}
              onChange={(e) => setTarget(e.target.value)}
            />
          </>
        )}

        {hist.current.length > 0 && (
          <>
            <div className="ws-sec">最近使用</div>
            {hist.current.map((h) => (
              <WsRow
                disabled={busy !== ""}
                key={h}
                ico="◷"
                title={h.split(/[\\/]/).filter(Boolean).pop() || h}
                sub={h}
                onClick={() => void commit(h)}
              />
            ))}
          </>
        )}

        {err && (
          <div className="file-err">
            <span className="err">{err}</span>
          </div>
        )}
        <div className="dlg-actions">
          <button className="ghost" onClick={props.onCancel} disabled={busy !== ""}>
            取消
          </button>
          {(open === "dir" && ws.trim()) || (open === "git" && git.trim()) ? (
            <button onClick={() => void submitForm()} disabled={busy !== ""}>
              {busy || (open === "git" ? "克隆并创建" : "创建")}
            </button>
          ) : (
            <button className="ghost" disabled title="直接点上方的选项行即可创建">
              {busy || "创建"}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
