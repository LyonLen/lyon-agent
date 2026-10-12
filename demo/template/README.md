# evodemo

A tiny Go arithmetic library (`calc`): Sum, GCD, Mean. Used as the
workspace for lyon's overnight self-evolution demo — each agent
candidate gets fresh tasks here (fix bugs, implement features, review,
explain, document) and every finished run is blind-judged into the
leaderboard.

```go
import "evodemo/calc"

total := calc.Sum([]int{1, 2, 3}) // 6
g := calc.GCD(12, 18)             // 6
```
