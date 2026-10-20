package agent

import (
	"context"
	"strings"

	"lyon-agent/pkg/logx"
	"lyon-agent/pkg/tools"
)

// shellSerialJudge is the session's escalation channel for the shell
// concurrency screen (#33). The keyword screen in pkg/tools already decided
// the command is SUSPICIOUS; this asks the small model whether it actually
// writes the workspace. "serial" (or anything we cannot parse) keeps the
// exclusive lock; "parallel" lets the command overlap other executions.
//
// Cost model: the screen hits only on commands that mention a mutating word
// or redirect output, so the common `ls`/`cat`/`git status` never reaches
// this function and costs zero tokens. The reply is one word, so the call
// is a few dozen tokens.
//
// It runs on the tool goroutine, possibly concurrently with other tool
// executions; Session.Ask is stateless and never touches s.mu, so that is
// safe.
func (s *Session) shellSerialJudge(ctx context.Context, tool, command string) bool {
	verdict, err := s.Ask(ctx,
		"You classify shell commands for a coding agent. Answer with exactly one word: "+
			"serial if the command may CREATE, MODIFY or DELETE files in the current workspace "+
			"(including via redirection, package installs, VCS writes or in-place edits); "+
			"parallel if it only reads. Answer serial when unsure.",
		"command:\n"+command,
		8,
	)
	if err != nil {
		// Fail closed: a judge we cannot reach must not widen concurrency.
		logx.Warn("tools", "shell serial judge failed; taking exclusive lock",
			"tool", tool, "err", err)
		return true
	}
	v := strings.ToLower(strings.TrimSpace(verdict))
	parallel := strings.Contains(v, "parallel") && !strings.Contains(v, "serial")
	if !parallel && !strings.Contains(v, "serial") {
		logx.Warn("tools", "shell serial judge unparseable; taking exclusive lock",
			"tool", tool, "reply", verdict)
		return true
	}
	logx.Debug("tools", "shell serial judgement", "tool", tool, "parallel", parallel)
	return !parallel
}

// withShellJudge wires the session judge onto the context tool executions
// inherit. Split out so newSession stays readable and so tests can build a
// context without a full Engine.
func (s *Session) withShellJudge(ctx context.Context) context.Context {
	return tools.WithSerialJudge(ctx, s.shellSerialJudge)
}
