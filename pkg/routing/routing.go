// Package routing is the entry-point recommendation network from
// docs/design/agent-evolution.md §5: tasks are classified into classes,
// each class remembers who handled it best (scores flow in from exams
// and flow runs), and the table grows into a routing network — "这类难题
// 交给谁" becomes a lookup, not a guess. It also grades how open-ended a
// task is, the signal a model uses to recommend someone else instead of
// forcing itself.
package routing

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is what the network knows about one task class: the recommended
// agent/model, a confidence born of samples and scores, and the rolling
// average that produced it.
type Entry struct {
	Class      string    `json:"class"`
	Agent      string    `json:"agent"`
	Model      string    `json:"model,omitempty"`
	Confidence float64   `json:"confidence"` // 0..1 — samples × mean score
	Samples    int       `json:"samples"`
	MeanScore  float64   `json:"mean_score"`
	Updated    time.Time `json:"updated"`
}

// Table is the file-backed network at $LYON_HOME/routing.json.
type Table struct {
	mu      sync.Mutex
	path    string
	Entries map[string]*Entry `json:"entries"`
}

// Open loads the table; missing or corrupt state starts empty (never
// fatal — a router with no memory still routes by classification).
func Open() *Table {
	path := filepath.Join(homeDir(), "routing.json")
	t := &Table{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, t)
	}
	if t.Entries == nil {
		t.Entries = map[string]*Entry{}
	}
	return t
}

// Lookup returns the cached recommendation for a class.
func (t *Table) Lookup(class string) (Entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.Entries[class]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Record folds one scored outcome into a class: running mean over all
// samples, confidence = mean × sample ramp (one lucky try stays unsure;
// twenty consistent tries become a fact).
func (t *Table) Record(class, agent, model string, score float64) Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.Entries[class]
	if !ok {
		e = &Entry{Class: class}
		t.Entries[class] = e
	}
	e.Samples++
	e.MeanScore = (e.MeanScore*float64(e.Samples-1) + score) / float64(e.Samples)
	if agent != "" {
		e.Agent = agent
	}
	if model != "" {
		e.Model = model
	}
	e.Confidence = round4(e.MeanScore * (float64(e.Samples) / float64(e.Samples+3)))
	e.Updated = time.Now().UTC()
	_ = t.saveLocked()
	return *e
}

// Save persists the table (Record already saves; exported for callers
// that mutate nothing else).
func (t *Table) Save() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saveLocked()
}

func (t *Table) saveLocked() error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// All returns a snapshot of the network (class → entry), classes sorted.
func (t *Table) All() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Entry, 0, len(t.Entries))
	for _, e := range t.Entries {
		out = append(out, *e)
	}
	// insertion order is map-random; sort by class for stable display
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Class < out[j-1].Class; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// Class is the task taxonomy the network routes over.
type Class string

const (
	ClassCoding   Class = "coding"   // implement, build, feature work
	ClassFix      Class = "fix"      // bugs, errors, regressions
	ClassReview   Class = "review"   // audits, reads, assessments
	ClassResearch Class = "research" // compare, investigate, learn
	ClassWriting  Class = "writing"  // prose, docs, translation
	ClassDesign   Class = "design"   // architecture, plans, trade-offs
	ClassData     Class = "data"     // parse, transform, statistics
	ClassGeneral  Class = "general"  // nothing matched
)

