import { useCallback, useEffect, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import type {
  AuditRecord,
  GetOverviewResponse,
  GetRunResponse,
  RunRef,
  SnapshotRef,
} from "../gen/lyon/v1/dashboard_pb";
import { dashboardClient, errText } from "../api";

function fmtTs(t?: Timestamp): string {
  return t ? timestampDate(t).toLocaleString() : "";
}

function fmtUptime(s: bigint): string {
  const sec = Number(s);
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`;
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`;
}

function fmtDur(ms: bigint): string {
  const v = Number(ms);
  if (v < 1000) return `${v}ms`;
  if (v < 60000) return `${(v / 1000).toFixed(1)}s`;
  return `${Math.floor(v / 60000)}m ${Math.round((v % 60000) / 1000)}s`;
}

// DashboardPanel: the read-only panels over $LYON_HOME — overview
// (aggregates + routing + leaderboard), run list → replay timeline,
// audit tail, saved sessions. The four loads fire concurrently on mount;
// only the overview polls (10s), the rest are explicit refreshes.
export default function DashboardPanel() {
  const [ov, setOv] = useState<GetOverviewResponse>();
  const [runs, setRuns] = useState<RunRef[]>([]);
  const [audit, setAudit] = useState<AuditRecord[]>([]);
  const [snaps, setSnaps] = useState<SnapshotRef[]>([]);
  const [openRun, setOpenRun] = useState<GetRunResponse>();
  const [runDay, setRunDay] = useState("");
  const [auditDay, setAuditDay] = useState("");
  const [err, setErr] = useState("");
  const [loadingRun, setLoadingRun] = useState("");

  const guard = useCallback(
    async (fn: () => Promise<void>) => {
      try {
        await fn();
        setErr("");
      } catch (e) {
        setErr(errText(e));
      }
    },
    [],
  );

  const refreshOverview = useCallback(
    () =>
      guard(async () => {
        setOv(await dashboardClient.getOverview({}));
      }),
    [guard],
  );
  const refreshRuns = useCallback(
    (day: string) =>
      guard(async () => {
        const res = await dashboardClient.listRuns({ day });
        setRuns(res.runs);
      }),
    [guard],
  );
  const refreshAudit = useCallback(
    (day: string) =>
      guard(async () => {
        const res = await dashboardClient.listAudit({ day });
        setAudit(res.records);
      }),
    [guard],
  );
  const refreshSnaps = useCallback(
    () =>
      guard(async () => {
        const res = await dashboardClient.listSnapshots({});
        setSnaps(res.snapshots);
      }),
    [guard],
  );

  useEffect(() => {
    void refreshOverview();
    void refreshRuns(runDay);
    void refreshAudit(auditDay);
    void refreshSnaps();
    const id = setInterval(() => void refreshOverview(), 10_000);
    return () => clearInterval(id);
    // Mount-only by design: the day fields below refresh through their
    // own buttons, so they are not effect dependencies.
  }, []);

  const openRunById = (id: string) => {
    setLoadingRun(id);
    void guard(async () => {
      setOpenRun(await dashboardClient.getRun({ runId: id }));
    }).then(() => setLoadingRun(""));
  };

  return (
    <div className="dash">
      {err && <div className="err">{err}</div>}

      <section>
        <h3>
          overview
          <button className="ghost" onClick={() => void refreshOverview()}>
            ↻
          </button>
        </h3>
        <div className="ov-cells">
          <div className="ov-cell">
            <div className="k">sessions</div>
            <div className="v">{ov ? Number(ov.sessions) : "…"}</div>
          </div>
          <div className="ov-cell">
            <div className="k">uptime</div>
            <div className="v">{ov ? fmtUptime(ov.uptimeSeconds) : "…"}</div>
          </div>
          <div className="ov-cell">
            <div className="k">model</div>
            <div className="v">{ov?.model || "—"}</div>
          </div>
          <div className="ov-cell">
            <div className="k">cache share</div>
            <div className="v">
              {ov ? `${Math.round(ov.cacheShare * 100)}%` : "…"}
            </div>
          </div>
        </div>
        <table>
          <thead>
            <tr>
              <th>class</th>
              <th>agent</th>
              <th>model</th>
              <th>conf</th>
              <th>n</th>
              <th>mean</th>
              <th>on duty</th>
              <th>rounds</th>
              <th>cards</th>
            </tr>
          </thead>
          <tbody>
            {(ov?.leaderboard ?? []).map((lb) => {
              const r = (ov?.routing ?? []).find((x) => x.class === lb.class);
              return (
                <tr key={lb.class}>
                  <td>{lb.class}</td>
                  <td>{r?.agent || "—"}</td>
                  <td>{r?.model || "—"}</td>
                  <td>{r ? r.confidence.toFixed(2) : "—"}</td>
                  <td>{r?.samples ?? "—"}</td>
                  <td>{r ? r.meanScore.toFixed(2) : "—"}</td>
                  <td>{lb.onDuty || "open"}</td>
                  <td>
                    {lb.rounds}
                    {lb.examFails > 0 ? ` (fail×${lb.examFails})` : ""}
                  </td>
                  <td>{lb.cards}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </section>

      <section>
        <h3>
          runs
          <input
            className="day-input"
            placeholder="YYYY-MM-DD"
            value={runDay}
            onChange={(e) => setRunDay(e.target.value)}
          />
          <button className="ghost" onClick={() => void refreshRuns(runDay)}>
            ↻
          </button>
        </h3>
        {runs.length === 0 ? (
          <div className="placeholder">no dumps for this day</div>
        ) : (
          <table>
            <tbody>
              {runs.map((r) => (
                <tr
                  key={r.runId}
                  className="row-run"
                  onClick={() => {
                    if (loadingRun !== r.runId) openRunById(r.runId);
                  }}
                >
                  <td className="run-prompt">
                    {r.failed && <span className="failed">✗ </span>}
                    {r.prompt || "(no prompt)"}
                  </td>
                  <td>{fmtTs(r.startedAt)}</td>
                  <td>{fmtDur(r.durationMs)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {openRun && (
        <section className="run-replay">
          <h3>
            <button className="ghost" onClick={() => setOpenRun(undefined)}>
              ← runs
            </button>
            replay · {openRun.runId}
            <span className="meta">
              {openRun.stopReason} · {fmtDur(openRun.durationMs)} ·{" "}
              {openRun.iterations} it
            </span>
          </h3>
          <div className="run-head">
            <div>{openRun.prompt}</div>
            <div className="meta">
              {openRun.model} · {fmtTs(openRun.startedAt)} →{" "}
              {fmtTs(openRun.endedAt)} · in {Number(openRun.usage?.input ?? 0n)}{" "}
              out {Number(openRun.usage?.output ?? 0n)}
            </div>
            {openRun.promptErr && <div className="err">{openRun.promptErr}</div>}
          </div>
          <ol className="ev-list">
            {openRun.events.map((e) => (
              <li className="ev" key={e.seq}>
                <span className="ev-seq">{e.seq}</span>
                <span className={"ev-type" + (e.isError ? " failed" : "")}>
                  {e.type}
                </span>
                <span className="ev-body">
                  {e.text || e.thinking || e.toolName || e.detail || e.error}
                  {e.inputJson && e.toolName && (
                    <span className="ev-input"> {e.inputJson}</span>
                  )}
                </span>
              </li>
            ))}
          </ol>
        </section>
      )}

      <section>
        <h3>
          audit
          <input
            className="day-input"
            placeholder="YYYY-MM-DD"
            value={auditDay}
            onChange={(e) => setAuditDay(e.target.value)}
          />
          <button className="ghost" onClick={() => void refreshAudit(auditDay)}>
            ↻
          </button>
        </h3>
        {audit.length === 0 ? (
          <div className="placeholder">no audit lines for this day</div>
        ) : (
          <table>
            <tbody>
              {audit.map((a, i) => (
                <tr key={i} className={a.isError ? "failed" : ""}>
                  <td className="audit-time">{a.time.slice(11, 19)}</td>
                  <td>{a.event}</td>
                  <td>{a.tool || a.session}</td>
                  <td className="run-prompt">{a.err || a.detail}</td>
                  <td>{a.durationMs > 0n ? fmtDur(a.durationMs) : ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section>
        <h3>
          snapshots
          <button className="ghost" onClick={() => void refreshSnaps()}>
            ↻
          </button>
        </h3>
        {snaps.length === 0 ? (
          <div className="placeholder">no saved sessions</div>
        ) : (
          <table>
            <tbody>
              {snaps.map((s) => (
                <tr key={s.path}>
                  <td className="run-prompt">{s.title || "(untitled)"}</td>
                  <td>{s.messages} msgs</td>
                  <td>{fmtTs(s.savedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}
