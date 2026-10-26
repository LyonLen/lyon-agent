// EvolvePanel — the evolution loop's read-only web surface: candidate
// lineage (Gen/Parent/seats), the specialist-space stores, frozen
// procedures and the exam-bank footprint via EvolveService, composed
// with DashboardService's routing/leaderboard rows (one source per
// dataset — this panel never re-reads what the dashboard owns). Actions
// (run exam, run contest, save flow) stay TUI-only in v1; the panel
// says so instead of pretending.

import { useCallback, useEffect, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import type { Candidate, GetEvolveStateResponse } from "../gen/lyon/v1/evolve_pb";
import type { GetOverviewResponse } from "../gen/lyon/v1/dashboard_pb";
import { dashboardClient, errText, evolveClient } from "../api";

function fmtTs(t?: Timestamp): string {
  return t && t.seconds > 0n ? timestampDate(t).toLocaleString() : "—";
}

// One row of the lineage table. Seats empty = off duty everywhere.
function seatChips(c: Candidate): string {
  return c.seats.length > 0 ? c.seats.join(", ") : "—";
}

export default function EvolvePanel() {
  const [st, setSt] = useState<GetEvolveStateResponse>();
  const [ov, setOv] = useState<GetOverviewResponse>();
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [state, overview] = await Promise.all([
        evolveClient.getEvolveState({}),
        dashboardClient.getOverview({}),
      ]);
      setSt(state);
      setOv(overview);
      setErr("");
    } catch (e) {
      setErr(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    // The ledger moves with every prompt; 15s keeps the panel live
    // without hammering the disk (DashboardPanel polls overview at 10s).
    const t = setInterval(() => void load(), 15000);
    return () => clearInterval(t);
  }, [load]);

  const seatsHeld = st?.candidates.reduce((n, c) => n + c.seats.length, 0) ?? 0;
  const banksSeeded = st?.banks.filter((b) => b.seeded).length ?? 0;
  const questions = st?.banks.reduce((n, b) => n + Number(b.questions), 0) ?? 0;

  return (
    <div className="dash">
      {err && <div className="err">{err}</div>}

      <div className="ov-cells">
        <div className="ov-cell">
          <div className="k">incumbent</div>
          <div className="v">{st?.selfModel || "—"}</div>
        </div>
        <div className="ov-cell">
          <div className="k">lineage</div>
          <div className="v">{st ? st.candidates.length : "…"}</div>
        </div>
        <div className="ov-cell">
          <div className="k">seats held</div>
          <div className="v">{st ? seatsHeld : "…"}</div>
        </div>
        <div className="ov-cell">
          <div className="k">exam banks</div>
          <div className="v">
            {st ? `${banksSeeded}/${st.banks.length}` : "…"}
            {st && <span className="meta"> {questions} questions</span>}
          </div>
        </div>
        <div className="ov-cell">
          <div className="k">flows</div>
          <div className="v">{st ? st.flows.length : "…"}</div>
        </div>
        <div className="ov-cell">
          <div className="k">classes</div>
          <div className="v">
            {ov ? `${ov.leaderboard.length}` : "…"}
          </div>
        </div>
        <button className="ghost" onClick={() => void load()} disabled={loading}>
          {loading ? "loading…" : "refresh"}
        </button>
      </div>

      <section>
        <h3>
          lineage <span className="meta">generations distill failures — gen 3 caps a line</span>
        </h3>
        {!st || st.candidates.length === 0 ? (
          <div className="placeholder">no candidates</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>id</th>
                <th>gen</th>
                <th>model</th>
                <th>effort</th>
                <th>parent</th>
                <th>seats</th>
                <th>created</th>
                <th>note</th>
              </tr>
            </thead>
            <tbody>
              {st.candidates.map((c) => (
                <tr key={c.id}>
                  <td>
                    {c.id}
                    {c.self && <span className="meta"> (self)</span>}
                  </td>
                  <td>{c.gen > 0 ? `g${c.gen}` : "—"}</td>
                  <td>{c.model || "—"}</td>
                  <td>{c.effort || "—"}</td>
                  <td>{c.parent || "—"}</td>
                  <td>{seatChips(c)}</td>
                  <td>{fmtTs(c.created)}</td>
                  <td className="run-prompt">{c.note}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section>
        <h3>
          seats &amp; routing <span className="meta">leaderboard · steering bar (3+ samples, conf ≥ 0.35)</span>
        </h3>
        {!ov || ov.leaderboard.length === 0 ? (
          <div className="placeholder">no classes yet — run a prompt</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>class</th>
                <th>on duty</th>
                <th>rounds</th>
                <th>exam fails</th>
                <th>cards</th>
                <th>mean</th>
                <th>newest</th>
              </tr>
            </thead>
            <tbody>
              {ov.leaderboard.map((v) => (
                <tr key={v.class}>
                  <td>{v.class}</td>
                  <td>{v.onDuty || <span className="meta">open</span>}</td>
                  <td>
                    {v.rounds}
                    {v.rounds > 0 && v.rounds % 10 === 0 && (
                      <span className="meta"> (due)</span>
                    )}
                  </td>
                  <td>{v.examFails > 0 ? v.examFails : "—"}</td>
                  <td>{v.cards}</td>
                  <td>{v.cards > 0 ? v.mean.toFixed(2) : "—"}</td>
                  <td>
                    {v.cards > 0
                      ? `${v.lastAgent} ${v.lastTotal.toFixed(2)}${v.lastPassed ? " ✓" : " ✗"}${v.lastSource === "exam" ? " (exam)" : ""}`
                      : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {ov && ov.routing.length > 0 && (
          <table>
            <thead>
              <tr>
                <th>route class</th>
                <th>agent</th>
                <th>model</th>
                <th>conf</th>
                <th>samples</th>
                <th>mean</th>
                <th>steers</th>
              </tr>
            </thead>
            <tbody>
              {ov.routing.map((r) => (
                <tr key={r.class}>
                  <td>{r.class}</td>
                  <td>{r.agent}</td>
                  <td>{r.model}</td>
                  <td>{r.confidence.toFixed(2)}</td>
                  <td>{r.samples}</td>
                  <td>{r.meanScore.toFixed(2)}</td>
                  <td>{r.samples >= 3 && r.confidence >= 0.35 ? "yes" : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section>
        <h3>
          exam banks <span className="meta">closed-book sets on disk · clock strikes every 10 rounds</span>
        </h3>
        {!st || st.banks.length === 0 ? (
          <div className="placeholder">no classes</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>class</th>
                <th>on disk</th>
                <th>questions</th>
              </tr>
            </thead>
            <tbody>
              {st.banks.map((b) => (
                <tr key={b.class}>
                  <td>{b.class}</td>
                  <td>{b.seeded ? "seeded" : <span className="meta">not seeded</span>}</td>
                  <td>{b.seeded ? b.questions : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section>
        <h3>
          spaces <span className="meta">specialist knowledge &amp; skills — commons by consensus</span>
        </h3>
        {!st || st.spaces.length === 0 ? (
          <div className="placeholder">no spaces yet</div>
        ) : (
          st.spaces.map((sp) => {
            if (sp.knowledge.length === 0 && sp.skills.length === 0) return null;
            return (
              <div key={sp.agentId} className="mem-group">
                <div className="mem-group-head">
                  <span>{sp.agentId}</span>
                  <span className="mem-badge">
                    {sp.knowledge.length + sp.skills.length}
                  </span>
                </div>
                {[...sp.knowledge, ...sp.skills].map((it) => (
                  <div key={it.name} className="mem-row">
                    <div className="mem-main">
                      <div className="mem-name">
                        {it.name}
                        {it.endorsements > 0 && (
                          <span className="mem-hits">×{it.endorsements}</span>
                        )}
                      </div>
                      {it.description && <div className="mem-desc">{it.description}</div>}
                    </div>
                  </div>
                ))}
              </div>
            );
          })
        )}
      </section>

      <section>
        <h3>
          flows <span className="meta">frozen procedures — run/save stay TUI (/flow run &lt;slug&gt;)</span>
        </h3>
        {!st || st.flows.length === 0 ? (
          <div className="placeholder">none — /flow save freezes a session in the TUI</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>slug</th>
                <th>title</th>
                <th>nodes</th>
                <th>trigger</th>
                <th>source</th>
              </tr>
            </thead>
            <tbody>
              {st.flows.map((f) => (
                <tr key={f.slug}>
                  <td>{f.slug}</td>
                  <td>{f.title}</td>
                  <td>{f.nodes}</td>
                  <td className="run-prompt">
                    {f.trigger.length > 0 ? f.trigger.join(", ") : "—"}
                  </td>
                  <td>{f.source}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}
