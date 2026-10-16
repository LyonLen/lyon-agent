import FilePanel from "./FilePanel";
import type { TodoItem } from "../todos";
import type { StepRow } from "../types";

// Plan progress for the resident header: how many done, and what the
// model is on right now (first in_progress item, TUI todoProgress).
export function todoProgress(items: TodoItem[]): {
  done: number;
  total: number;
  current: string;
} {
  let done = 0;
  let current = "";
  for (const it of items) {
    if (it.status === "completed") done++;
    if (it.status === "in_progress" && !current) current = it.content;
  }
  return { done, total: items.length, current };
}

function PlanCorner({ todos }: { todos: TodoItem[] }) {
  if (todos.length === 0) {
    return (
      <div className="plan-corner empty">
        <span className="plan-head">📋 plan · none yet</span>
      </div>
    );
  }
  const { done, total, current } = todoProgress(todos);
  return (
    <div className="plan-corner">
      <div className="plan-head">
        <span className="plan-scale">
          📋 plan {done}/{total}
        </span>
        {current && <span className="plan-current">▸ {current}</span>}
      </div>
      <ul className="plan-items">
        {todos.map((it, i) => (
          <li key={i} className={`plan-item ${it.status}`}>
            <span className="plan-glyph" aria-hidden="true">
              {it.status === "completed" ? "☑" : it.status === "in_progress" ? "◐" : "☐"}
            </span>
            {it.content}
          </li>
        ))}
      </ul>
    </div>
  );
}

function StepsPanel({ steps }: { steps: StepRow[] }) {
  if (steps.length === 0) {
    return (
      <div className="placeholder">tool executions appear here live</div>
    );
  }
  return (
    <div className="steps-list">
      {steps.map((s) => (
        <details
          key={s.execId.toString()}
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

// One clipped line of the freshest output — the "live output band" of the
// TUI right column, per step.
function liveTail(output: string): string {
  const t = output.trimEnd().split("\n").pop() ?? "";
  return t.length > 60 ? `… ${t.slice(-60)}` : t;
}

// SidePanel is the right column's new v2 shape: the resident plan corner
// on top (TUI #1 seat), then files/steps as tabs. Panel state lives in
// App so slash commands (/steps) can drive the tab. Both tabs stay
// mounted (portal precedent): hiding via CSS keeps the file browser's
// path and preview alive across tab flips.
export default function SidePanel(props: {
  todos: TodoItem[];
  steps: StepRow[];
  tab: "files" | "steps";
  onTab: (t: "files" | "steps") => void;
}) {
  return (
    <div className="col-files">
      <PlanCorner todos={props.todos} />
      <div className="files-head panel-tabs">
        <button
          className={"ghost tab" + (props.tab === "steps" ? " active" : "")}
          onClick={() => props.onTab("steps")}
        >
          steps
        </button>
        <button
          className={"ghost tab" + (props.tab === "files" ? " active" : "")}
          onClick={() => props.onTab("files")}
        >
          files
        </button>
      </div>
      <div className="panel-body">
        <div className="panel-slot" hidden={props.tab !== "files"}>
          <FilePanel />
        </div>
        <div className="panel-slot" hidden={props.tab !== "steps"}>
          <StepsPanel steps={props.steps} />
        </div>
      </div>
    </div>
  );
}
