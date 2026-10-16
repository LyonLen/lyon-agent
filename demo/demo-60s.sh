#!/usr/bin/env bash
# demo-60s.sh — ROADMAP 0.3: the 60-second learn-from-failure demo.
#
# Drives the REAL `lyon chat` TUI (the diagnosis writeback lives only in
# its bubbletea loop) inside a ConPTY helper, against demo/mockserve —
# a supervised mock gateway whose fake model makes the same mistake
# three times before the saved lesson brings it back on track:
#
#   镜头 1  失败      prompt → model reads the wrong file → injected 500
#                     → prompt_end(error)                (TUI shows the error)
#   镜头 2  错因归纳  kickDiagnose → Diagnosis{failed_conditions, patch}
#                     → audit "diagnosis" event
#   镜头 3  写入记忆  rememberDiagnosis → project-scope hot fact
#                     failed-conditions-<fnv64a> in <workspace>/.lyon/memory
#   镜头 4  注入教训  next session's FIRST request body carries the fact
#                     (asserted via mockserve /__demo/last_body)
#   镜头 5  不再犯    model reads notes.txt, answers 16160, no error
#
# Usage:
#   bash demo/demo-60s.sh          run the scripted demo, PASS/FAIL list
#   bash demo/demo-60s.sh --live   set everything up, walk a human through
#                                  the four sessions in their own terminal
#                                  (for filming; mock resets per shot)
#
# Exit code: 0 only when every assertion passed (and, scripted mode, the
# whole run stayed inside the 90s grace budget). Artifacts stay in .demo/
# for inspection: demo-60s.log, demo-60s/{ws,home,transcripts,steps}...
#
# Windows/MinGW notes: no `cp` (not reliably present), paths are
# forward-slashed, taskkill uses //PID with the exact pid the demo itself
# started (never a name-wide kill), exe suffixes are explicit.

set -u

# pwd -W (MSYS): Windows form of the repo root. Plain POSIX "/d/..." would
# reach lyon as a drive-relative path — filepath.Abs mangles it to
# D:\d\... — so every path handed to Windows processes must be native.
ROOT="$(cd "$(dirname "$0")/.." && pwd -W 2>/dev/null || pwd)"
DEMO="$ROOT/.demo"
ART="$DEMO/demo-60s"
BIN="$ROOT/tmp/demo60"
LOG="$DEMO/demo-60s.log"
GRACE_SECS=90

PROMPT='读取 notes.txt 里的 port 数值，翻倍后告诉我。'
ANSWER='16160'
LESSON_TAG='demo60s-lesson'

case "$(uname -s)" in
MINGW*|MSYS*) ;;
*)
    echo "demo-60s: the scripted run needs Windows ConPTY (tuipty) under MSYS/Git Bash;" \
         "on this platform use --live from a real console, or drive the TUI by hand."
    exit 2
    ;;
