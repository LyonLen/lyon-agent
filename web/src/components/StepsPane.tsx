import type { StepRow } from "../types";

// One clipped line of the freshest output — the "live output band" of the
// TUI right column, per step.
function liveTail(output: string): string {
  const t = output.trimEnd().split("\n").pop() ?? "";
  return t.length > 60 ? `… ${t.slice(-60)}` : t;
}

// StepsPane is the tool-execution audit list as its own workspace pane
// (extracted from SidePanel when the fixed right column dissolved).
export default function StepsPane({ steps }: { steps: StepRow[] }) {
  if (steps.length === 0) {
    return <div className="placeholder">tool executions appear here live</div>;
  }
  return (
    <div className="steps-list">
      {steps.map((s, i) => (
        <details
          // execId is 0 (null) for tool cards that haven't started an
          // exec yet — index in the key keeps those rows unique.
          key={`${i}:${s.execId.toString()}`}
          className={"step-row" + (s.status === "failed" ? " failed" : "")}
        >
          <summary className="step-head">
            <span className="step-title">
              {s.status === "running" && <span className="spin" aria-hidden="true" />}
              {s.status === "done" && <span className="step-ok">✓</span>}
              {s.status === "failed" && <span className="step-bad">✗</span>}
              <span className="tool-name">{s.toolName}</span>
              {s.sub && <span className="tool-badge">sub</span>}
            </span>
            <span className="step-meta">
              {s.status === "running"
                ? liveTail(s.output)
                : s.durationMs
                  ? `${s.durationMs}ms`
                  : ""}
            </span>
          </summary>
          {s.input && <pre className="tool-input">{s.input}</pre>}
          {s.output && <pre className="tool-output">{s.output}</pre>}
          {s.error && <div className="err">{s.error}</div>}
        </details>
      ))}
    </div>
  );
}
