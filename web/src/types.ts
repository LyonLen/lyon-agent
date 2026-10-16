// StepRow is the right column's audit view of one tool execution,
// derived from the transcript's tool blocks (single source of truth —
// ChatView folds blocks and publishes the rows App renders).
export type StepRow = {
  execId: bigint;
  toolName: string;
  input: string;
  output: string;
  status: "pending" | "running" | "done" | "failed" | "server";
  durationMs: number;
  error: string;
  sub: boolean;
};