esac
# Refuse to continue with a POSIX-style root: everything downstream (and
# lyon's -workspace) would resolve to a D:\d\... drive-relative monster.
case "$ROOT" in
[A-Za-z]:/*) ;;
*)
    echo "demo-60s: cannot resolve a Windows-style repo root (got '$ROOT')"
    exit 2
    ;;
esac

# ---------------------------------------------------------------- logging
RESULTS=()      # one "PASS|FAIL <shot>: <what>" per assertion
FAILS=0
log() { printf '%s %s\n' "$(date '+%H:%M:%S')" "$*" >> "$LOG"; }
shot_result() { # shot_result PASS|FAIL "text"
    RESULTS+=("$1 $2")
    [ "$1" = FAIL ] && FAILS=$((FAILS + 1))
    log "[$1] $2"
    printf '[%s] %s\n' "$1" "$2"
}

MOCK_PID=""
TUIPTY_PID=""
cleanup() {
    # Reap only processes this run started, by exact pid. Files stay.
    if [ -n "$TUIPTY_PID" ]; then
        taskkill //PID "$TUIPTY_PID" //T //F >/dev/null 2>&1
    fi
    if [ -n "$MOCK_PID" ]; then
        taskkill //PID "$MOCK_PID" //F >/dev/null 2>&1
    fi
}
trap cleanup EXIT

START_SECS=$(date +%s)

# ---------------------------------------------------------------- setup
rm -rf "$ART"
mkdir -p "$ART/ws" "$ART/home" "$ART/empty-skills" "$ART/transcripts" "$ART/steps" "$BIN"
: > "$LOG"
log "demo-60s start (root=$ROOT)"

WS="$ART/ws"
HOME_DIR="$ART/home"
AUDIT="$HOME_DIR/audit/$(date +%Y-%m-%d).jsonl"

# The workspace the four sessions share: the wrong file the fake model
# keeps reading, and the right one the lesson teaches it to read.
printf 'port=8080\n' > "$WS/notes.txt"
printf 'port=4242\n' > "$WS/config.txt"

command -v go >/dev/null 2>&1 || { echo "demo-60s: go not found in PATH"; exit 2; }
cd "$ROOT" || exit 2

# Two attempts per binary: a nearly-full disk plus AV scans occasionally
# make a GOCACHE write vanish between "cached" and "read back" — the very
# symptom that has no relation to the code being built.
build_one() { # build_one <out> <pkg>
    local out="$1" pkg="$2" i
    for i in 1 2; do
        go build -o "$out" "$pkg" && return 0
        log "build $pkg failed (attempt $i)"
        sleep 1
    done
    return 1
}

log "building tuipty / mockserve / lyon-demo"
build_one "$BIN/tuipty.exe"    ./demo/tuipty    || { echo "demo-60s: build tuipty failed";    exit 2; }
build_one "$BIN/mockserve.exe" ./demo/mockserve || { echo "demo-60s: build mockserve failed"; exit 2; }
build_one "$BIN/lyon-demo.exe" ./cmd/lyon       || { echo "demo-60s: build lyon failed";     exit 2; }
log "builds ok"

# ------------------------------------------------------- mock gateway
MS_OUT="$ART/mockserve.out"
"$BIN/mockserve.exe" -addr 127.0.0.1:0 -latency 15ms > "$MS_OUT" 2>&1 &
for _ in $(seq 1 40); do
    [ -s "$MS_OUT" ] && break
    sleep 0.25
done
MOCK_URL=$(sed -n 's/^MOCKSERVE \(http:\/\/[^ ]*\) pid=\([0-9]*\)$/\1/p' "$MS_OUT" | head -1)
MOCK_PID=$(sed -n 's/^MOCKSERVE \(http:\/\/[^ ]*\) pid=\([0-9]*\)$/\2/p' "$MS_OUT" | head -1)
if [ -z "$MOCK_URL" ] || [ -z "$MOCK_PID" ]; then
    echo "demo-60s: mockserve did not come up; output:"
    cat "$MS_OUT"
    exit 2
fi
log "mockserve up: $MOCK_URL pid=$MOCK_PID"

mock_reset() { # mock_reset <payload-file>
    curl -sf -X POST --data-binary @"$1" "$MOCK_URL/__demo/reset" >/dev/null || return 1
}

# ------------------------------------------------- environment (hermetic)
# Scratch LYON_HOME (audit + config live here, never the user's real
# ~/.lyon); the shared workspace carries the memory store. Every
# side-channel Ask is gated off so the only model calls are the task and
# the diagnosis; skills point at an empty dir.
export ANTHROPIC_BASE_URL="$MOCK_URL"
export ANTHROPIC_AUTH_TOKEN=demo-token
export ANTHROPIC_API_KEY=demo-key
export ANTHROPIC_MODEL=demo-mock-model
export LYON_HOME="$HOME_DIR"
export LYON_LANG=zh
export LYON_TITLE=off
export LYON_SCORE=off
export LYON_DISTILL=off
export LYON_EXAM=off
export LYON_NOTIFY=off
export LYON_AUTOFREEZE=off
export LYON_FLOW_GATE=off
export LYON_SKILLS_DIR="$ART/empty-skills"
export LYON_MAX_ITER=8
unset LYON_OPENAI_BASE 2>/dev/null || true

LYON_CMD="$BIN/lyon-demo.exe chat -workspace $WS -approve=false"

# ----------------------------------------------------------- sessions
reset_mistake() { # the scripted model: reads config.txt, then the
                  # injected 500 repeats its mistake; the diagnosis reply
                  # carries the conditions + the $LESSON_TAG patch.
    cat > "$ART/reset-mistake.json" <<EOF
{"task":[{"tool":{"name":"read_file","input":"{\"path\":\"config.txt\"}"}}],"diag":[{"text":["{\"failed_conditions\":[\"必须用 read_file 读取 notes.txt 而不是 config.txt\",\"端口数值必须先翻倍再回答\"],\"patch\":\"下次先读 notes.txt，把 port 翻倍后作答（$LESSON_TAG）\"}"]}],"fail_at":2}
EOF
    mock_reset "$ART/reset-mistake.json"
}

reset_correct() { # the reformed model: reads notes.txt, doubles 8080.
    cat > "$ART/reset-correct.json" <<EOF
{"task":[{"tool":{"name":"read_file","input":"{\"path\":\"notes.txt\"}"}},{"text":["端口翻倍后是 $ANSWER。"]}],"diag":[],"fail_at":0}
EOF
    mock_reset "$ART/reset-correct.json"
}

run_session() { # run_session <name> <steps-file>  → 0 iff tuipty exited 0
    local name="$1" steps="$2"
    "$BIN/tuipty.exe" \
        -cmd "$LYON_CMD" \
        -cwd "$WS" \
        -steps "$steps" \
        -transcript "$ART/transcripts/$name.txt" \
        -timeout 60s >> "$LOG" 2>&1 &
    TUIPTY_PID=$!
    wait "$TUIPTY_PID"
    local rc=$?
    TUIPTY_PID=""
    return $rc
}

steps_mistake() { # steps_mistake <file> <done-wait substr>
    cat > "$1" <<EOF
sleep 700
send $PROMPT
wait-audit $2 30000
send /quit
wait-exit 10000
EOF
}

steps_correct() {
    cat > "$1" <<EOF
sleep 700
send $PROMPT
wait-audit prompt_end 30000
send /quit
wait-exit 10000
EOF
}

# Audit records marshal compact (encoding/json): "event":"x", no spaces.
audit_count() { grep -cF "\"event\":\"$1\"" "$AUDIT" 2>/dev/null || true; }
audit_has()   { grep -qF "$1" "$AUDIT" 2>/dev/null; }
last_body()  { curl -sf "$MOCK_URL/__demo/last_body"; }

# ================================================== scripted run
if [ "${1:-}" != "--live" ]; then

    # ---- 镜头 1..3: three identical failed attempts -------------------
    for n in 1 2 3; do
        case $n in
        1) waitsub=memory_save ;;   # shot 1 goes all the way to the write
        *) waitsub=diagnosis ;;     # shots 2-3: a repeat lesson dedups
        esac
        reset_mistake || { shot_result FAIL "S$n mock reset"; continue; }
        steps_mistake "$ART/steps/s$n.txt" "$waitsub"
        if run_session "s$n" "$ART/steps/s$n.txt"; then
            shot_result PASS "S$n TUI 会话跑完并干净退出"
        else
            shot_result FAIL "S$n tuipty 退出码非 0（见 $LOG）"
        fi

        case $n in
        1)
            audit_has "$PROMPT" \
                && shot_result PASS "镜头1 失败前的 CJK prompt 完整进入会话 (prompt_start)" \
                || shot_result FAIL "镜头1 audit 里找不到 prompt 文本"
            grep -qF 'injected failure' "$AUDIT" \
                && shot_result PASS "镜头1 注入 500 → prompt_end(error) 落账" \
                || shot_result FAIL "镜头1 没有带 error 的 prompt_end"
            audit_has "$LESSON_TAG" \
                && shot_result PASS "镜头2 错因归纳：diagnosis 带 patch($LESSON_TAG)" \
                || shot_result FAIL "镜头2 diagnosis 事件缺失或无 lesson patch"
            [ "$(audit_count memory_save)" -ge 1 ] && audit_has failed-conditions- \
                && shot_result PASS "镜头3 记忆写入：failed-conditions-<hash> 项目级 fact" \
                || shot_result FAIL "镜头3 memory_save 事件缺失"
            ;;
        2)
            [ "$(audit_count diagnosis)" -eq 2 ] && [ "$(audit_count memory_save)" -eq 1 ] \
                && shot_result PASS "镜头2/3 第二次同类失败：再次归纳，记忆去重不重写" \
                || shot_result FAIL "S2 计数不对: diagnosis=$(audit_count diagnosis) memory_save=$(audit_count memory_save)"
            ;;
        3)
            [ "$(audit_count diagnosis)" -eq 3 ] && [ "$(audit_count memory_save)" -eq 1 ] \
                && shot_result PASS "镜头2/3 第三次同类失败：归纳三次，记忆仍只有一条" \
                || shot_result FAIL "S3 计数不对: diagnosis=$(audit_count diagnosis) memory_save=$(audit_count memory_save)"
            ;;
        esac
    done

    # ---- 镜头 4..5: the next session carries the lesson ---------------
    reset_correct || shot_result FAIL "S4 mock reset"
    steps_correct "$ART/steps/s4.txt"
    if run_session "s4" "$ART/steps/s4.txt"; then
        shot_result PASS "S4（新会话）TUI 跑完并干净退出"
    else
        shot_result FAIL "S4 tuipty 退出码非 0（见 $LOG）"
    fi

    LB=$(last_body)
    echo "$LB" > "$ART/last_body_s4.json"
    printf '%s' "$LB" | grep -qF "$LESSON_TAG" \
        && shot_result PASS "镜头4 下一次会话的请求体注入了教训（last_body 含 $LESSON_TAG）" \
        || shot_result FAIL "镜头4 last_body 里没有 $LESSON_TAG —— 记忆未注入"
    printf '%s' "$LB" | grep -qF '"path":"notes.txt"' \
        && shot_result PASS "镜头5 这次读对了文件（tool_use read_file notes.txt）" \
        || shot_result FAIL "镜头5 没有对 notes.txt 的 read_file"
    grep -qF "$ANSWER" "$ART/transcripts/s4.txt" \
        && shot_result PASS "镜头5 正确答案 $ANSWER 上屏（TUI transcript）" \
        || shot_result FAIL "镜头5 transcript 里找不到 $ANSWER"
    if command -v python >/dev/null 2>&1; then
        python -X utf8 - "$AUDIT" <<'PYEOF' && shot_result PASS "镜头5 成功收尾：末次 prompt_end 无 error，且不再新增归纳" || shot_result FAIL "镜头5 末次 prompt_end 带 error 或又触发了 diagnosis"
import json, sys
lines = [json.loads(l) for l in open(sys.argv[1], encoding="utf-8") if l.strip()]
pes = [r for r in lines if r.get("event") == "prompt_end"]
assert pes and not (pes[-1].get("error") or ""), "last prompt_end carries an error"
assert sum(1 for r in lines if r.get("event") == "diagnosis") == 3, "diagnosis count changed in S4"
PYEOF
    else
        shot_result FAIL "python 不可用，无法断言末次 prompt_end"
    fi

# ================================================== live run (filming)
else
    echo "== demo-60s --live: 拍摄模式 =="
    echo "每一步在下面提示后，把你终端里的 TUI 操作完，再回车进入下一镜头。"
    echo "准备命令（复制到你自己的真实终端里执行）："
    echo
    echo "  export ANTHROPIC_BASE_URL=$MOCK_URL ANTHROPIC_AUTH_TOKEN=demo-token \\"
    echo "         ANTHROPIC_MODEL=demo-mock-model LYON_HOME='$HOME_DIR' LYON_LANG=zh \\"
    echo "         LYON_TITLE=off LYON_SCORE=off LYON_DISTILL=off LYON_EXAM=off \\"
    echo "         LYON_NOTIFY=off LYON_AUTOFREEZE=off LYON_FLOW_GATE=off \\"
    echo "         LYON_SKILLS_DIR='$ART/empty-skills' LYON_MAX_ITER=8"
    echo "  $LYON_CMD"
    echo
    for n in 1 2 3; do
        case $n in
        1) waitsub=memory_save ;;
        *) waitsub=diagnosis ;;
        esac
        reset_mistake
        steps_mistake "$ART/steps/s$n.txt" "$waitsub"
        echo "---- 镜头 $n/4（失败→归纳→写入记忆，重复第 $n 次）----"
        echo "在 TUI 里输入并回车：$PROMPT"
        echo "预期：报错（fake model repeated its mistake）后自动归纳，状态行出现诊断；"
        echo "      会后 \$LYON_HOME/audit 里落 diagnosis / memory_save 事件。"
        [ "$n" = 1 ] || echo "      （这是第 $n 次同类失败：再次归纳，但记忆去重、只保留一条）"
        printf '操作完（含 /quit 退出 TUI）后回车继续…'; read -r _
    done
    reset_correct
    steps_correct "$ART/steps/s4.txt"
    echo "---- 镜头 4/4（注入教训 → 不再犯）----"
    echo "在 TUI 里输入并回车：$PROMPT"
    echo "预期：agent 这次读 notes.txt，回答「端口翻倍后是 $ANSWER。」，无报错。"
    echo "      （请求体里带着上一镜头写入的 $LESSON_TAG 教训 —— /__demo/last_body 可验证）"
    printf '操作完（含 /quit）后回车结束…'; read -r _
    echo "拍完了。GIF 素材见 demo/README-60s.md 的 ffmpeg 说明；产物在 $ART"
fi

# ================================================== summary
ELAPSED=$(( $(date +%s) - START_SECS ))
echo
echo "================ demo-60s 结果 ================"
if [ "${#RESULTS[@]}" -gt 0 ]; then
    for r in "${RESULTS[@]}"; do echo "  $r"; done
    echo "----------------------------------------------"
fi
echo "耗时: ${ELAPSED}s (上限 ${GRACE_SECS}s)   产物: $ART   日志: $LOG"

if [ "${1:-}" = "--live" ]; then
    echo "拍摄模式结束 —— 产物与日志已就位（GIF 转换见 demo/README-60s.md）"
    exit 0
fi
if [ "$FAILS" -gt 0 ]; then
    echo "FAIL: $FAILS 条断言未通过"
    exit 1
fi
if [ "$ELAPSED" -ge "$GRACE_SECS" ]; then
    echo "FAIL: 超时（${ELAPSED}s >= ${GRACE_SECS}s）"
    exit 1
fi
echo "PASS: 全部断言通过"
exit 0
