# lyon-agent — convenience targets. go build/test remain the primary
# workflow; these wrap the demo entry points (ROADMAP 0.3).

.PHONY: demo-60s demo-60s-live

# 60 秒“从失败中学习”演示：脚本化跑完四个会话并打印 PASS/FAIL 清单。
demo-60s:
	bash demo/demo-60s.sh

# 同上，但在你自己打开的真实终端里驱动 TUI（拍 GIF / 现场演示用）。
demo-60s-live:
	bash demo/demo-60s.sh --live
