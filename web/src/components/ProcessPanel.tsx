// ProcessPanel — live background-shell tasks (proto process.proto): every
// run_shell background=true a session started, one row with the command,
// pid, owning session, elapsed, and the output tail; 结束 is a two-click
// kill through KillProcess (the same tree-kill the owner sweep uses).
//
// The registry only holds RUNNING tasks — exit unregisters — so the
// panel's3s poll IS the status: a row vanishing means the command
// finished (its story lands in chat as background_end + tail). The tail
// here is the glance; the full stream is the log path (read_file it in
// chat).

import { useEffect, useState } from "react";
import { errText, fetchListeners, processClient } from "../api";
import type { Listener } from "../api";
import type { ProcessInfo } from "../gen/lyon/v1/process_pb";

// fmtElapsed renders an age the way a process list does: seconds under
// a minute, then m/s, then h/m — the tick below keeps it live.
function fmtElapsed(ms: number): string {
  if (ms < 0) return "0s";
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s`;
  const m = Math.floor(ms / 60_000);
  if (m < 60) return `${m}m ${Math.floor((ms % 60_000) / 1000)}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

export default function ProcessPanel() {
  const [rows, setRows] = useState<ProcessInfo[]>([]);
  const [err, setErr] = useState("");
  const [note, setNote] = useState("");
  // Two-click kill (the session-row delete pattern): first click arms,
  // a second confirms; the prompt self-expires.
  const [arm, setArm] = useState(0);
  const [openTail, setOpenTail] = useState<number | null>(null);
  // Local 1s tick so elapsed ages without waiting for the3s poll.
  const [, setTick] = useState(0);
  // Host listeners back the empty state: "0" alone is a lie (a service the
  // human or an old session left with nohup never enters the registry), so
  // with nothing managed the panel shows what the box listens on instead.
  const [ports, setPorts] = useState<Listener[]>([]);
  const [portsErr, setPortsErr] = useState("");

  const load = async () => {
    try {
      const res = await processClient.listProcesses({});
      setRows(res.processes);
      setErr("");
    } catch (e) {
      setErr(errText(e));
    }
  };

  useEffect(() => {
    void load();
    const poll = setInterval(() => void load(), 3000);
    const tick = setInterval(() => setTick((x) => x + 1), 1000);
    return () => {
      clearInterval(poll);
      clearInterval(tick);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Listener polling only runs while the registry is empty: with rows on
  // screen the managed view is the answer, and netstat costs a process
  // spawn every tick.
  useEffect(() => {
    if (rows.length > 0) return;
    let stop = false;
    const grab = async () => {
      try {
        const res = await fetchListeners();
        if (stop) return;
        setPorts(res.listeners ?? []);
        setPortsErr(res.error ?? "");
      } catch (e) {
        if (!stop) setPortsErr(errText(e));
      }
    };
    void grab();
    const poll = setInterval(() => void grab(), 5000);
    return () => {
      stop = true;
      clearInterval(poll);
    };
  }, [rows.length]);

  const kill = async (id: number) => {
    if (arm !== id) {
      setArm(id);
      window.setTimeout(() => setArm((a) => (a === id ? 0 : a)), 3000);
      return;
    }
    setArm(0);
    try {
      await processClient.killProcess({ id });
      setNote(`已结束 #${id} — 树一并收走`);
      window.setTimeout(() => setNote(""), 3000);
      // The waiter unregisters on exit; a beat later the row is gone.
      window.setTimeout(() => void load(), 400);
    } catch (e) {
      setErr(errText(e));
      void load(); // stale id (already exited) → refresh the list
    }
  };

  const now = Date.now();
  return (
    <div className="pp-panel">
      <div className="pp-head">
        <span
          className="pp-title"
          title="只统计仍在运行的 run_shell background=true 任务：进程退出即从列表消失，结果以 background_end 回到聊天。"
        >
          后台进程 <b>{rows.length}</b>
        </span>
        {note && <span className="pp-note">{note}</span>}
        {rows.length > 0 && (
          <span className="pp-meta" title="由模型以 background=true 起的常驻命令；本表的结束键做整棵树 kill">
            run_shell background=true · 退出即消失
          </span>
        )}
        <button type="button" className="ghost" title="刷新" onClick={() => void load()}>
          ↻
        </button>
      </div>
      {err && <div className="pp-err">{err}</div>}
      {rows.length === 0 && !err && (
        <>
          <div
            className="pp-strip"
            title="这里只列模型用 run_shell background=true 起的常驻命令。用 nohup / 别的终端起的服务不在注册表里，所以下面改列本机实际监听的端口。"
          >
            <span className="pp-strip-dot" aria-hidden="true" />
            没有托管的常驻任务
          </div>
          <div className="pp-ports">
            <div className="pp-ports-head">
              <span>本机监听端口</span>
              <span className="pp-tag" title="不是 lyon 起的进程：这里的行只读，kill 请自己动手">
                非托管
              </span>
              <span className="pp-ports-meta">
                {portsErr ? portsErr : `${ports.length} 个`}
              </span>
            </div>
            {ports.map((p) => (
              <div
                key={`${p.pid}/${p.ports.join(",")}`}
                className="pp-port-row"
                title={`${p.ports.join(", ")} · pid ${p.pid || "?"}`}
              >
                <span className="pp-ports-n">{p.ports.join(", ")}</span>
                <span className="pp-pname">{p.name || "—"}</span>
                {p.managed && (
                  <span className="pp-tag pp-tag-ok" title="这个 pid 在后台注册表里：见上面的托管视图">
                    托管
                  </span>
                )}
                <span className="pp-ppid">pid {p.pid || "?"}</span>
              </div>
            ))}
            {!portsErr && ports.length === 0 && (
              <div className="pp-ports-empty">1024–49151 这一带没有监听端口</div>
            )}
          </div>
        </>
      )}
      <div className="pp-list">
        {rows.map((p) => {
          const elapsed = now - Number(p.startedUnix) * 1000;
          const who = p.sessionTitle || p.owner;
          const expanded = openTail === p.id;
          return (
            <div key={p.id} className="pp-row">
              <span className="pp-dot" aria-hidden="true" />
              <div className="pp-main">
                <div className="pp-line">
                  <code className="pp-cmd" title={p.command}>
                    {p.command}
                  </code>
                  <span
                    className="pp-meta"
                    title={`pid ${p.pid} · owner ${p.owner}${p.sessionId ? ` · seat ${p.sessionId}` : ""}`}
                  >
                    pid {p.pid} · {who} · {fmtElapsed(elapsed)}
                  </span>
                  <span className="pp-acts">
                    <button
                      type="button"
                      className="ghost"
                      title="输出尾部（全文用 read_file 读 log）"
                      onClick={() => setOpenTail(expanded ? null : p.id)}
                    >
                      {expanded ? "收起" : "输出"}
                    </button>
                    <button
                      type="button"
                      className={"ghost danger" + (arm === p.id ? " armed" : "")}
                      title="结束这棵进程树"
                      onClick={() => void kill(p.id)}
                    >
                      {arm === p.id ? "确认?" : "结束"}
                    </button>
                  </span>
                </div>
                {expanded && (
                  <pre className="pp-tail">{p.tail || "（还没有输出）"}</pre>
                )}
                {expanded && (
                  <div className="pp-log" title={p.logPath}>
                    log: {p.logPath}
                  </div>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
