#!/usr/bin/env bash
# Overnight self-evolution demo driver (see docs: 自进化 demo).
#
# What one round does: clone the pristine template into a scratch
# workspace, optionally inject a bug, run `lyon run` on a task with a
# pinned candidate model (early rounds) or no -model at all (later
# rounds — the routing network decides, design §5). Every finished run
# is blind-judged into the leaderboard + routing table; when the exam
# clock strikes ten the next round automatically carries `-exam <class>`.
#
# Serial by design: leaderboard/routing JSON has no cross-process lock,
# so exactly one lyon process writes LYON_HOME at a time.
#
# Usage: LYON_HOME=... bash demo/evolve-loop.sh   (driver builds/copies
# its own runner to .demo/lyon-runner.exe first — see build step below).
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEMO="$ROOT/.demo"
TPL="$ROOT/demo/template"
WS="$DEMO/evolve-ws"
RUNNER="$DEMO/lyon-runner.exe"
LOG="$DEMO/evolve-log.txt"
export LYON_HOME="$DEMO/evolve-home"
# Judge/exam/coverage ride the subagent tier; the gateway only serves
# mimo models, so pin the cheapest one.
export LYON_SUBAGENT_MODEL="${LYON_SUBAGENT_MODEL:-mimo-v2.5-pro}"

mkdir -p "$DEMO" "$LYON_HOME"

log() { echo "[$(date '+%H:%M:%S')] $*" >>"$LOG"; }

MODELS=(mimo-v2.6-flash mimo-v2.6-pro mimo-v2.5-pro)

# Task rotation: three injectable bugs plus feature/review/research/
# writing rounds, so fix/coding/review/research/writing all accumulate
# evidence in the routing table.
TASKS=(fix_sum fix_gcd coding_median review research writing fix_mean coding_clamp)

inject_sum_bug() { # Sum skips the last element (classic off-by-one)
  sed -i 's/for _, n := range nums {/for _, n := range nums[:len(nums)-1] {/' "$WS/calc/calc.go"
}
inject_gcd_bug() { # GCD returns 0 when the inputs are equal
  sed -i 's/for b != 0 {/if a == b { return 0 }\n\tfor b != 0 {/' "$WS/calc/calc.go"
}
inject_mean_bug() { # Mean forgets the empty-slice guard (divides by zero → NaN, but off-by-one length)
  sed -i 's|return float64(Sum(nums)) / float64(len(nums))|return float64(Sum(nums[:len(nums)-1])) / float64(len(nums))|' "$WS/calc/calc.go"
}

prompt_for() {
  case "$1" in
    fix_sum)        echo "修复 calc.Sum 的 bug：它少加了最后一个元素。只改 Sum，改完运行 go test ./... 确认全绿，最后用一句话总结根因。" ;;
    fix_gcd)        echo "修复 calc.GCD 的 bug：两个输入相等时错误返回 0。只改 GCD，改完运行 go test ./... 确认全绿，最后用一句话总结根因。" ;;
    fix_mean)       echo "修复 calc.Mean 的 bug：结果总比正确值小。只改 Mean，改完运行 go test ./... 确认全绿，最后用一句话总结根因。" ;;
    coding_median)  echo "在 calc 包实现函数 Median(nums []int) int：返回中位数（奇数取中间，偶数取中间两数平均向下取整；空切片返回 0）。在 calc_test.go 补测试，运行 go test ./... 确认通过，最后简述实现思路。" ;;
    coding_clamp)   echo "在 calc 包实现函数 Clamp(v, lo, hi int) int：把 v 收敛到 [lo, hi] 区间（lo > hi 时返回 lo）。在 calc_test.go 补测试，运行 go test ./... 确认通过，最后简述实现思路。" ;;
    review)         echo "审查 calc/calc.go 的全部代码（不要修改文件），列出你发现的 bug 风险与改进建议，按严重程度排序，每条给出理由。" ;;
    research)       echo "阅读本项目全部代码，解释 Sum 对空切片返回 0 的设计考虑，以及这个库还有哪些边界条件值得写进测试；只分析不改代码。" ;;
    writing)        echo "为这个库写一段中文 README 介绍：一句话定位、安装、三个函数的用法示例（各一行代码）。只输出 Markdown 文本，不改文件。" ;;
  esac
}

# Run until 06:30 the next morning (absolute deadline — a time-of-day
# window comparison would misfire on an 18:00 start).
deadline=$(date -d 'tomorrow 06:30' +%s)

round=0
while :; do
  round=$((round + 1))
  [ "$round" -gt 60 ] && { log "round cap 60 reached — stopping"; break; }
  if [ "$(date +%s)" -ge "$deadline" ]; then
    log "reached 06:30 — stopping after $((round - 1)) rounds"
    break
  fi

  task=${TASKS[$(((round - 1) % ${#TASKS[@]}))]}
  rm -rf "$WS"
  git clone -q "$TPL" "$WS" 2>>"$LOG" || { log "clone failed"; break; }

  exam_class=""
  case "$task" in
    fix_sum)    inject_sum_bug ;;
    fix_gcd)    inject_gcd_bug ;;
    fix_mean)   inject_mean_bug ;;
  esac

  # First 9 rounds pin candidates round-robin so every model earns
  # routing evidence; from round 10 on no -model is passed and the
  # routing table picks who works (⇄ routing lines in the log).
  model_args=()
  pinned="routing"
  if [ "$round" -le 9 ]; then
    pick=${MODELS[$(((round - 1) % ${#MODELS[@]}))]}
    model_args=(-model "$pick")
    pinned="$pick"
  fi

  # Every class hits its exam clock after 10 rounds; the previous run's
  # hint tells us which class is due — carry its exam into this round.
  if [ -f "$DEMO/round-$((round - 1)).out" ]; then
    due=$(grep -o 'lyon run -exam [a-z]*' "$DEMO/round-$((round - 1)).out" | tail -1 | awk '{print $4}')
    [ -n "$due" ] && exam_class="$due"
  fi

  exam_args=()
  [ -n "$exam_class" ] && exam_args=(-exam "$exam_class")

  prompt=$(prompt_for "$task")
  log "== round $round  task=$task  model=$pinned  exam=${exam_class:-none}"
  echo "===== round $round [$pinned] task=$task exam=${exam_class:-none} =====" >>"$LOG"
  "$RUNNER" run "${model_args[@]}" "${exam_args[@]}" \
    -workspace "$WS" -stats "$prompt" >"$DEMO/round-$round.out" 2>&1
  code=$?
  cat "$DEMO/round-$round.out" >>"$LOG"
  log "   round $round exit=$code"
  # Keep only the last few round outputs around.
  [ -f "$DEMO/round-$((round - 3)).out" ] && rm "$DEMO/round-$((round - 3)).out"
  sleep 15
done

log "loop finished: $((round - 1)) rounds"
