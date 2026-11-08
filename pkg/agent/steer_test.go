package agent

import "testing"

// TestClassifySteer: the mid-run input taxonomy end to end — stop is an
// emergency brake (whole-prompt imperatives only), redirect is an explicit
// switch, supplement defers, everything else folds. False stop/redirect
// kills half-done work, so ordinary questions and task statements must
// never land there.
func TestClassifySteer(t *testing.T) {
	cases := []struct {
		in   string
		want SteerIntent
	}{
		// stop: bare imperatives (with particles), never with content after.
		{"停", SteerStop},
		{"停一下", SteerStop},
		{"停下", SteerStop},
		{"停止！", SteerStop},
		{"别弄了", SteerStop},
		{"算了", SteerStop},
		{"stop", SteerStop},
		{"  停  ", SteerStop},
		// not stop: content rides along, or the stop word is embedded.
		{"停止条件是什么", SteerFold},
		{"帮我停止容器", SteerFold},
		{"停，先做这个", SteerRedirect},
		{"取消订单并重新下单", SteerFold},
		// redirect: explicit switches.
		{"换个方向", SteerRedirect},
		{"换方向，去做文档", SteerRedirect},
		{"改做单元测试", SteerRedirect},
		{"别做这个了，去写 README", SteerRedirect},
		{"先做这个", SteerRedirect},
		{"新任务：把日志清了", SteerRedirect},
		// supplement: additive, defer to after the current task.
		{"对了，把 README 也改一下", SteerSupplement},
		{"补充一个要求：输出用中文", SteerSupplement},
		{"别忘了加单测", SteerSupplement},
		{"顺便把 LICENSE 也更新", SteerSupplement},
		{"等下再帮我看看部署", SteerSupplement},
		{"之后记得提交", SteerSupplement},
		{"再加一条：兼容旧版", SteerSupplement},
		// fold: clarifications, corrections, nudges, chatter, plain asks.
		{"不就是这个会话嘛？", SteerFold},
		{"不对，我说的是昨天那个", SteerFold},
		{"为什么用这个方案？", SteerFold},
		{"怎么样了？", SteerFold},
		{"帮我把 README 翻译成英文", SteerFold},
		{"现在几点", SteerFold},
		{"", SteerFold},
	}
	for _, c := range cases {
		if got := ClassifySteer(c.in); got != c.want {
			t.Errorf("ClassifySteer(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