// classRules are bilingual keyword hits — Chinese and English tasks both
// land. Classify counts distinct hits per rule; on an exact tie the rule
// listed earlier wins, so order runs specific → general: actions that
// change code (fix, review) before the writing/data tails, with coding
// ahead of writing because "write tests" is coding work.
var classRules = []struct {
	class Class
	keys  []string
}{
	{ClassFix, []string{"fix", "bug", "error", "broken", "crash", "panic", "failing", "regression",
		"修复", "报错", "崩溃", "坏了", "失败", "异常", "排查"}},
	{ClassReview, []string{"review", "audit", "inspect", "critique", "evaluate",
		"评审", "审查", "审阅", "检查", "评估", "把关"}},
	{ClassResearch, []string{"research", "compare", "investigate", "explore options", "why does",
		"调研", "对比", "研究", "了解下", "为什么", "原理", "解释", "分析"}},
	{ClassDesign, []string{"design", "architect", "refactor plan", "trade-off", "schema design",
		"设计", "架构", "方案", "重构规划", "权衡"}},
	{ClassCoding, []string{"implement", "add", "build", "code", "function", "module", "test",
		"feature", "refactor", "optimize",
		"实现", "开发", "编写", "加一个", "做个", "功能", "重构", "优化", "测试"}},
	{ClassWriting, []string{"write", "draft", "translate", "blog", "email", "changelog entry",
		"写作", "写一篇", "翻译", "文档写", "邮件", "文章"}},
	{ClassData, []string{"parse", "transform", "csv", "json data", "statistics", "etl",
		"解析", "清洗", "统计", "报表", "数据处理"}},
}

// strongCoding marks verbs of construction — a task that says
// "implement / refactor / 实现…" is coding work whatever else it
// mentions, so the coding rule gets one bonus point over topical hits:
// "implement the parser for CSV" is coding (action wins), a bare
// "parse the csv" stays data (topic only).
var strongCoding = []string{
	"implement", "build", "refactor", "optimize", "add",
	"实现", "开发", "编写", "重构", "优化", "加一个", "做个",
}

// Classify buckets free text into a routing class (§5.2's "任务类").
// Rule hits are counted: the class with the most distinct keyword hits
// wins (coding earns a bonus when a construction verb is present), and
// an exact tie keeps the rule listed earlier (specific classes come
// before general). No hit → general.
func Classify(text string) Class {
	if text == "" {
		return ClassGeneral
	}
	lower := strings.ToLower(text)
	best, bestHits := ClassGeneral, 0
	for _, r := range classRules {
		hits := 0
		for _, k := range r.keys {
			if strings.Contains(lower, strings.ToLower(k)) {
				hits++
			}
		}
		if r.class == ClassCoding && hits > 0 && hasAny(lower, strongCoding) {
			hits++
		}
		if hits > bestHits {
			best, bestHits = r.class, hits
		}
	}
	return best
}

func hasAny(s string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// openWords pull a task toward open-ended (design, trade-offs, creation);
// closedWords pull toward mechanical (known procedure, exact answer).
var openWords = []string{
	"design", "architect", "brainstorm", "strategy", "trade-off", "tradeoff",
	"creative", "should we", "pros and cons", "migrate",
	"设计", "架构", "头脑风暴", "策略", "权衡", "创意", "要不要", "利弊", "迁移", "方案选型",
}
var closedWords = []string{
	"run", "execute", "format", "rename", "list files", "grep", "convert",
	"print", "calculate", "decode", "unit test",
	"跑一下", "执行", "格式化", "改名", "列一下", "算一下", "转换", "打印", "解码", "单测",
}

// Openness grades how open-ended a task is, 0 (mechanical, procedure
// known) to 5 (design/creation with many right answers). This is the
// self-knowledge signal of §5.1: when the task's openness exceeds the
// current model's Openness profile, recommending a stronger candidate is
// the right move instead of forcing it.
func Openness(text string) int {
	lower := strings.ToLower(text)
	open, closed := 0, 0
	for _, w := range openWords {
		if strings.Contains(lower, strings.ToLower(w)) {
			open++
		}
	}
	for _, w := range closedWords {
		if strings.Contains(lower, strings.ToLower(w)) {
			closed++
		}
	}
	switch {
	case open == 0 && closed == 0:
		return 2 // no signal — lean slightly open, "general conversation"
	case open > closed:
		return min(5, 2+open)
	case closed > open:
		return max(0, 2-closed)
	default:
		return 2
	}
}

func homeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lyon"
	}
	return filepath.Join(home, ".lyon")
}
